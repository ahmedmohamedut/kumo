package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
)

// The VM behind a DB instance is not the caller's to change: stopping or
// terminating it would take the database down without its own service knowing
// (docs/design/07-databases.md section 4). The refusal is a 409 that names the
// DB instance, so an operator is not left guessing which resource owns the VM.
func TestInstanceWritesRefuseAManagedInstance(t *testing.T) {
	t.Parallel()

	const (
		dbID       = "db-0a1b2c3d4e5f60718"
		identifier = "orders"
	)

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)
	launched := launchedInstance(t, ir, imageID, "orders")
	base := "/api/v1/instances/" + launched.ID

	ctx := t.Context()
	if _, err := ir.store.DB().ExecContext(ctx,
		`UPDATE instances SET managed_by = ? WHERE id = ?`,
		string(instance.ManagedByDatabases), launched.ID); err != nil {
		t.Fatalf("mark managed: %v", err)
	}
	if _, err := ir.store.DB().ExecContext(ctx,
		`INSERT INTO db_instances (id, identifier, state, engine, engine_version, image_id,
		   instance_id, instance_type, storage_gb, admin_username, database_name,
		   created_at, updated_at)
		 VALUES (?, ?, 'available', 'postgres', '18', ?, ?, 'kumo.small', 20, 'app', 'orders',
		   '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		dbID, identifier, imageID, launched.ID); err != nil {
		t.Fatalf("insert db_instances: %v", err)
	}

	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, base + "/start", ""},
		{http.MethodPost, base + "/stop", ""},
		{http.MethodPost, base + "/stop?force=true", ""},
		{http.MethodPost, base + "/reboot", ""},
		{http.MethodDelete, base, ""},
		{http.MethodPatch, base, `{"name":"renamed"}`},
	} {
		recorder := doWithToken(t, ir.router, test.method, test.path, test.body, ir.tokens.access)
		if recorder.Code != http.StatusConflict {
			t.Errorf("%s %s status = %d, want 409 (body %s)",
				test.method, test.path, recorder.Code, recorder.Body)
			continue
		}
		body := recorder.Body.String()
		for _, want := range []string{dbID, identifier, "/api/v1/rds/db-instances"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s %s body = %s, want it to mention %q", test.method, test.path, body, want)
			}
		}
	}

	// Nothing moved: a refused action must not have queued work behind itself.
	if got := stateOf(t, ir, launched.ID); got != string(instance.StateRunning) {
		t.Errorf("state = %q after the refusals, want running", got)
	}

	// Hidden from the list the Instances pages read.
	list := doWithToken(t, ir.router, http.MethodGet, "/api/v1/instances", "", ir.tokens.access)
	if list.Code != http.StatusOK {
		t.Fatalf("GET /instances status = %d (body %s)", list.Code, list.Body)
	}
	if strings.Contains(list.Body.String(), launched.ID) {
		t.Errorf("GET /instances returned the managed instance %s: %s", launched.ID, list.Body)
	}
}

// A viewer is refused before the managed check is reached: the role gate comes
// first, so the answer is 403 and not a 409 that leaks which VM is a database.
func TestInstanceWritesRefuseAViewerBeforeTheManagedCheck(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)
	launched := launchedInstance(t, ir, imageID, "orders")

	if _, err := ir.store.DB().ExecContext(t.Context(),
		`UPDATE instances SET managed_by = ? WHERE id = ?`,
		string(instance.ManagedByDatabases), launched.ID); err != nil {
		t.Fatalf("mark managed: %v", err)
	}

	viewer, err := ir.signer.IssueAccessToken("01J0000000000000000000000V", auth.RoleViewer)
	if err != nil {
		t.Fatalf("IssueAccessToken(): %v", err)
	}

	recorder := doWithToken(t, ir.router, http.MethodDelete,
		"/api/v1/instances/"+launched.ID, "", viewer.Token)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("viewer DELETE status = %d, want 403 (body %s)", recorder.Code, recorder.Body)
	}
}
