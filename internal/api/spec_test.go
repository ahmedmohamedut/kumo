package api_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The API contract lives in two places that must not drift: api/openapi.yaml is
// what the console types are generated from, and docs/design/01-api.md is what
// the project agreed on. This test reads both and fails when they disagree,
// which is cheaper than finding the drift from a missing endpoint in M2.
const (
	openapiPath = "../../api/openapi.yaml"
	designPath  = "../../docs/design/01-api.md"
	apiPrefix   = "/api/v1"
)

// outsideV1 are endpoints 01-api.md mentions but that are explicitly not part
// of the v1 contract yet: a multipart upload marked optional for M3b, and the
// SSE stream under "Realtime (later)". Adding them to the spec without an
// implementation would make the console generate calls to nothing.
var outsideV1 = []string{"/images/upload", "/events"}

// httpMethods are the keys of a Path Item Object that carry an operation; the
// other keys (parameters, summary, servers, …) are not routes.
var httpMethods = []string{"get", "post", "put", "patch", "delete"}

// openapiDoc is the subset of the spec this test needs.
type openapiDoc struct {
	OpenAPI string `yaml:"openapi"`
	Info    struct {
		Title   string `yaml:"title"`
		Version string `yaml:"version"`
	} `yaml:"info"`
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Components struct {
		Schemas         map[string]yaml.Node `yaml:"schemas"`
		Parameters      map[string]yaml.Node `yaml:"parameters"`
		Responses       map[string]yaml.Node `yaml:"responses"`
		SecuritySchemes map[string]yaml.Node `yaml:"securitySchemes"`
	} `yaml:"components"`
}

func TestOpenAPISpec(t *testing.T) {
	t.Parallel()

	doc := readSpec(t)
	documented := documentedEndpoints(t)

	t.Run("has an identity", func(t *testing.T) {
		t.Parallel()

		if doc.OpenAPI != "3.0.3" {
			t.Errorf("openapi = %q, want 3.0.3", doc.OpenAPI)
		}
		if doc.Info.Title == "" || doc.Info.Version == "" {
			t.Errorf("info = %+v, want a title and a version", doc.Info)
		}
		if len(doc.Components.SecuritySchemes) == 0 {
			t.Error("no securitySchemes; every /api/v1 route is JWT protected")
		}
		if _, ok := doc.Components.Schemas["ErrorResponse"]; !ok {
			t.Error("no ErrorResponse schema; every non-2xx uses that envelope")
		}
	})

	t.Run("every endpoint in the design doc is described", func(t *testing.T) {
		t.Parallel()

		for _, endpoint := range documented {
			if slices.Contains(outsideV1, endpoint.Path) {
				continue
			}

			methods, ok := doc.Paths[endpoint.Path]
			if !ok {
				t.Errorf("%s %s is in 01-api.md but missing from openapi.yaml", endpoint.Method, endpoint.Path)
				continue
			}
			if _, ok := methods[endpoint.Method]; !ok {
				t.Errorf("%s %s is in 01-api.md but missing from openapi.yaml", endpoint.Method, endpoint.Path)
			}
		}
	})

	t.Run("every endpoint in the spec is in the design doc", func(t *testing.T) {
		t.Parallel()

		for specPath, operations := range doc.Paths {
			for _, method := range httpMethods {
				if _, ok := operations[method]; !ok {
					continue
				}
				if !slices.Contains(documented, endpoint{Method: method, Path: specPath}) {
					t.Errorf("%s %s is in openapi.yaml but not in 01-api.md; document it or drop it",
						method, specPath)
				}
			}
		}
	})

	t.Run("endpoints left out of v1 are deliberate", func(t *testing.T) {
		t.Parallel()

		for _, path := range outsideV1 {
			if _, ok := doc.Paths[path]; ok {
				t.Errorf("%s is in openapi.yaml but is not part of the v1 contract", path)
			}
		}
	})

	t.Run("every $ref resolves", func(t *testing.T) {
		t.Parallel()

		defined := map[string]map[string]yaml.Node{
			"schemas":         doc.Components.Schemas,
			"parameters":      doc.Components.Parameters,
			"responses":       doc.Components.Responses,
			"securitySchemes": doc.Components.SecuritySchemes,
		}

		for _, ref := range collectRefs(readYAML(t, openapiPath)) {
			kind, name, ok := cutRef(ref)
			if !ok {
				t.Errorf("ref %q does not point at a component", ref)
				continue
			}
			components, known := defined[kind]
			if !known {
				t.Errorf("ref %q uses an unknown component kind %q", ref, kind)
				continue
			}
			if _, ok := components[name]; !ok {
				t.Errorf("ref %q has no definition in components.%s", ref, kind)
			}
		}
	})
}

// endpoint is one row of a table in 01-api.md, normalised to the spec's view of
// the API: no query string and no /api/v1 prefix, because that is the server
// URL in the spec.
type endpoint struct {
	Method string
	Path   string
}

// rowPattern matches a table row that starts with a method and a quoted path,
// e.g. "| POST | `/key-pairs/import` | Import `{name, public_key}` |".
var rowPattern = regexp.MustCompile("^\\|\\s*(GET|POST|PUT|PATCH|DELETE)\\s*\\|\\s*`([^`]+)`")

// documentedEndpoints extracts every endpoint from the tables of 01-api.md.
func documentedEndpoints(t *testing.T) []endpoint {
	t.Helper()

	content, err := os.ReadFile(designPath) //nolint:gosec // fixed path inside the repo
	if err != nil {
		t.Fatalf("read %s: %v", designPath, err)
	}

	var endpoints []endpoint
	for _, line := range strings.Split(string(content), "\n") {
		match := rowPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		path, _, _ := strings.Cut(match[2], "?")
		path = strings.TrimSuffix(path, "/")
		path = strings.TrimPrefix(path, apiPrefix)
		if path == "/healthz" || path == "/readyz" {
			// Health checks live outside /api/v1; the router serves them.
			continue
		}

		endpoints = append(endpoints, endpoint{Method: strings.ToLower(match[1]), Path: path})
	}

	if len(endpoints) < 20 {
		t.Fatalf("found only %d endpoints in %s; the parsing broke", len(endpoints), designPath)
	}

	slices.SortFunc(endpoints, func(a, b endpoint) int {
		if a.Path == b.Path {
			return strings.Compare(a.Method, b.Method)
		}
		return strings.Compare(a.Path, b.Path)
	})

	return endpoints
}

func readSpec(t *testing.T) openapiDoc {
	t.Helper()

	var doc openapiDoc
	if err := yaml.Unmarshal(readYAML(t, openapiPath), &doc); err != nil {
		t.Fatalf("parse %s: %v", openapiPath, err)
	}
	return doc
}

func readYAML(t *testing.T, path string) []byte {
	t.Helper()

	content, err := os.ReadFile(path) //nolint:gosec // fixed path inside the repo
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return content
}

// collectRefs returns every `#/components/...` reference in the document.
func collectRefs(content []byte) []string {
	return regexp.MustCompile(`#/components/[A-Za-z]+/[A-Za-z0-9_]+`).FindAllString(string(content), -1)
}

// cutRef splits `#/components/<kind>/<name>`.
func cutRef(ref string) (kind, name string, ok bool) {
	rest, found := strings.CutPrefix(ref, "#/components/")
	if !found {
		return "", "", false
	}

	kind, name, found = strings.Cut(rest, "/")
	return kind, name, found && kind != "" && name != ""
}
