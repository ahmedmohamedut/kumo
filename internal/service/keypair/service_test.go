package keypair_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
)

// Real fixtures generated with ssh-keygen so the fingerprint assertions below
// double-check against the OpenSSH implementation:
//
//	ssh-keygen -t ed25519 -C fixture-ed25519 -f fixture_ed25519
//	ssh-keygen -t rsa -b 4096 -C fixture-rsa4096 -f fixture_rsa4096
//	ssh-keygen -lf <file>.pub     # prints the SHA256: fingerprints below
const (
	ed25519PublicKey   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMSkpB4KwlcQbdHTekHMstz+XeDU0Eugj1nOCRMlwz0e fixture-ed25519"
	ed25519Fingerprint = "SHA256:nE1g6wUlVBQKTyKOSJF1xO4wZDGZSJaEgxVU2je12jg"

	rsa4096PublicKey   = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAACAQDMmL6FNAluCYZGNQs4/76CBVsslCSRwqH8pt6zpeUVKrxAo90ZF/4TPpTNhgkCEyjAnONZGSA6Fnkm3EqCRyKYe6QkW/Yhra7SQXuGjXhceAu2kaMDrWpWGt3xVLCNtLyLGhLz4/yWQdkdg3i5AFoO2EEuJXp88MWUSaNcRnEOvQAEbLGbU9mw9ICDH2fkWSI5Jbbaur7+a1W6C0NO+O/QIRi7y4Li79XStDHRbaJmxu0L771QVDOU/Uyl+Dy7pzDTrs6E0SoZulVBSSzhlQEiZb2F7MDm7VFBfBaVUtLxDF5k7A2TKair0EaR4i0fbje5G7sDWWuBXv3GwmxVXvImtYb9gyFVKzRcw4A3+q+3zZuaXGhd87Zit33G0A4vMbH/mntQ195C7twWUsHIWV4L22dngBbXj8W2uMnrn/x0vrFLWyBYDiIntq1i+DbJdB9NVIXPUNH01og8hIQJLvoz+JYM894gGwebrD676zxNHvnn8PV5IsG9ccTQNTgmVS3h9ndN/Ozaisr0OYlpJh+sL+5OGzL7MnX6DRLIGTbfDy7yBGLv1mCh8Bl/z+jTDgBhB5vJAiUofGVi2taAAgnBXWyRRg7s9PoBIEeV3Lym45ofYf3vyIDCoeFY2b6/NGpkUT+TKVYWXpwdtgevHxUDn1AWfOo7UpXkBxyG4Elw9w== fixture-rsa4096"
	rsa4096Fingerprint = "SHA256:0X8NCHcXhs6JyZafbPwK/WtThXvo0l2qoNQTehrRGTA"

	// pastedPrivateKey is a fragment of an OpenSSH private key, the form an
	// operator who grabbed the wrong file would paste into the import field.
	pastedPrivateKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n-----END OPENSSH PRIVATE KEY-----"
)

var testClock = func() time.Time {
	return time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
}

// harness is the service under test plus its store, for assertions below the
// API surface. now starts at testClock; advance steps it forward, since
// keyset pagination orders by created_at and a frozen clock collapses rows
// into a tie.
type harness struct {
	svc   *keypair.Service
	store *store.Store
	now   *time.Time
}

func newHarness(t *testing.T) harness {
	t.Helper()

	now := testClock()
	db := storetest.New(t)
	svc, err := keypair.New(keypair.Options{
		Logger: logging.Discard(),
		Store:  db,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("keypair.New(): %v", err)
	}

	return harness{svc: svc, store: db, now: &now}
}

// Now is the current time of the harness clock.
func (h harness) Now() time.Time { return *h.now }

// advance steps the harness clock forward by one second.
func (h harness) advance() { *h.now = h.now.Add(time.Second) }

func TestNewValidatesItsDependencies(t *testing.T) {
	t.Parallel()

	if _, err := keypair.New(keypair.Options{Store: storetest.New(t)}); !errors.Is(err, keypair.ErrNoLogger) {
		t.Errorf("New() error = %v, want ErrNoLogger", err)
	}
	if _, err := keypair.New(keypair.Options{Logger: logging.Discard()}); !errors.Is(err, keypair.ErrNoStore) {
		t.Errorf("New() error = %v, want ErrNoStore", err)
	}
}

func TestCreateGeneratesED25519ByDefault(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), keypair.CreateInput{
		Name: "workstation",
		Tags: map[string]string{"env": "dev"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if created.Type != keypair.TypeED25519 {
		t.Errorf("Type = %q, want ed25519", created.Type)
	}
	if !strings.HasPrefix(created.ID, "key-") {
		t.Errorf("ID = %q, want a key- prefixed id", created.ID)
	}
	if !strings.HasPrefix(created.Fingerprint, "SHA256:") {
		t.Errorf("Fingerprint = %q, want the OpenSSH SHA256: form", created.Fingerprint)
	}
	if !strings.HasPrefix(created.PublicKey, "ssh-ed25519 ") ||
		!strings.HasSuffix(created.PublicKey, " workstation") {
		t.Errorf("PublicKey = %q, want an authorized_keys line commented with the name", created.PublicKey)
	}
	if created.Imported {
		t.Error("Imported = true for a server generated key")
	}
	if !created.CreatedAt.Equal(testClock()) {
		t.Errorf("CreatedAt = %v, want %v", created.CreatedAt, testClock())
	}
	if created.Tags["env"] != "dev" {
		t.Errorf("Tags = %v, want env=dev", created.Tags)
	}

	// The private key must be a real OpenSSH PEM matching the public half.
	key, err := ssh.ParsePrivateKey([]byte(created.PrivateKey))
	if err != nil {
		t.Fatalf("ParsePrivateKey() error = %v", err)
	}
	got := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key.PublicKey())))
	want := strings.TrimSuffix(created.PublicKey, " workstation")
	if got != want {
		t.Errorf("public half of the private key = %q, want %q", got, want)
	}

	// The private key never reaches the database.
	assertNoPrivateKeyStored(t, h.store)
}

func TestCreateGeneratesRSA4096(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), keypair.CreateInput{
		Name: "builds",
		Type: keypair.TypeRSA,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if created.Type != keypair.TypeRSA {
		t.Errorf("Type = %q, want rsa", created.Type)
	}
	key, err := ssh.ParsePrivateKey([]byte(created.PrivateKey))
	if err != nil {
		t.Fatalf("ParsePrivateKey() error = %v", err)
	}
	cryptoPub, ok := key.PublicKey().(ssh.CryptoPublicKey)
	if !ok {
		t.Fatalf("the generated key is %T, want a crypto key", key.PublicKey())
	}
	rsaKey, ok := cryptoPub.CryptoPublicKey().(*rsa.PublicKey)
	if !ok {
		t.Fatalf("the generated key is %T, want *rsa.PublicKey", cryptoPub.CryptoPublicKey())
	}
	if bits := rsaKey.N.BitLen(); bits != 4096 {
		t.Errorf("RSA key size = %d bits, want 4096", bits)
	}
}

func TestCreateValidates(t *testing.T) {
	t.Parallel()

	tooManyTags := make(map[string]string, 51)
	for i := range 51 {
		tooManyTags[fmt.Sprintf("tag-%02d", i)] = ""
	}

	tests := []struct {
		name  string
		input keypair.CreateInput
		want  string
	}{
		{name: "an empty name", input: keypair.CreateInput{}, want: "name"},
		{name: "a name with a space", input: keypair.CreateInput{Name: "web key"}, want: "name"},
		{name: "a name with a slash", input: keypair.CreateInput{Name: "a/b"}, want: "name"},
		{name: "a 65 character name", input: keypair.CreateInput{Name: strings.Repeat("a", 65)}, want: "name"},
		{name: "an unknown type", input: keypair.CreateInput{Name: "k", Type: "dsa"}, want: "type"},
		{name: "more than fifty tags", input: keypair.CreateInput{Name: "k", Tags: tooManyTags}, want: "tags"},
		{
			name:  "an empty tag key",
			input: keypair.CreateInput{Name: "k", Tags: map[string]string{"": "v"}},
			want:  "tags",
		},
		{
			name:  "a 129 character tag key",
			input: keypair.CreateInput{Name: "k", Tags: map[string]string{strings.Repeat("k", 129): "v"}},
			want:  "tags",
		},
		{
			name:  "a 257 character tag value",
			input: keypair.CreateInput{Name: "k", Tags: map[string]string{"k": strings.Repeat("v", 257)}},
			want:  "tags",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)

			_, err := h.svc.Create(context.Background(), tt.input)
			assertFieldError(t, err, tt.want)
		})
	}
}

func TestCreateRejectsADuplicateName(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if _, err := h.svc.Create(context.Background(), keypair.CreateInput{Name: "workstation"}); err != nil {
		t.Fatalf("first Create() error = %v", err)
	}

	_, err := h.svc.Import(context.Background(), keypair.ImportInput{
		Name:      "workstation",
		PublicKey: ed25519PublicKey,
	})
	if !errors.Is(err, service.ErrAlreadyExists) {
		t.Fatalf("duplicate Create() error = %v, want ErrAlreadyExists", err)
	}
}

func TestImport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		publicKey       string
		wantType        keypair.Type
		wantFingerprint string
	}{
		{name: "ed25519", publicKey: ed25519PublicKey, wantType: keypair.TypeED25519, wantFingerprint: ed25519Fingerprint},
		{name: "rsa 4096", publicKey: rsa4096PublicKey, wantType: keypair.TypeRSA, wantFingerprint: rsa4096Fingerprint},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)

			pair, err := h.svc.Import(context.Background(), keypair.ImportInput{
				Name:      "laptop",
				PublicKey: tt.publicKey,
			})
			if err != nil {
				t.Fatalf("Import() error = %v", err)
			}

			if pair.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", pair.Type, tt.wantType)
			}
			if pair.Fingerprint != tt.wantFingerprint {
				t.Errorf("Fingerprint = %q, want the ssh-keygen fingerprint %q",
					pair.Fingerprint, tt.wantFingerprint)
			}
			if !pair.Imported {
				t.Error("Imported = false for an imported key")
			}
			// The stored line is canonical: same material, same comment.
			if pair.PublicKey != tt.publicKey {
				t.Errorf("PublicKey = %q, want %q", pair.PublicKey, tt.publicKey)
			}

			assertFingerprintMatchesSSHKeygen(t, tt.publicKey, pair.Fingerprint)
		})
	}
}

func TestImportRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		publicKey string
		wantField string
	}{
		{name: "an empty public key", publicKey: "", wantField: "public_key"},
		{name: "not a key", publicKey: "hello world", wantField: "public_key"},
		{name: "a truncated key", publicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMSkpB", wantField: "public_key"},
		{name: "pasted private key material", publicKey: pastedPrivateKey, wantField: "public_key"},
		{
			name:      "authorized_keys options",
			publicKey: `command="echo nope" ` + ed25519PublicKey,
			wantField: "public_key",
		},
		{
			name:      "two keys at once",
			publicKey: ed25519PublicKey + "\n" + ed25519PublicKey,
			wantField: "public_key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)

			_, err := h.svc.Import(context.Background(), keypair.ImportInput{
				Name:      "laptop",
				PublicKey: tt.publicKey,
			})
			assertFieldError(t, err, tt.wantField)

			// A refused import must not leave a row behind.
			page, listErr := h.svc.List(context.Background(), keypair.ListInput{})
			if listErr != nil {
				t.Fatalf("List() error = %v", listErr)
			}
			if len(page.Items) != 0 {
				t.Errorf("a rejected import left %d key pairs behind", len(page.Items))
			}
		})
	}
}

func TestImportRejectsWeakRSA(t *testing.T) {
	t.Parallel()

	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("rsa.GenerateKey(): %v", err)
	}
	line := authorizedLineOf(t, weak.Public())

	h := newHarness(t)

	_, err = h.svc.Import(context.Background(), keypair.ImportInput{Name: "laptop", PublicKey: line})
	assertFieldReason(t, err, "public_key", "2048")
}

func TestImportRejectsUnsupportedAlgorithms(t *testing.T) {
	t.Parallel()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey(): %v", err)
	}
	line := authorizedLineOf(t, key.Public())

	h := newHarness(t)

	_, err = h.svc.Import(context.Background(), keypair.ImportInput{Name: "laptop", PublicKey: line})
	assertFieldReason(t, err, "public_key", "not supported")
}

func TestListPaginatesInCreationOrder(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	for _, name := range []string{"alpha", "bravo", "charlie"} {
		h.advance() // created_at drives the page order, so space the keys out
		if _, err := h.svc.Create(context.Background(), keypair.CreateInput{Name: name}); err != nil {
			t.Fatalf("Create(%q) error = %v", name, err)
		}
	}

	// Newest first by default, one item per page.
	page, err := h.svc.List(context.Background(), keypair.ListInput{PageSize: 1})
	if err != nil {
		t.Fatalf("List() page 1 error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Name != "charlie" {
		t.Fatalf("page 1 = %+v, want charlie alone", page.Items)
	}
	if page.NextPageToken == nil {
		t.Fatal("page 1 has no next_page_token, want one")
	}

	page2, err := h.svc.List(context.Background(), keypair.ListInput{PageSize: 1, PageToken: *page.NextPageToken})
	if err != nil {
		t.Fatalf("List() page 2 error = %v", err)
	}
	if len(page2.Items) != 1 || page2.Items[0].Name != "bravo" {
		t.Fatalf("page 2 = %+v, want bravo alone", page2.Items)
	}

	page3, err := h.svc.List(context.Background(), keypair.ListInput{PageSize: 1, PageToken: *page2.NextPageToken})
	if err != nil {
		t.Fatalf("List() page 3 error = %v", err)
	}
	if len(page3.Items) != 1 || page3.Items[0].Name != "alpha" {
		t.Fatalf("page 3 = %+v, want alpha alone", page3.Items)
	}
	if page3.NextPageToken != nil {
		t.Errorf("page 3 next_page_token = %q, want none on the last page", *page3.NextPageToken)
	}

	// Ascending order flips the page order.
	asc, err := h.svc.List(context.Background(), keypair.ListInput{PageSize: 2, Sort: keypair.SortCreatedAsc})
	if err != nil {
		t.Fatalf("List(asc) error = %v", err)
	}
	if asc.Items[0].Name != "alpha" || asc.Items[1].Name != "bravo" {
		t.Errorf("asc page = %+v, want alpha then bravo", asc.Items)
	}

	// A name substring filters the listing.
	filtered, err := h.svc.List(context.Background(), keypair.ListInput{Query: "char"})
	if err != nil {
		t.Fatalf("List(q=char) error = %v", err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].Name != "charlie" {
		t.Errorf("filtered page = %+v, want charlie alone", filtered.Items)
	}
}

func TestListRejectsBadArguments(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	tests := []struct {
		name  string
		input keypair.ListInput
		want  string
	}{
		{name: "an unknown sort", input: keypair.ListInput{Sort: "name:desc"}, want: "sort"},
		{name: "a negative page size", input: keypair.ListInput{PageSize: -1}, want: "page_size"},
		{name: "an oversized page", input: keypair.ListInput{PageSize: 500}, want: "page_size"},
		{name: "a mangled page token", input: keypair.ListInput{PageToken: "not base64 ==="}, want: "page_token"},
		{
			name:  "a page token for a foreign resource",
			input: keypair.ListInput{PageToken: tokenFor(t, "2026-01-02T10:00:00Z", "i-0123456789abcdef0")},
			want:  "page_token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := h.svc.List(context.Background(), tt.input)
			assertFieldError(t, err, tt.want)
		})
	}
}

func TestGet(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), keypair.CreateInput{
		Name: "workstation",
		Tags: map[string]string{"team": "platform"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// An instance launched with this key shows up in used_by.
	instanceID := launchFakeInstance(t, h.store, created.ID, "web-1")

	detail, err := h.svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if detail.Name != "workstation" || detail.Tags["team"] != "platform" {
		t.Errorf("Get() = %+v, want the stored key pair", detail)
	}
	if len(detail.UsedBy) != 1 || detail.UsedBy[0] != instanceID {
		t.Errorf("UsedBy = %v, want [%s]", detail.UsedBy, instanceID)
	}
}

func TestGetUnknownKeyPair(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, err := h.svc.Get(context.Background(), "key-00000000000000000")
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), keypair.CreateInput{Name: "temp"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := h.svc.Delete(context.Background(), created.ID, false); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	// Deleting is idempotent-exact: a second attempt is a 404, and the tags
	// went away with the row.
	if err := h.svc.Delete(context.Background(), created.ID, false); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("second Delete() error = %v, want ErrNotFound", err)
	}
	var tagCount int
	if err := h.store.DB().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM tags WHERE resource_id = ?", created.ID).Scan(&tagCount); err != nil {
		t.Fatalf("count tags: %v", err)
	}
	if tagCount != 0 {
		t.Errorf("%d orphan tags remain after delete", tagCount)
	}
}

func TestDeleteRefusesAKeyInUse(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), keypair.CreateInput{Name: "production"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	launchFakeInstance(t, h.store, created.ID, "db-1")

	err = h.svc.Delete(context.Background(), created.ID, false)
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("Delete() error = %v, want ErrConflict", err)
	}

	// force=true overrides, as in AWS.
	if err := h.svc.Delete(context.Background(), created.ID, true); err != nil {
		t.Errorf("Delete(force) error = %v", err)
	}

	// The instance survives the force delete; the foreign key just un-links
	// the key pair.
	var keyPairID *string
	if err := h.store.DB().QueryRowContext(context.Background(),
		"SELECT key_pair_id FROM instances WHERE name = 'db-1'").Scan(&keyPairID); err != nil {
		t.Fatalf("read instance: %v", err)
	}
	if keyPairID != nil {
		t.Errorf("instance key_pair_id = %q, want NULL after the delete", *keyPairID)
	}
}

func TestUpdateTags(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), keypair.CreateInput{
		Name: "workstation",
		Tags: map[string]string{"env": "dev"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	updated, err := h.svc.UpdateTags(context.Background(), created.ID,
		map[string]string{"env": "prod", "owner": "ada"})
	if err != nil {
		t.Fatalf("UpdateTags() error = %v", err)
	}
	if updated.Tags["env"] != "prod" || updated.Tags["owner"] != "ada" {
		t.Errorf("Tags = %v, want the replaced set", updated.Tags)
	}

	// An empty map clears the set.
	cleared, err := h.svc.UpdateTags(context.Background(), created.ID, map[string]string{})
	if err != nil {
		t.Fatalf("UpdateTags({}) error = %v", err)
	}
	if len(cleared.Tags) != 0 {
		t.Errorf("Tags = %v, want empty", cleared.Tags)
	}

	// Unknown key pairs and bad tags are their own errors.
	if _, err := h.svc.UpdateTags(context.Background(), "key-00000000000000000", nil); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("UpdateTags(unknown) error = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.UpdateTags(context.Background(), created.ID, map[string]string{"": "x"}); err == nil {
		t.Error("UpdateTags(invalid) error = nil, want a validation failure")
	}
}

// assertFieldError wants err to be a validation failure about the given field.
func assertFieldError(t *testing.T, err error, field string) {
	t.Helper()

	assertFieldReason(t, err, field, "")
}

// assertFieldReason additionally wants the field reason to carry a substring.
func assertFieldReason(t *testing.T, err error, field, reasonPart string) {
	t.Helper()

	var validation *service.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %v, want a *service.ValidationError about %q", err, field)
	}
	for _, item := range validation.Fields {
		if item.Field == field && strings.Contains(item.Reason, reasonPart) {
			return
		}
	}
	t.Errorf("Fields = %+v, want an error about %q mentioning %q", validation.Fields, field, reasonPart)
}

// assertNoPrivateKeyStored fails if any private key fragment reached the
// database.
func assertNoPrivateKeyStored(t *testing.T, db *store.Store) {
	t.Helper()

	var count int
	err := db.DB().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM key_pairs WHERE public_key LIKE '%PRIVATE KEY%'").Scan(&count)
	if err != nil {
		t.Fatalf("scan key_pairs: %v", err)
	}
	if count != 0 {
		t.Errorf("%d private keys stored; the private half must never be persisted", count)
	}
}

// assertFingerprintMatchesSSHKeygen runs the real ssh-keygen -lf on the
// public key and compares its answer, when the binary is around. The fixture
// constants above pin the same values, so this is a belt-and-braces check
// that skips on minimal systems.
func assertFingerprintMatchesSSHKeygen(t *testing.T, publicKey, want string) {
	t.Helper()

	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not available")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.pub")
	if err := os.WriteFile(path, []byte(publicKey+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	out, err := exec.Command("ssh-keygen", "-lf", path).Output()
	if err != nil {
		t.Fatalf("ssh-keygen -lf: %v", err)
	}

	parts := strings.Fields(string(out))
	if len(parts) < 2 || parts[1] != want {
		t.Errorf("ssh-keygen -lf = %q, want fingerprint %q", strings.TrimSpace(string(out)), want)
	}
}

// authorizedLineOf renders the public half of a generated key.
func authorizedLineOf(t *testing.T, publicKey any) string {
	t.Helper()

	sshKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatalf("ssh.NewPublicKey(%T): %v", publicKey, err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshKey)))
}

// tokenFor builds a page token shaped like the ones List hands out, without
// exporting the encoding from the service.
func tokenFor(_ *testing.T, createdAt, resourceID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt + "|" + resourceID))
}

// launchFakeInstance inserts the minimum viable instance row that references
// the key pair, standing in for the M4 launch workflow.
func launchFakeInstance(t *testing.T, db *store.Store, keyPairID, name string) string {
	t.Helper()

	if _, err := db.Exec(context.Background(),
		`INSERT OR IGNORE INTO instance_types (id, vcpus, memory_mib) VALUES ('kumo.small', 1, 2048)`); err != nil {
		t.Fatalf("insert instance type: %v", err)
	}

	imageID := "img-" + strings.Repeat("0", 17)
	if _, err := db.Exec(context.Background(),
		`INSERT INTO images (id, name, state, created_at, updated_at)
		 VALUES (?1, ?1, 'available', '2026-01-02T09:00:00Z', '2026-01-02T09:00:00Z')`,
		imageID); err != nil {
		t.Fatalf("insert image: %v", err)
	}

	instanceID := "i-" + strings.Repeat("1", 17)
	if _, err := db.Exec(context.Background(),
		`INSERT INTO instances (id, name, state, image_id, instance_type, vcpus, memory_mib,
		  root_disk_gb, key_pair_id, launched_at, updated_at)
		 VALUES (?1, ?2, 'running', ?3, 'kumo.small', 1, 2048, 10, ?4,
		  '2026-01-02T09:05:00Z', '2026-01-02T09:05:00Z')`,
		instanceID, name, imageID, keyPairID); err != nil {
		t.Fatalf("insert instance: %v", err)
	}

	return instanceID
}
