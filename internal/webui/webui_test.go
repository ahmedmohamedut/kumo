package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ahmedmohamedut/kumo/internal/logging"
)

func TestServeHTTP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		method      string
		target      string
		wantStatus  int
		wantBody    string
		wantType    string
		wantCache   string
		wantMissing bool
	}{
		{
			name:       "the root serves index.html",
			method:     http.MethodGet,
			target:     "/",
			wantStatus: http.StatusOK,
			wantBody:   "<!doctype html>",
			wantType:   "text/html",
			wantCache:  "no-cache",
		},
		{
			name:       "index.html can be requested directly",
			method:     http.MethodGet,
			target:     "/index.html",
			wantStatus: http.StatusOK,
			wantBody:   "<!doctype html>",
			wantType:   "text/html",
			wantCache:  "no-cache",
		},
		{
			name:       "HEAD on the root works",
			method:     http.MethodHead,
			target:     "/",
			wantStatus: http.StatusOK,
			wantType:   "text/html",
		},
		{
			name:       "a hashed asset is served and cached forever",
			method:     http.MethodGet,
			target:     "/assets/index-abc123.js",
			wantStatus: http.StatusOK,
			wantBody:   "console",
			wantType:   "text/javascript",
			wantCache:  "public, max-age=31536000, immutable",
		},
		{
			name:       "a client-side route falls back to the shell",
			method:     http.MethodGet,
			target:     "/instances/i-0123456789abcdef0",
			wantStatus: http.StatusOK,
			wantBody:   "<!doctype html>",
			wantType:   "text/html",
			wantCache:  "no-cache",
		},
		{
			name:       "a nested client-side route falls back to the shell",
			method:     http.MethodGet,
			target:     "/images/img-0123456789abcdef0/settings",
			wantStatus: http.StatusOK,
			wantBody:   "<!doctype html>",
		},
		{
			name:        "a missing asset is a 404, not the shell",
			method:      http.MethodGet,
			target:      "/assets/index-stale.js",
			wantStatus:  http.StatusNotFound,
			wantMissing: true,
		},
		{
			name:        "a missing file with an extension is a 404",
			method:      http.MethodGet,
			target:      "/favicon.svg",
			wantStatus:  http.StatusNotFound,
			wantMissing: true,
		},
		{
			name:        "a traversal attempt is a 404",
			method:      http.MethodGet,
			target:      "/../../etc/passwd",
			wantStatus:  http.StatusNotFound,
			wantMissing: true,
		},
		{
			name:        "a traversal attempt inside a client route is a 404",
			method:      http.MethodGet,
			target:      "/instances/../../../etc/passwd",
			wantStatus:  http.StatusNotFound,
			wantMissing: true,
		},
		{
			name:       "POST is rejected",
			method:     http.MethodPost,
			target:     "/",
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			testHandler(t).ServeHTTP(recorder, httptest.NewRequest(tt.method, tt.target, http.NoBody))

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantMissing {
				return
			}
			if tt.wantBody != "" && !strings.Contains(recorder.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", recorder.Body.String(), tt.wantBody)
			}
			if tt.wantType != "" && !strings.Contains(recorder.Header().Get("Content-Type"), tt.wantType) {
				t.Errorf("Content-Type = %q, want it to contain %q", recorder.Header().Get("Content-Type"), tt.wantType)
			}
			if tt.wantCache != "" && recorder.Header().Get("Cache-Control") != tt.wantCache {
				t.Errorf("Cache-Control = %q, want %q", recorder.Header().Get("Cache-Control"), tt.wantCache)
			}
		})
	}
}

func TestServesRepeatedRequestsFromTheCache(t *testing.T) {
	t.Parallel()

	handler := testHandler(t)

	for range 3 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/instances/i-0123456789abcdef0", http.NoBody))

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "<!doctype html>") {
			t.Fatalf("body = %q, want the shell", recorder.Body.String())
		}
	}
}

func TestWithoutABuiltConsole(t *testing.T) {
	t.Parallel()

	handler := newHandler(logging.Discard(), fstest.MapFS{"README": {Data: []byte("nothing here")}})

	if handler.Built() {
		t.Error("Built() = true, want false when index.html is missing")
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", http.NoBody))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "make build") {
		t.Errorf("body = %q, want it to explain how to build the console", recorder.Body.String())
	}
}

func TestTheEmbeddedConsoleIsBuilt(t *testing.T) {
	t.Parallel()

	handler, err := New(logging.Discard())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !handler.Built() {
		t.Fatal("Built() = false: the binary has no console; run `make web-build`")
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", http.NoBody))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

func TestConsolePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		urlPath string
		want    string
	}{
		{urlPath: "/", want: indexFile},
		{urlPath: "", want: indexFile},
		{urlPath: "//", want: indexFile},
		{urlPath: "/index.html", want: "index.html"},
		{urlPath: "/instances", want: "instances"},
		{urlPath: "/instances/", want: "instances"},
		{urlPath: "/instances//i-1", want: "instances/i-1"},
		{urlPath: "/assets/index-abc.js", want: "assets/index-abc.js"},
		{urlPath: "/../secrets", want: ""},
		{urlPath: "/instances/../../secrets", want: ""},
	}

	for _, tt := range tests {
		if got := consolePath(tt.urlPath); got != tt.want {
			t.Errorf("consolePath(%q) = %q, want %q", tt.urlPath, got, tt.want)
		}
	}
}

func TestIsAssetPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		{name: "assets/index-abc.js", want: true},
		{name: "favicon.svg", want: true},
		{name: "index.html", want: true},
		{name: "instances", want: false},
		{name: "instances/i-0123456789abcdef0", want: false},
	}

	for _, tt := range tests {
		if got := isAssetPath(tt.name); got != tt.want {
			t.Errorf("isAssetPath(%q) = %t, want %t", tt.name, got, tt.want)
		}
	}
}

// testHandler returns a handler over a small in-memory console.
func testHandler(t *testing.T) *Handler {
	t.Helper()

	return newHandler(logging.Discard(), fstest.MapFS{
		"index.html":              {Data: []byte("<!doctype html><title>Kumo</title>")},
		"assets/index-abc123.js":  {Data: []byte("console")},
		"assets/index-abc123.css": {Data: []byte("body{}")},
	})
}
