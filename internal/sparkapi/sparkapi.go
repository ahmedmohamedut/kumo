// Package sparkapi reads a Spark standalone master's own status, the one view
// of a Spark cluster kumod needs (docs/design/09-spark.md section 6): is the
// master alive, and which workers are registered in which state.
//
// It is the standard library and nothing else, as internal/k8sapi is: one GET
// of the master's web UI at /json/, parsed with encoding/json. The trailing
// slash matters -- Spark 4.3 answers /json with a 301 to /json/ -- and the
// client never follows redirects, so a master that moved its status page fails
// loudly instead of answering with some other page. No credential is sent or
// needed: the status page is what any host of the VPC can read.
package sparkapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// DefaultTimeout bounds one status read. The health pass runs it for every
// cluster in turn, so a master that hangs must not hold the pass.
const DefaultTimeout = 10 * time.Second

// maxBody bounds what is read of an answer. A master's status with hundreds of
// workers and applications is tens of kilobytes.
const maxBody = 4 << 20

// statusPath is the master's JSON status.
const statusPath = "/json/"

// Worker states as the master reports them.
const (
	WorkerAlive          = "ALIVE"
	WorkerDead           = "DEAD"
	WorkerDecommissioned = "DECOMMISSIONED"
	WorkerUnknown        = "UNKNOWN"
)

// MasterAlive is the status of a master that leads and accepts work.
const MasterAlive = "ALIVE"

var (
	// ErrUnavailable means the master could not be reached, did not answer
	// in time, or answered with something other than its status.
	ErrUnavailable = errors.New("sparkapi: the master is unavailable")
	// ErrBadResponse means the master answered 200 with a body that is not
	// its status.
	ErrBadResponse = errors.New("sparkapi: unreadable status")
	// ErrInvalidConfig means the client was built with an address it cannot
	// use.
	ErrInvalidConfig = errors.New("sparkapi: invalid configuration")
)

// Worker is one worker as the master sees it.
type Worker struct {
	ID         string `json:"id"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	State      string `json:"state"`
	Cores      int    `json:"cores"`
	CoresUsed  int    `json:"coresused"`
	MemoryMiB  int    `json:"memory"`
	MemoryUsed int    `json:"memoryused"`
}

// Alive reports whether the master counts the worker as usable.
func (w Worker) Alive() bool { return w.State == WorkerAlive }

// Status is the master's view of its cluster.
type Status struct {
	// Status is the master's own state: ALIVE when it leads.
	Status  string   `json:"status"`
	Workers []Worker `json:"workers"`
	// ActiveApps is how many applications are running now.
	ActiveApps int `json:"-"`
}

// Alive reports whether the master leads.
func (s Status) Alive() bool { return s.Status == MasterAlive }

// AliveWorkers is the workers the master counts as usable, by host.
func (s Status) AliveWorkers() map[string]bool {
	out := map[string]bool{}
	for _, w := range s.Workers {
		if w.Alive() {
			out[w.Host] = true
		}
	}
	return out
}

// Client reads one master's status.
type Client struct {
	url  string
	http *http.Client
}

// Config is where the master is.
type Config struct {
	// Address is the master's VPC address, host or host:port; the port is
	// the web UI's, 8080, when it is missing.
	Address string
	// Timeout bounds one read; DefaultTimeout when zero.
	Timeout time.Duration
}

// New builds a client.
func New(cfg Config) (*Client, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("%w: no address", ErrInvalidConfig)
	}
	host, port, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		host, port = cfg.Address, "8080"
	}
	if host == "" {
		return nil, fmt.Errorf("%w: address %q has no host", ErrInvalidConfig, cfg.Address)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("%w: address %q has no valid port", ErrInvalidConfig, cfg.Address)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		url: "http://" + net.JoinHostPort(host, port) + statusPath,
		http: &http.Client{
			Timeout: timeout,
			// A redirect is an answer that is not the status: report it,
			// never follow it somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				Proxy:               nil, // a VPC address is never reached through a proxy
				DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
				MaxIdleConns:        1,
				IdleConnTimeout:     30 * time.Second,
				TLSHandshakeTimeout: timeout,
			},
		},
	}, nil
}

// Close releases the client's idle connection.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// rawStatus is the subset of the master's answer that is read.
type rawStatus struct {
	Status     *string           `json:"status"`
	Workers    []Worker          `json:"workers"`
	ActiveApps []json.RawMessage `json:"activeapps"`
}

// Status reads the master's status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, http.NoBody)
	if err != nil {
		return Status{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Status{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close() //nolint:errcheck // a read-only body
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return Status{}, fmt.Errorf("%w: %s answered %d", ErrUnavailable, c.url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return Status{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if len(body) > maxBody {
		return Status{}, fmt.Errorf("%w: the status is over %d bytes", ErrBadResponse, maxBody)
	}
	var raw rawStatus
	if err := json.Unmarshal(body, &raw); err != nil {
		return Status{}, fmt.Errorf("%w: %w", ErrBadResponse, err)
	}
	if raw.Status == nil {
		return Status{}, fmt.Errorf("%w: no status field", ErrBadResponse)
	}
	return Status{Status: *raw.Status, Workers: raw.Workers, ActiveApps: len(raw.ActiveApps)}, nil
}
