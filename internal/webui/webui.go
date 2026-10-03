// Package webui embeds the built React console into the kumod binary. The SPA
// is served at the root, with a fallback to index.html for client-side routes.
//
// See docs/design/05-roadmap.md M0.6.
package webui

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

//go:embed all:dist
var dist embed.FS

const (
	indexFile = "index.html"
	// assetPrefix is where Vite writes hashed bundles; they are immutable.
	assetPrefix = "assets/"
)

// errNotBuilt is returned when the binary was compiled without a console.
var errNotBuilt = errors.New("web console is not built")

// Handler serves the embedded console.
type Handler struct {
	logger *slog.Logger
	fsys   fs.FS
	cache  sync.Map // name -> []byte
}

// New returns a handler for the console embedded in the binary. A binary built
// without `make web-build` is valid: the handler then answers with a 404 that
// says how to fix it, so the API stays usable on its own.
func New(logger *slog.Logger) (*Handler, error) {
	fsys, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}

	return newHandler(logger, fsys), nil
}

// newHandler serves fsys, which lets the tests run against an in-memory console
// without embedding one.
func newHandler(logger *slog.Logger, fsys fs.FS) *Handler {
	return &Handler{logger: logger, fsys: fsys}
}

// Built reports whether a console was embedded.
func (h *Handler) Built() bool {
	_, ok := h.read(context.Background(), indexFile)
	return ok
}

// ServeHTTP serves the console for GET and HEAD.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	name := consolePath(r.URL.Path)
	if name == "" {
		h.notFound(w, r)
		return
	}

	if data, ok := h.read(r.Context(), name); ok {
		h.serve(w, r, name, data)
		return
	}

	// Vite writes hashed file names, so a missing asset is a stale tab or a
	// broken build and must not be answered with HTML. Everything else is a
	// client-side route: React Router owns it from there.
	if name != indexFile && isAssetPath(name) {
		h.notFound(w, r)
		return
	}

	index, ok := h.read(r.Context(), indexFile)
	if !ok {
		h.logger.WarnContext(r.Context(), "webui: no console embedded", "error", errNotBuilt)
		http.Error(w, "the web console was not built; run `make build`", http.StatusNotFound)
		return
	}

	h.serve(w, r, indexFile, index)
}

// notFound answers a missing file. It stays HTML on purpose: the browser asked
// for a static asset, and a JSON envelope would only be noise in the console.
func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
}

// serve writes data with the content type implied by name. ServeContent also
// gives us Range and HEAD support, which the console does not need but curl
// and proxies assume.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, name string, data []byte) {
	if strings.HasPrefix(name, assetPrefix) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// The shell must not outlive a deploy, so it is revalidated.
		w.Header().Set("Cache-Control", "no-cache")
	}

	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// read returns the contents of name from the embedded console, caching it after
// the first read. A missing or unreadable file is reported as not found; the
// reason is logged, never returned.
func (h *Handler) read(ctx context.Context, name string) ([]byte, bool) {
	if cached, ok := h.cache.Load(name); ok {
		data, isBytes := cached.([]byte)
		return data, isBytes
	}

	file, err := h.fsys.Open(name)
	if err != nil {
		return nil, false
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		return nil, false
	}

	data, err := io.ReadAll(file)
	if err != nil {
		h.logger.ErrorContext(ctx, "webui: failed to read embedded file", "file", name, "error", err)
		return nil, false
	}

	h.cache.Store(name, data)
	return data, true
}

// consolePath maps a URL path to a path inside the console, or "" when the
// request must not touch the filesystem at all. The URL is cleaned before use
// so that "..", "//" and friends cannot escape the embedded FS.
func consolePath(urlPath string) string {
	if strings.Contains(urlPath, "..") {
		return ""
	}

	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	switch name {
	case "", ".":
		return indexFile
	default:
		if !fs.ValidPath(name) {
			return ""
		}
		return name
	}
}

// isAssetPath reports whether a path looks like a file rather than a
// client-side route, which is decided by the presence of an extension.
func isAssetPath(name string) bool {
	return strings.Contains(path.Base(name), ".")
}
