package sparkapi_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/sparkapi"
)

// masterJSON is what a Spark 4.3 master answers at /json/, trimmed: the fields
// sparkapi reads and some it does not.
const masterJSON = `{
  "url": "spark://10.0.0.10:7077",
  "workers": [
    {"id": "worker-20261120214238-10.0.0.11-42999", "host": "10.0.0.11", "port": 42999,
     "webuiaddress": "http://10.0.0.11:8081", "cores": 2, "coresused": 2, "coresfree": 0,
     "memory": 1536, "memoryused": 1024, "memoryfree": 512, "resources": {},
     "state": "ALIVE", "lastheartbeat": 1795210962378},
    {"id": "worker-20261120214238-10.0.0.12-33177", "host": "10.0.0.12", "port": 33177,
     "cores": 2, "coresused": 0, "memory": 1536, "memoryused": 0,
     "state": "DECOMMISSIONED", "lastheartbeat": 1795210960000}
  ],
  "aliveworkers": 1,
  "cores": 2, "coresused": 2, "memory": 1536, "memoryused": 1024,
  "resources": [], "resourcesused": [],
  "activeapps": [{"id": "app-20261120214242-0000", "name": "Spark Pi", "state": "RUNNING"}],
  "completedapps": [],
  "activedrivers": [], "completeddrivers": [],
  "status": "ALIVE"
}`

// master serves h at /json/ and, as Spark 4.3 does, redirects /json there.
func master(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/json/", h)
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/json/", http.StatusMovedPermanently)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func client(t *testing.T, address string, timeout time.Duration) *sparkapi.Client {
	t.Helper()
	c, err := sparkapi.New(sparkapi.Config{Address: address, Timeout: timeout})
	if err != nil {
		t.Fatalf("New(%q): %v", address, err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestStatusOfAnAliveMaster(t *testing.T) {
	t.Parallel()
	addr := master(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/" || r.Method != http.MethodGet {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(masterJSON))
	})
	st, err := client(t, addr, 0).Status(t.Context())
	if err != nil {
		t.Fatalf("Status(): %v", err)
	}
	if !st.Alive() || st.ActiveApps != 1 || len(st.Workers) != 2 {
		t.Fatalf("status = %+v", st)
	}
	w := st.Workers[0]
	if w.Host != "10.0.0.11" || w.State != sparkapi.WorkerAlive || w.Cores != 2 || w.CoresUsed != 2 ||
		w.MemoryMiB != 1536 || w.MemoryUsed != 1024 || w.Port != 42999 {
		t.Errorf("worker = %+v", w)
	}
	alive := st.AliveWorkers()
	if !alive["10.0.0.11"] || alive["10.0.0.12"] || len(alive) != 1 {
		t.Errorf("AliveWorkers() = %v, want 10.0.0.11 alone (the other is decommissioned)", alive)
	}
}

// A master in standby, or recovering, answers with another status: it is not
// alive, and the health pass must say so.
func TestStatusOfAMasterThatDoesNotLead(t *testing.T) {
	t.Parallel()
	addr := master(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"STANDBY","workers":[]}`))
	})
	st, err := client(t, addr, 0).Status(t.Context())
	if err != nil || st.Alive() || st.Status != "STANDBY" {
		t.Errorf("Status() = %+v, %v, want STANDBY and not alive", st, err)
	}
}

func TestStatusFailures(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		handler http.HandlerFunc
		want    error
	}{
		"a server error": {func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}, sparkapi.ErrUnavailable},
		// Not followed: a status page that moved is reported, not chased.
		"a redirect": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}, sparkapi.ErrUnavailable},
		"an HTML page": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html><body>Spark Master</body></html>"))
		}, sparkapi.ErrBadResponse},
		"JSON without a status": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"workers":[]}`))
		}, sparkapi.ErrBadResponse},
		"a truncated body": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(masterJSON[:120]))
		}, sparkapi.ErrBadResponse},
		"a body over the limit": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"ALIVE","pad":"` + strings.Repeat("x", 5<<20) + `"}`))
		}, sparkapi.ErrBadResponse},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			addr := master(t, tc.handler)
			if _, err := client(t, addr, 0).Status(t.Context()); !errors.Is(err, tc.want) {
				t.Errorf("Status() = %v, want %v", err, tc.want)
			}
		})
	}
}

// A master that accepts the connection and never answers is back within the
// timeout, so it cannot hold the health pass.
func TestStatusOfAMasterThatNeverAnswers(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	addr := master(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	start := time.Now()
	_, err := client(t, addr, 300*time.Millisecond).Status(t.Context())
	if !errors.Is(err, sparkapi.ErrUnavailable) {
		t.Errorf("Status() = %v, want ErrUnavailable", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("Status() took %s with a 300ms timeout", took)
	}
}

func TestStatusOfNothingListening(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	if _, err := client(t, addr, time.Second).Status(t.Context()); !errors.Is(err, sparkapi.ErrUnavailable) {
		t.Errorf("Status() = %v, want ErrUnavailable", err)
	}
}

func TestStatusHonorsTheContext(t *testing.T) {
	t.Parallel()
	addr := master(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client(t, addr, time.Minute).Status(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Status() with a canceled context = %v", err)
	}
}

func TestNewChecksTheAddress(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"", ":8080", "10.0.0.10:0", "10.0.0.10:99999", "10.0.0.10:http"} {
		if _, err := sparkapi.New(sparkapi.Config{Address: addr}); !errors.Is(err, sparkapi.ErrInvalidConfig) {
			t.Errorf("New(%q) = %v, want ErrInvalidConfig", addr, err)
		}
	}
	// A bare host means the web UI's port.
	addr := master(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(masterJSON)) })
	host, port, _ := net.SplitHostPort(addr)
	if port == "8080" {
		t.Skip("the test server happens to be on 8080")
	}
	if _, err := sparkapi.New(sparkapi.Config{Address: host}); err != nil {
		t.Errorf("New(%q) = %v, want a client on port 8080", host, err)
	}
}
