package libvirt

import (
	"context"
	"sync"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
)

// Reconnector wraps a libvirt connection with reconnect-on-error behavior.
type Reconnector struct {
	mu sync.RWMutex

	// driverURI is a libvirt connection URI such as qemu:///system; socketPath
	// is the unix socket to dial.
	driverURI  string
	socketPath string
	conn       *golibvirt.Libvirt
	alive      bool
	lastErr    error
	lastPing   time.Time
}

// NewReconnector creates a new reconnecting wrapper.
func NewReconnector(driverURI, socketPath string) *Reconnector {
	return &Reconnector{
		driverURI:  driverURI,
		socketPath: socketPath,
	}
}

// Connect establishes the initial connection.
func (r *Reconnector) Connect(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.conn != nil && r.conn.IsConnected() {
		return nil
	}

	conn := golibvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(r.socketPath)))
	if err := conn.ConnectToURI(golibvirt.ConnectURI(r.driverURI)); err != nil {
		return err
	}

	r.conn = conn
	r.alive = true
	return nil
}

// IsAlive reports whether the connection is alive.
func (r *Reconnector) IsAlive() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.conn == nil {
		return false
	}
	return r.conn.IsConnected()
}

// Close closes the underlying connection.
func (r *Reconnector) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.conn == nil {
		return nil
	}

	err := r.conn.Disconnect()
	r.conn = nil
	r.alive = false
	return err
}

// ensureConnected reconnects if needed.
func (r *Reconnector) ensureConnected(ctx context.Context) (*golibvirt.Libvirt, error) {
	r.mu.RLock()
	if r.conn != nil && r.conn.IsConnected() {
		c := r.conn
		r.mu.RUnlock()
		return c, nil
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.conn != nil && r.conn.IsConnected() {
		return r.conn, nil
	}

	if r.conn != nil {
		_ = r.conn.Disconnect()
		r.conn = nil
	}

	conn := golibvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(r.socketPath)))
	if err := conn.ConnectToURI(golibvirt.ConnectURI(r.driverURI)); err != nil {
		r.alive = false
		r.lastErr = err
		return nil, err
	}

	r.conn = conn
	r.alive = true
	r.lastErr = nil
	r.lastPing = time.Now()
	return conn, nil
}
