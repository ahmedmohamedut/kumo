package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/service/image"
)

// assertFieldError checks the envelope names the field, not just that the
// request was refused: the console marks the input from details, so a refusal
// that does not say which field is a refusal the form cannot render
// (AGENTS.md, frontend conventions).
func assertFieldError(t *testing.T, recorder *httptest.ResponseRecorder, field string) {
	t.Helper()

	var envelope errorEnvelope
	decode(t, recorder, &envelope)

	if envelope.Error.Code != "invalid_argument" {
		t.Errorf("code = %q, want invalid_argument (body %s)", envelope.Error.Code, recorder.Body)
	}
	for _, detail := range envelope.Error.Details {
		if detail.Field == field {
			return
		}
	}
	t.Errorf("details = %+v, want one naming %q", envelope.Error.Details, field)
}

// engineImageBody adds the three M7.2 fields to the shape the other image
// tests decode. The pointers are how a test tells null from an empty string,
// which is what a user image must report for both engine fields.
type engineImageBody struct {
	imageBody
	Kind          string  `json:"kind"`
	Engine        *string `json:"engine"`
	EngineVersion *string `json:"engine_version"`
}

func TestImportAnEngineImageThroughTheAPI(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)
	startWorker(t, ir.svc, ir.store)
	login := setupUserAndLogin(t, ir.router)
	url, sum := tinyImageServer(t)

	accepted := doWithToken(t, ir.router, http.MethodPost, "/api/v1/images/import", fmt.Sprintf(
		`{"name":"postgres-18","source_url":%q,"checksum":{"algorithm":"sha256","value":%q},`+
			`"kind":"engine","engine":"postgres","engine_version":"18","min_disk_gb":6}`, url, sum),
		login.access)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("POST /images/import status = %d, want 202 (body %s)", accepted.Code, accepted.Body)
	}

	var imported engineImageBody
	decode(t, accepted, &imported)
	if imported.Kind != "engine" {
		t.Errorf("kind = %q, want engine", imported.Kind)
	}
	if imported.Engine == nil || *imported.Engine != "postgres" {
		t.Errorf("engine = %v, want postgres", imported.Engine)
	}
	if imported.EngineVersion == nil || *imported.EngineVersion != "18" {
		t.Errorf("engine_version = %v, want 18", imported.EngineVersion)
	}

	waitForTaskState(t, ir.router, login.access, imported.TaskID, "succeeded")

	// Reachable by id, because an operator debugging a database needs the row.
	detail := doWithToken(t, ir.router, http.MethodGet, "/api/v1/images/"+imported.ID, "", login.access)
	if detail.Code != http.StatusOK {
		t.Fatalf("GET /images/{id} status = %d, want 200 (body %s)", detail.Code, detail.Body)
	}
	var got engineImageBody
	decode(t, detail, &got)
	if got.Kind != "engine" || got.State != string(image.StateAvailable) {
		t.Errorf("detail kind/state = %q/%q, want engine/available", got.Kind, got.State)
	}

	// But absent from the list the Images pages read, which is the point.
	list := doWithToken(t, ir.router, http.MethodGet, "/api/v1/images", "", login.access)
	if list.Code != http.StatusOK {
		t.Fatalf("GET /images status = %d, want 200 (body %s)", list.Code, list.Body)
	}
	var page imagePage
	decode(t, list, &page)
	for _, item := range page.Items {
		if item.ID == imported.ID {
			t.Errorf("GET /images returned the engine image %s", imported.ID)
		}
	}
	if len(page.Items) != 0 {
		t.Errorf("GET /images returned %d items, want none: the only image is an engine image", len(page.Items))
	}
}

// An ordinary import says nothing about kind, which is what every client
// written before M7.2 sends.
func TestImportWithoutAKindIsAUserImage(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)
	login := setupUserAndLogin(t, ir.router)
	url, sum := tinyImageServer(t)

	accepted := doWithToken(t, ir.router, http.MethodPost, "/api/v1/images/import", fmt.Sprintf(
		`{"name":"ubuntu-2404","source_url":%q,"checksum":{"algorithm":"sha256","value":%q}}`, url, sum),
		login.access)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", accepted.Code, accepted.Body)
	}

	var imported engineImageBody
	decode(t, accepted, &imported)
	if imported.Kind != "user" {
		t.Errorf("kind = %q, want user", imported.Kind)
	}
	if imported.Engine != nil || imported.EngineVersion != nil {
		t.Errorf("engine/engine_version = %v/%v, want null on a user image",
			imported.Engine, imported.EngineVersion)
	}
}

func TestImportRejectsBadEngineFields(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)
	login := setupUserAndLogin(t, ir.router)
	url, sum := tinyImageServer(t)

	tests := []struct {
		name  string
		extra string
		field string
	}{
		{"an unknown kind", `"kind":"golden"`, "kind"},
		{"an engine image without an engine", `"kind":"engine","engine_version":"18"`, "engine"},
		{"an engine image without a version", `"kind":"engine","engine":"postgres"`, "engine_version"},
		{"an unsupported engine", `"kind":"engine","engine":"mysql","engine_version":"8"`, "engine"},
		{"a minor version", `"kind":"engine","engine":"postgres","engine_version":"18.6"`, "engine_version"},
		{"a user image claiming an engine", `"engine":"postgres"`, "engine"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body := fmt.Sprintf(
				`{"name":"candidate","source_url":%q,"checksum":{"algorithm":"sha256","value":%q},%s}`,
				url, sum, tc.extra)
			response := doWithToken(t, ir.router, http.MethodPost,
				"/api/v1/images/import", body, login.access)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", response.Code, response.Body)
			}
			assertFieldError(t, response, tc.field)
		})
	}
}
