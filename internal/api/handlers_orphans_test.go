package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// orphansBody is the body of GET /api/v1/system/orphans, the Orphan schema of
// api/openapi.yaml.
type orphansBody struct {
	Items []struct {
		DomainName string `json:"domain_name"`
		UUID       string `json:"uuid"`
		State      string `json:"state"`
		FirstSeen  string `json:"first_seen"`
		LastSeen   string `json:"last_seen"`
	} `json:"items"`
}

// orphans reads the report and decodes it.
func orphans(t *testing.T, ir instanceRouter, token string) (*orphansBody, int) {
	t.Helper()

	recorder := doWithToken(t, ir.router, http.MethodGet, "/api/v1/system/orphans", "", token)
	if recorder.Code != http.StatusOK {
		return nil, recorder.Code
	}

	var body orphansBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the orphan report: %v", err)
	}
	return &body, recorder.Code
}

// TestOrphansIsEmptyBeforeAPass is what a freshly started node answers. An empty
// list is a real answer rather than an error: the endpoint reports what the last
// pass saw, and a node that has not completed one has seen nothing.
func TestOrphansIsEmptyBeforeAPass(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)

	body, code := orphans(t, ir, ir.tokens.access)
	if code != http.StatusOK {
		t.Fatalf("GET /system/orphans = %d, want 200", code)
	}
	if len(body.Items) != 0 {
		t.Errorf("items = %d, want 0 before any pass", len(body.Items))
	}
	if body.Items == nil {
		t.Error("items is null, want an empty array: a client should not have to special-case it")
	}
}

// TestOrphansReportsADomainWithNoRow is the endpoint over rule 3 of
// docs/design/00-architecture.md §4: a domain running under an instance id that
// no row claims is reported, never removed.
func TestOrphansReportsADomainWithNoRow(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	launched := launchedInstance(t, ir, availableImageID(t, ir), "web-01")

	// The row goes while the domain stays, which is what a crash between the
	// define and the insert leaves behind.
	if _, err := ir.store.DeleteInstance(t.Context(),
		sqlc.DeleteInstanceParams{ID: launched.ID}); err != nil {
		t.Fatalf("DeleteInstance(): %v", err)
	}
	if _, err := ir.instances.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}

	body, code := orphans(t, ir, ir.tokens.access)
	if code != http.StatusOK {
		t.Fatalf("GET /system/orphans = %d, want 200", code)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(body.Items))
	}

	got := body.Items[0]
	if got.DomainName != launched.ID {
		t.Errorf("domain_name = %q, want %q", got.DomainName, launched.ID)
	}
	if got.State != "running" {
		t.Errorf("state = %q, want running", got.State)
	}
	if got.UUID == "" {
		t.Error("uuid is empty, which is what identifies the domain to virsh")
	}
	if got.FirstSeen == "" || got.LastSeen == "" {
		t.Errorf("first_seen = %q and last_seen = %q, want both set", got.FirstSeen, got.LastSeen)
	}
}

// TestOrphansIsAdminOnly keeps host-level detail out of a viewer's console. An
// orphan names a guest the control plane does not manage, and the report is the
// thing an operator acts on by hand.
func TestOrphansIsAdminOnly(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)

	viewer, err := ir.signer.IssueAccessToken("01J0000000000000000000000V", auth.RoleViewer)
	if err != nil {
		t.Fatalf("IssueAccessToken(): %v", err)
	}

	recorder := doWithToken(t, ir.router, http.MethodGet, "/api/v1/system/orphans", "", viewer.Token)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("GET /system/orphans as a viewer = %d, want 403", recorder.Code)
	}
}

// TestOrphansNeedsAToken is the rule for every route but login, refresh and the
// health probes (docs/design/01-api.md).
func TestOrphansNeedsAToken(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)

	recorder := do(t, ir.router, http.MethodGet, "/api/v1/system/orphans", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("GET /system/orphans without a token = %d, want 401", recorder.Code)
	}
}

// TestOrphansDoesNotListAManagedInstance is the other half of the report: an
// instance Kumo knows about is not an orphan, however it is running.
func TestOrphansDoesNotListAManagedInstance(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	launchedInstance(t, ir, availableImageID(t, ir), "web-02")

	if _, err := ir.instances.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}

	body, code := orphans(t, ir, ir.tokens.access)
	if code != http.StatusOK {
		t.Fatalf("GET /system/orphans = %d, want 200", code)
	}
	if len(body.Items) != 0 {
		t.Errorf("items = %d, want 0: the instance has a row", len(body.Items))
	}
}
