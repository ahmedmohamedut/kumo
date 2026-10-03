package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
)

func TestHealthz(t *testing.T) {
	t.Parallel()

	recorder, response := call(t, testRouter(t, nil, nil, nil), http.MethodGet, "/healthz")

	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200 (body %s)", response.StatusCode, recorder.Body.String())
	}

	var body healthBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("GET /healthz body = %+v, want status ok", body)
	}
}

func TestReadyz(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		checks     []api.Check
		wantStatus int
		wantState  string
		wantChecks map[string]string
	}{
		{
			name:       "no checks means ready",
			wantStatus: http.StatusOK,
			wantState:  "ok",
			wantChecks: map[string]string{},
		},
		{
			name: "all checks pass",
			checks: []api.Check{
				{Name: "db", Probe: func(context.Context) error { return nil }},
				{Name: "hypervisor", Probe: func(context.Context) error { return nil }},
			},
			wantStatus: http.StatusOK,
			wantState:  "ok",
			wantChecks: map[string]string{"db": "ok", "hypervisor": "ok"},
		},
		{
			name: "a failing check is reported without its reason",
			checks: []api.Check{
				{Name: "db", Probe: func(context.Context) error { return nil }},
				{Name: "hypervisor", Probe: func(context.Context) error {
					return fmt.Errorf("%w: dial unix /var/run/libvirt/libvirt-sock", errLibvirtUnreachable)
				}},
			},
			wantStatus: http.StatusServiceUnavailable,
			wantState:  "unavailable",
			wantChecks: map[string]string{"db": "ok", "hypervisor": "failed"},
		},
		{
			name: "a timing out dependency fails the check",
			checks: []api.Check{
				{Name: "db", Probe: func(context.Context) error { return context.DeadlineExceeded }},
			},
			wantStatus: http.StatusServiceUnavailable,
			wantState:  "unavailable",
			wantChecks: map[string]string{"db": "failed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder, response := call(t, testRouter(t, nil, tt.checks, nil), http.MethodGet, "/readyz")

			if response.StatusCode != tt.wantStatus {
				t.Fatalf("GET /readyz status = %d, want %d", response.StatusCode, tt.wantStatus)
			}

			var body readyBody
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Status != tt.wantState {
				t.Errorf("status = %q, want %q", body.Status, tt.wantState)
			}
			if len(body.Checks) != len(tt.wantChecks) {
				t.Fatalf("checks = %v, want %v", body.Checks, tt.wantChecks)
			}
			for name, want := range tt.wantChecks {
				if body.Checks[name] != want {
					t.Errorf("checks[%q] = %q, want %q", name, body.Checks[name], want)
				}
			}
			if strings.Contains(recorder.Body.String(), "connection refused") {
				t.Error("readyz leaked the reason of a failed check")
			}
		})
	}
}

func TestSystemInfo(t *testing.T) {
	t.Parallel()

	t.Run("without a store", func(t *testing.T) {
		t.Parallel()

		svc := system.New(logging.Discard(), "test", "fake", nil, nil)
		recorder, response := call(t, testRouter(t, nil, nil, svc), http.MethodGet, "/api/v1/system/info")

		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", response.StatusCode, recorder.Body.String())
		}

		var body system.Info
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		want := system.Info{
			Version:    "test",
			NeedsSetup: false,
			Hypervisor: system.HypervisorInfo{Type: "fake", OK: false},
		}
		if body != want {
			t.Errorf("body = %+v, want %+v", body, want)
		}
	})

	t.Run("a failing store becomes a 500 without details", func(t *testing.T) {
		t.Parallel()

		logger, logs := testLogger()
		svc := system.New(logger, "test", "libvirt", failingUsers{}, nil)

		recorder, response := call(t, testRouter(t, logger, nil, svc), http.MethodGet, "/api/v1/system/info")

		if response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", response.StatusCode)
		}
		body := decodeError(t, recorder.Body.Bytes())
		if body.Error.Code != api.CodeInternal {
			t.Errorf("code = %q, want %q", body.Error.Code, api.CodeInternal)
		}
		if strings.Contains(recorder.Body.String(), "database is gone") {
			t.Errorf("the 500 body leaked the error: %s", recorder.Body.String())
		}
		if !strings.Contains(logs.String(), "database is gone") {
			t.Errorf("the real error was not logged: %s", logs.String())
		}
	})
}

func TestConsoleFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantBody   string
		wantJSON   bool
	}{
		{
			name:       "the root is served by the console",
			method:     http.MethodGet,
			path:       "/",
			wantStatus: http.StatusOK,
			wantBody:   "<!doctype html>",
		},
		{
			name:       "a client-side route is served by the console",
			method:     http.MethodGet,
			path:       "/instances/i-0123456789abcdef0",
			wantStatus: http.StatusOK,
			wantBody:   "<!doctype html>",
		},
		{
			name:       "an unknown API route keeps the error envelope",
			method:     http.MethodGet,
			path:       "/api/v1/instances/i-0123456789abcdef0",
			wantStatus: http.StatusNotFound,
			wantJSON:   true,
		},
		{
			name:       "the API root keeps the error envelope",
			method:     http.MethodGet,
			path:       "/api",
			wantStatus: http.StatusNotFound,
			wantJSON:   true,
		},
		{
			name:       "the console does not swallow health checks",
			method:     http.MethodPost,
			path:       "/healthz",
			wantStatus: http.StatusMethodNotAllowed,
			wantJSON:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			router := testRouterWith(t, nil, nil, nil, nil, consoleHandler())

			recorder, _ := call(t, router, tt.method, tt.path)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantJSON {
				if body := decodeError(t, recorder.Body.Bytes()); body.Error.Code != api.CodeNotFound &&
					body.Error.Code != api.CodeMethodNotAllowed {
					t.Errorf("code = %q, want a JSON error code", body.Error.Code)
				}
				return
			}
			if !strings.Contains(recorder.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", recorder.Body.String(), tt.wantBody)
			}
		})
	}

	t.Run("without a console unknown routes stay JSON", func(t *testing.T) {
		t.Parallel()

		recorder, _ := call(t, testRouter(t, nil, nil, nil), http.MethodGet, "/instances/i-0123456789abcdef0")

		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", recorder.Code)
		}
		if body := decodeError(t, recorder.Body.Bytes()); body.Error.Code != api.CodeNotFound {
			t.Errorf("code = %q, want %q", body.Error.Code, api.CodeNotFound)
		}
	})
}

// consoleHandler stands in for internal/webui, which is exercised by its own
// package tests.
func consoleHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "<!doctype html><title>Kumo</title><body>%s", r.URL.Path)
	})
}

func TestUnknownRoutesAndMethodsUseTheEnvelope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "route not implemented yet",
			method:     http.MethodGet,
			path:       "/api/v1/instances",
			wantStatus: http.StatusNotFound,
			wantCode:   api.CodeNotFound,
		},
		{
			name:       "unknown nested route",
			method:     http.MethodPost,
			path:       "/nope/deeper",
			wantStatus: http.StatusNotFound,
			wantCode:   api.CodeNotFound,
		},
		{
			name:       "wrong method",
			method:     http.MethodPost,
			path:       "/healthz",
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   api.CodeMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder, response := call(t, testRouter(t, nil, nil, nil), tt.method, tt.path)

			if response.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", response.StatusCode, tt.wantStatus, recorder.Body.String())
			}

			body := decodeError(t, recorder.Body.Bytes())
			if body.Error.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.wantCode)
			}
			if body.Error.Message == "" {
				t.Error("message is empty")
			}
			if body.Error.RequestID == "" {
				t.Error("request_id is empty")
			}
			if got := response.Header.Get("X-Request-Id"); got != body.Error.RequestID {
				t.Errorf("X-Request-Id = %q, envelope request_id = %q", got, body.Error.RequestID)
			}
		})
	}
}

func TestRequestID(t *testing.T) {
	t.Parallel()

	t.Run("mints a ULID", func(t *testing.T) {
		t.Parallel()

		_, response := call(t, testRouter(t, nil, nil, nil), http.MethodGet, "/healthz")

		requestID := response.Header.Get("X-Request-Id")
		if len(requestID) != 26 {
			t.Errorf("X-Request-Id = %q, want a 26 character ULID", requestID)
		}
	})

	t.Run("reuses a caller supplied id", func(t *testing.T) {
		t.Parallel()

		router := testRouter(t, nil, nil, nil)
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/nope", http.NoBody)
		request.Header.Set("X-Request-Id", "caller-supplied-id")
		router.ServeHTTP(recorder, request)

		if got := recorder.Header().Get("X-Request-Id"); got != "caller-supplied-id" {
			t.Errorf("X-Request-Id = %q, want the caller supplied id", got)
		}
		if body := decodeError(t, recorder.Body.Bytes()); body.Error.RequestID != "caller-supplied-id" {
			t.Errorf("envelope request_id = %q", body.Error.RequestID)
		}
	})

	t.Run("replaces an absurdly long id", func(t *testing.T) {
		t.Parallel()

		router := testRouter(t, nil, nil, nil)
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
		request.Header.Set("X-Request-Id", strings.Repeat("x", 512))
		router.ServeHTTP(recorder, request)

		if got := recorder.Header().Get("X-Request-Id"); len(got) != 26 {
			t.Errorf("X-Request-Id = %q, want a fresh 26 character ULID", got)
		}
	})
}

func TestCORS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		origin     string
		origins    []string
		wantHeader bool
	}{
		{
			name:       "allowed dev origin",
			origin:     "http://localhost:5173",
			origins:    []string{"http://localhost:5173"},
			wantHeader: true,
		},
		{
			name:       "unknown origin",
			origin:     "http://evil.example",
			origins:    []string{"http://localhost:5173"},
			wantHeader: false,
		},
		{
			name:       "no origins configured",
			origin:     "http://localhost:5173",
			origins:    nil,
			wantHeader: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			router := testRouter(t, nil, nil, nil, tt.origins)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
			request.Header.Set("Origin", tt.origin)
			router.ServeHTTP(recorder, request)

			got := recorder.Header().Get("Access-Control-Allow-Origin")
			if tt.wantHeader && got != tt.origin {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tt.origin)
			}
			if !tt.wantHeader && got != "" {
				t.Errorf("Access-Control-Allow-Origin = %q, want none", got)
			}
		})
	}
}

func TestCORSPreflight(t *testing.T) {
	t.Parallel()

	router := testRouter(t, nil, nil, nil, []string{"http://localhost:5173"})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodOptions, "/api/v1/system/info", http.NoBody)
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("Access-Control-Request-Method", http.MethodGet)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", recorder.Code)
	}
	if recorder.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Error("preflight response has no Access-Control-Allow-Headers")
	}
}

func TestRecoveryKeepsTheProcessAlive(t *testing.T) {
	t.Parallel()

	logger, logs := testLogger()
	router := testRouter(t, logger, nil, nil)
	router.GET("/panic", func(*gin.Context) { panic("boom") })

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panic", http.NoBody))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", recorder.Code, recorder.Body.String())
	}
	if body := decodeError(t, recorder.Body.Bytes()); body.Error.Code != api.CodeInternal {
		t.Errorf("code = %q, want %q", body.Error.Code, api.CodeInternal)
	}
	if !strings.Contains(logs.String(), "boom") {
		t.Errorf("the panic was not logged: %s", logs.String())
	}

	healthy := httptest.NewRecorder()
	router.ServeHTTP(healthy, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	if healthy.Code != http.StatusOK {
		t.Errorf("GET /healthz after a panic = %d, want 200", healthy.Code)
	}
}

func TestAccessLogIsOneJSONLinePerRequest(t *testing.T) {
	t.Parallel()

	logger, logs := testLogger()
	recorder := httptest.NewRecorder()
	testRouter(t, logger, nil, nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz?probe=1", http.NoBody))

	var entry map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
	}

	for _, key := range []string{"time", "level", "msg", "request_id", "method", "path", "status", "latency_ms"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("access log entry has no %q: %s", key, logs.String())
		}
	}
	if entry["path"] != "/healthz" {
		t.Errorf("path = %v, want /healthz", entry["path"])
	}
	if entry["query"] != "probe=1" {
		t.Errorf("query = %v, want probe=1", entry["query"])
	}
	if entry["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200", entry["status"])
	}
}

func TestServerGracefulShutdown(t *testing.T) {
	t.Parallel()

	server := api.NewServer(logging.Discard(), "127.0.0.1:0", testRouter(t, nil, nil, nil))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, listener) }()

	url := "http://" + listener.Addr().String() + "/healthz"
	waitUntilHealthy(t, url)

	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not return after the context was canceled")
	}

	if response, err := http.Get(url); err == nil { //nolint:noctx // short-lived probe
		response.Body.Close() //nolint:errcheck // nothing to read
		t.Error("the server still answers after shutdown")
	}
}

func TestServerReportsListenErrors(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close() //nolint:errcheck // the test ends here either way

	server := api.NewServer(logging.Discard(), listener.Addr().String(), testRouter(t, nil, nil, nil))

	if err := server.Run(context.Background()); err == nil {
		t.Fatal("Run() error = nil, want the address-in-use error")
	}
}

var (
	// errLibvirtUnreachable stands in for a real libvirt dial failure.
	errLibvirtUnreachable = errors.New("libvirt is unreachable")
	// errDBGone stands in for a real store failure.
	errDBGone = errors.New("database is gone")
)

type failingUsers struct{}

func (failingUsers) CountUsers(context.Context) (int64, error) {
	return 0, errDBGone
}

type healthBody struct {
	Status string `json:"status"`
}

type readyBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

type errorBody struct {
	Code      string           `json:"code"`
	Message   string           `json:"message"`
	Details   []api.FieldError `json:"details"`
	RequestID string           `json:"request_id"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

func decodeError(t *testing.T, body []byte) errorEnvelope {
	t.Helper()

	var envelope errorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error envelope from %q: %v", body, err)
	}
	return envelope
}

// testRouter builds a router without a web console.
func testRouter(t *testing.T, logger *slog.Logger, checks []api.Check, svc *system.Service, origins ...[]string) *gin.Engine {
	t.Helper()

	var allowed []string
	if len(origins) > 0 {
		allowed = origins[0]
	}

	return testRouterWith(t, logger, checks, svc, allowed, nil)
}

// testRouterWith builds a router with every option the handler takes.
func testRouterWith(
	t *testing.T,
	logger *slog.Logger,
	checks []api.Check,
	svc *system.Service,
	origins []string,
	console http.Handler,
) *gin.Engine {
	t.Helper()

	if logger == nil {
		logger = logging.Discard()
	}
	if svc == nil {
		svc = system.New(logger, "test", "fake", nil, nil)
	}

	handler := api.NewHandler(api.Options{
		Logger:         logger,
		GinMode:        gin.TestMode,
		System:         svc,
		ReadyChecks:    checks,
		AllowedOrigins: origins,
		Console:        console,
	})
	return handler.Router()
}

// call issues a request against a router and returns the recorder plus the
// response for header assertions.
func call(t *testing.T, router *gin.Engine, method, path string) (*httptest.ResponseRecorder, *http.Response) {
	t.Helper()

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, path, http.NoBody))

	response := recorder.Result()
	return recorder, response
}

// testLogger returns a JSON logger writing into the returned buffer.
func testLogger() (*slog.Logger, *bytes.Buffer) {
	buffer := &bytes.Buffer{}
	return logging.New(slog.LevelDebug, buffer), buffer
}

func waitUntilHealthy(t *testing.T, url string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(url) //nolint:noctx // short-lived liveness probe
		if err == nil {
			response.Body.Close() //nolint:errcheck // nothing to read
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s never became ready", url)
}
