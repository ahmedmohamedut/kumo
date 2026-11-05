//go:build libvirt

package dbinstance_test

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// A Postgres client, in as much of the wire protocol as it takes to prove two
// things about a DB instance Kumo created:
//
//   - the **original cleartext password** authenticates against the role that
//     was created from the SCRAM verifier Kumo pre-computed and discarded
//     (07-databases.md §5), and the server's own signature checks out, so both
//     sides derived from the same secret;
//   - a client whose address is outside the VPC is refused by `pg_hba.conf`,
//     which is the whole of R6.
//
// It is here rather than as a dependency because `psql` is not on every node
// and a Postgres driver is not something to add to the binary for a test
// (AGENTS.md: ask before adding a dependency). The exchange is RFC 5802 SCRAM
// over the Postgres message framing, and nothing more: no query is sent, and
// the connection is closed as soon as the server says it is ready.

// Postgres backend message types used here.
const (
	msgAuthentication = 'R'
	msgErrorResponse  = 'E'
	msgReadyForQuery  = 'Z'
)

// Authentication request subtypes.
const (
	authOK           = 0
	authSASL         = 10
	authSASLContinue = 11
	authSASLFinal    = 12
)

// pgConn is one connection being driven through the handshake.
type pgConn struct {
	conn net.Conn
}

// pgError is an ErrorResponse the server sent, with the fields that say why.
type pgError struct {
	Severity string
	Code     string
	Message  string
}

func (e *pgError) Error() string {
	return fmt.Sprintf("%s %s: %s", e.Severity, e.Code, e.Message)
}

// pgDial opens a connection, optionally from a chosen local address, which is
// how the test presents itself as a client from outside the VPC.
func pgDial(address string, localAddr net.Addr, timeout time.Duration) (*pgConn, error) {
	dialer := net.Dialer{Timeout: timeout, LocalAddr: localAddr}
	conn, err := dialer.Dial("tcp", address)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &pgConn{conn: conn}, nil
}

func (c *pgConn) Close() {
	_ = c.conn.Close()
}

// pgLogin runs the startup and SCRAM-SHA-256 exchange and returns nil when the
// server reports the session ready.
//
// A refusal comes back as a *pgError, so a caller can tell a wrong password
// (28P01) from a host the server does not admit (28000, "no pg_hba.conf
// entry") -- which are the two negative cases worth distinguishing here.
func (c *pgConn) Login(user, database, password string) error {
	if err := c.startup(user, database); err != nil {
		return err
	}

	mechanisms, err := c.expectSASL()
	if err != nil {
		return err
	}
	const mechanism = "SCRAM-SHA-256"
	if !contains(mechanisms, mechanism) {
		return fmt.Errorf("the server offered %v, not %s; password_encryption is not scram-sha-256",
			mechanisms, mechanism)
	}

	nonce, err := clientNonce()
	if err != nil {
		return err
	}
	clientFirstBare := "n=,r=" + nonce
	if err := c.sendSASLInitial(mechanism, "n,,"+clientFirstBare); err != nil {
		return err
	}

	serverFirst, err := c.expectSASLContinue()
	if err != nil {
		return err
	}
	salt, iterations, serverNonce, err := parseServerFirst(serverFirst, nonce)
	if err != nil {
		return err
	}

	// The derivation of 07-databases.md §5, from the client's side this time:
	// the same SaltedPassword the verifier was built from, or the proof below
	// does not check out.
	saltedPassword, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return err
	}
	clientKey := mac(saltedPassword, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := mac(saltedPassword, "Server Key")

	clientFinalBare := "c=biws,r=" + serverNonce
	authMessage := clientFirstBare + "," + serverFirst + "," + clientFinalBare
	clientSignature := mac(storedKey[:], authMessage)

	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ clientSignature[i]
	}
	clientFinal := clientFinalBare + ",p=" + base64.StdEncoding.EncodeToString(proof)
	if err := c.sendSASLResponse(clientFinal); err != nil {
		return err
	}

	final, err := c.expectSASLFinal()
	if err != nil {
		return err
	}
	// The server proves it holds the same secret, which is what makes a
	// successful login evidence about the verifier rather than about the
	// server being willing to say yes.
	wantSignature := base64.StdEncoding.EncodeToString(mac(serverKey, authMessage))
	if got := strings.TrimPrefix(final, "v="); got != wantSignature {
		return fmt.Errorf("the server signature does not check out: got %q", got)
	}

	return c.expectReady()
}

// startup sends the StartupMessage: protocol 3.0, a user and a database.
func (c *pgConn) startup(user, database string) error {
	var payload []byte
	payload = binary.BigEndian.AppendUint32(payload, 196608) // 3.0
	for _, pair := range [][2]string{{"user", user}, {"database", database}} {
		payload = append(payload, pair[0]...)
		payload = append(payload, 0)
		payload = append(payload, pair[1]...)
		payload = append(payload, 0)
	}
	payload = append(payload, 0)

	frame := binary.BigEndian.AppendUint32(nil, uint32(len(payload)+4))
	_, err := c.conn.Write(append(frame, payload...))
	return err
}

func (c *pgConn) sendSASLInitial(mechanism, response string) error {
	var payload []byte
	payload = append(payload, mechanism...)
	payload = append(payload, 0)
	payload = binary.BigEndian.AppendUint32(payload, uint32(len(response)))
	payload = append(payload, response...)
	return c.send('p', payload)
}

func (c *pgConn) sendSASLResponse(response string) error {
	return c.send('p', []byte(response))
}

func (c *pgConn) send(kind byte, payload []byte) error {
	frame := []byte{kind}
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(payload)+4))
	_, err := c.conn.Write(append(frame, payload...))
	return err
}

// receive reads one backend message, turning an ErrorResponse into *pgError.
func (c *pgConn) receive() (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(c.conn, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size < 4 || size > 1<<20 {
		return 0, nil, fmt.Errorf("the server sent a %d byte message", size)
	}
	payload := make([]byte, size-4)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return 0, nil, err
	}
	if header[0] == msgErrorResponse {
		return header[0], payload, parseError(payload)
	}
	return header[0], payload, nil
}

func (c *pgConn) expectSASL() ([]string, error) {
	kind, payload, err := c.receive()
	if err != nil {
		return nil, err
	}
	if kind != msgAuthentication {
		return nil, fmt.Errorf("the server answered %q, want an authentication request", kind)
	}
	if subtype := binary.BigEndian.Uint32(payload); subtype != authSASL {
		return nil, fmt.Errorf("the server asked for authentication %d, want SASL", subtype)
	}

	var mechanisms []string
	for _, name := range strings.Split(string(payload[4:]), "\x00") {
		if name != "" {
			mechanisms = append(mechanisms, name)
		}
	}
	return mechanisms, nil
}

func (c *pgConn) expectSASLContinue() (string, error) {
	return c.expectAuthData(authSASLContinue)
}

func (c *pgConn) expectSASLFinal() (string, error) {
	return c.expectAuthData(authSASLFinal)
}

func (c *pgConn) expectAuthData(want uint32) (string, error) {
	kind, payload, err := c.receive()
	if err != nil {
		return "", err
	}
	if kind != msgAuthentication {
		return "", fmt.Errorf("the server answered %q, want an authentication message", kind)
	}
	if subtype := binary.BigEndian.Uint32(payload); subtype != want {
		return "", fmt.Errorf("the server sent authentication %d, want %d", subtype, want)
	}
	return string(payload[4:]), nil
}

// expectReady drains the messages that follow a successful authentication
// until ReadyForQuery, which is the server saying the session is usable.
func (c *pgConn) expectReady() error {
	kind, payload, err := c.receive()
	if err != nil {
		return err
	}
	if kind != msgAuthentication || binary.BigEndian.Uint32(payload) != authOK {
		return fmt.Errorf("the server answered %q after the proof, want AuthenticationOk", kind)
	}

	for {
		kind, _, err := c.receive()
		if err != nil {
			return err
		}
		if kind == msgReadyForQuery {
			return nil
		}
	}
}

// parseError reads the fields of an ErrorResponse.
func parseError(payload []byte) error {
	out := &pgError{}
	for _, field := range strings.Split(string(payload), "\x00") {
		if field == "" {
			continue
		}
		switch field[0] {
		case 'S':
			out.Severity = field[1:]
		case 'C':
			out.Code = field[1:]
		case 'M':
			out.Message = field[1:]
		}
	}
	return out
}

// parseServerFirst reads the salt, the iteration count and the combined nonce
// out of the server's first SCRAM message.
func parseServerFirst(message, clientNonce string) (salt []byte, iterations int, nonce string, err error) {
	for _, part := range strings.Split(message, ",") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch key {
		case "r":
			nonce = value
		case "s":
			salt, err = base64.StdEncoding.DecodeString(value)
			if err != nil {
				return nil, 0, "", fmt.Errorf("the server salt is not base64: %w", err)
			}
		case "i":
			iterations, err = strconv.Atoi(value)
			if err != nil {
				return nil, 0, "", fmt.Errorf("the server iteration count is not a number: %w", err)
			}
		}
	}
	switch {
	case nonce == "" || !strings.HasPrefix(nonce, clientNonce):
		return nil, 0, "", errors.New("the server nonce does not extend the client's")
	case len(salt) == 0:
		return nil, 0, "", errors.New("the server sent no salt")
	case iterations <= 0:
		return nil, 0, "", errors.New("the server sent no iteration count")
	}
	return salt, iterations, nonce, nil
}

func mac(key []byte, message string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(message))
	return h.Sum(nil)
}

func clientNonce() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw[:]), nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
