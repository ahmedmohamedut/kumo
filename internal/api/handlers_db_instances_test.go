package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/dbinstance"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// dbInstanceBody is the response shape api/openapi.yaml promises for a DB
// instance. It is written out rather than reusing the service type, so a field
// that silently disappears from the JSON fails here.
type dbInstanceBody struct {
	ID            string  `json:"id"`
	Identifier    string  `json:"identifier"`
	State         string  `json:"state"`
	StateReason   *string `json:"state_reason"`
	Engine        string  `json:"engine"`
	EngineVersion string  `json:"engine_version"`
	ImageID       string  `json:"image_id"`
	InstanceID    string  `json:"instance_id"`
	InstanceType  string  `json:"instance_type"`
	VCPUs         int     `json:"vcpus"`
	MemoryMiB     int     `json:"memory_mib"`
	StorageGB     int     `json:"storage_gb"`
	Port          int     `json:"port"`
	AdminUsername string  `json:"admin_username"`
	DatabaseName  string  `json:"database_name"`
	Endpoint      *string `json:"endpoint"`
	TaskID        *string `json:"task_id"`
}

type dbInstancePage struct {
	Items         []dbInstanceBody `json:"items"`
	NextPageToken *string          `json:"next_page_token"`
}

// dbRouter is the router under test plus what a test needs below the API.
type dbRouter struct {
	router *gin.Engine
	store  *store.Store
	hv     *fake.Fake
	images *image.Service
	signer *auth.Signer
	tokens session
}

// newDBRouter builds a router with the databases module on, a fake hypervisor
// and a readiness probe that answers: no test listens on a port, so the probe
// is where a test says whether the engine is up.
func newDBRouter(t *testing.T) dbRouter {
	t.Helper()
	return newDBRouterWith(t, func(context.Context, string) error { return nil })
}

func newDBRouterWith(t *testing.T, probe dbinstance.Prober) dbRouter {
	t.Helper()

	logger := logging.Discard()
	db := storetest.New(t)

	signer, err := auth.NewSigner([]byte(strings.Repeat("d", 32)), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewSigner(): %v", err)
	}
	authSvc, err := authservice.New(authservice.Options{
		Logger: logger, Store: db, Signer: signer, RefreshTTL: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("auth.New(): %v", err)
	}
	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}

	hv := fake.New(fake.Config{})
	if err := hv.EnsureInfra(t.Context(), hypervisor.InfraConfig{
		ImagesPool: imagesPool, InstancesPool: instancesPool, Network: networkName,
		DataDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("EnsureInfra(): %v", err)
	}
	seedDefaultNetwork(t, db, hv)

	images, err := image.New(image.Options{
		Logger:     logger,
		Store:      db,
		Hypervisor: hv,
		Tasks:      tasks,
		Downloader: imagefetch.New(imagefetch.Options{
			AllowPrivateNetworks: true,
			MaxBytes:             4 << 20,
		}),
		MaxBytes:   4 << 20,
		ImagesPool: imagesPool,
		TempDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("image.New(): %v", err)
	}
	types, err := instancetype.New(instancetype.Options{Logger: logger, Store: db})
	if err != nil {
		t.Fatalf("instancetype.New(): %v", err)
	}
	pairs, err := keypair.New(keypair.Options{Logger: logger, Store: db})
	if err != nil {
		t.Fatalf("keypair.New(): %v", err)
	}
	sys := system.New(logger, "test", "fake", db, hv, db)
	limits := capacity.Limits{MemoryReserveMiB: 1024, VCPUOvercommit: 4}
	sys.SetLimits(limits)

	instances, err := instance.New(instance.Options{
		Logger: logger, Store: db, Hypervisor: hv, Volumes: hv, Tasks: tasks,
		Images: images, Types: types, KeyPairs: pairs, Capacity: sys,
		InstancesPool: instancesPool, ImagesPool: imagesPool,
		ConsoleDir: t.TempDir(), Network: networkName, Limits: limits,
		PollInterval: time.Millisecond, PollTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("instance.New(): %v", err)
	}

	databases, err := dbinstance.New(dbinstance.Options{
		Logger: logger, Store: db, Hypervisor: hv, Instances: instances, Tasks: tasks,
		InstancesPool: instancesPool,
		Probe:         probe,
		ProbeInterval: time.Millisecond,
		ReadyTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("dbinstance.New(): %v", err)
	}

	handler := api.NewHandler(api.Options{
		Logger:       logger,
		GinMode:      gin.TestMode,
		System:       system.New(logger, "test", "fake", nil, nil, nil),
		Auth:         authSvc,
		Images:       images,
		Instances:    instances,
		DBInstances:  databases,
		Tasks:        tasks,
		Signer:       signer,
		RefreshTTL:   7 * 24 * time.Hour,
		CookieSecure: false,
	})
	router := handler.Router()

	pool := worker.New(worker.Options{
		Logger:       logger,
		Store:        db,
		Registry:     mergeRegistries(images.Handler(), instances.Handler(), databases.Handler()),
		Workers:      1,
		PollInterval: 5 * time.Millisecond,
	})
	if err := pool.Start(t.Context()); err != nil {
		t.Fatalf("worker.Start(): %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := pool.Stop(ctx); err != nil {
			t.Errorf("worker.Stop(): %v", err)
		}
	})

	return dbRouter{
		router: router,
		store:  db,
		hv:     hv,
		images: images,
		signer: signer,
		tokens: setupUserAndLogin(t, router),
	}
}

// engineImageID imports an engine image through the API and waits for it, which
// is how an operator makes a node able to create databases at all (M7.2).
func engineImageID(t *testing.T, dr dbRouter) string {
	t.Helper()

	url, sum := tinyImageServer(t)
	recorder := doWithToken(t, dr.router, http.MethodPost, "/api/v1/images/import", fmt.Sprintf(
		`{"name":"postgres-18","source_url":%q,"checksum":{"algorithm":"sha256","value":%q},`+
			`"os_family":"linux","architecture":"x86_64","default_user":"ubuntu","min_disk_gb":1,`+
			`"kind":"engine","engine":"postgres","engine_version":"18"}`,
		url, sum), dr.tokens.access)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("POST /images/import status = %d, want 202 (body %s)", recorder.Code, recorder.Body)
	}

	var imported imageBody
	decode(t, recorder, &imported)
	waitForTaskState(t, dr.router, dr.tokens.access, imported.TaskID, "succeeded")
	return imported.ID
}

// errNothingListening is what the probe returns in the test where the engine
// never comes up.
var errNothingListening = errors.New("nothing is listening on the port")

// stateAvailable is the state a DB instance reaches when its engine answered.
const stateAvailable = "available"

const createDBInstanceBody = `{"identifier":"orders","engine":"postgres","engine_version":"18",` +
	`"instance_type":"kumo.small","storage_gb":20,"admin_username":"app",` +
	`"admin_password":"sup3r-secret-password","database_name":"orders"}`

// The create is asynchronous and ends available, with the endpoint published
// only once the engine answered.
func TestCreateDBInstanceIsAsynchronousAndEndsAvailable(t *testing.T) {
	t.Parallel()

	dr := newDBRouter(t)
	imageID := engineImageID(t, dr)

	accepted := doWithToken(t, dr.router, http.MethodPost, "/api/v1/rds/db-instances",
		createDBInstanceBody, dr.tokens.access)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("POST /rds/db-instances status = %d, want 202 (body %s)", accepted.Code, accepted.Body)
	}

	var created dbInstanceBody
	decode(t, accepted, &created)
	if created.State != "creating" {
		t.Errorf("State = %q, want creating", created.State)
	}
	if created.TaskID == nil {
		t.Fatal("task_id is null; the console has nothing to follow")
	}
	if want := "/api/v1/tasks/" + *created.TaskID; accepted.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", accepted.Header().Get("Location"), want)
	}
	if created.ImageID != imageID {
		t.Errorf("image_id = %q, want the engine image %q", created.ImageID, imageID)
	}
	if created.Endpoint != nil {
		t.Errorf("endpoint = %q before the guest has an address", *created.Endpoint)
	}

	waitForTaskState(t, dr.router, dr.tokens.access, *created.TaskID, "succeeded")

	got := getDBInstance(t, dr, created.ID)
	if got.State != stateAvailable {
		t.Fatalf("State = %q (%v), want available", got.State, got.StateReason)
	}
	if got.Endpoint == nil || *got.Endpoint != "orders.default" {
		t.Errorf("endpoint = %v, want orders.default", got.Endpoint)
	}
	if got.AdminUsername != "app" || got.DatabaseName != "orders" || got.Port != 5432 {
		t.Errorf("the connection details are %+v", got)
	}
	if got.VCPUs == 0 || got.MemoryMiB == 0 {
		t.Errorf("vcpus = %d and memory_mib = %d, want the preset's", got.VCPUs, got.MemoryMiB)
	}

	// No response ever carries the credential, because none is kept.
	body := accepted.Body.String() + getBody(t, dr, "/api/v1/rds/db-instances/"+created.ID)
	for _, forbidden := range []string{"sup3r-secret-password", "SCRAM", "verifier", "salt"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("a response carries %q", forbidden)
		}
	}

	// And the VM behind it is on no Instances page.
	list := doWithToken(t, dr.router, http.MethodGet, "/api/v1/instances", "", dr.tokens.access)
	if list.Code != http.StatusOK {
		t.Fatalf("GET /instances status = %d", list.Code)
	}
	var instances instancePage
	decode(t, list, &instances)
	if len(instances.Items) != 0 {
		t.Errorf("GET /instances shows %d rows, want none", len(instances.Items))
	}
}

// A node with the module on and no engine image answers 409 with its own code,
// which is what tells the console to explain the import.
func TestCreateDBInstanceWithoutAnEngineImage(t *testing.T) {
	t.Parallel()

	dr := newDBRouter(t)

	recorder := doWithToken(t, dr.router, http.MethodPost, "/api/v1/rds/db-instances",
		createDBInstanceBody, dr.tokens.access)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", recorder.Code, recorder.Body)
	}
	if code := errorCode(t, recorder); code != api.CodeNoEngineImage {
		t.Errorf("error code = %q, want %q", code, api.CodeNoEngineImage)
	}
}

// Every field-level refusal names the field the console marks.
func TestCreateDBInstanceValidation(t *testing.T) {
	t.Parallel()

	dr := newDBRouter(t)
	engineImageID(t, dr)

	tests := []struct {
		name  string
		body  string
		field string
	}{
		{"not a JSON object", `nope`, "body"},
		{"no identifier", `{"instance_type":"kumo.small","admin_username":"app",` +
			`"admin_password":"sup3r-secret-password","database_name":"orders"}`, "identifier"},
		{"an identifier in capitals", strings.Replace(createDBInstanceBody,
			`"identifier":"orders"`, `"identifier":"Orders"`, 1), "identifier"},
		{"a reserved admin role", strings.Replace(createDBInstanceBody,
			`"admin_username":"app"`, `"admin_username":"postgres"`, 1), "admin_username"},
		{"a short password", strings.Replace(createDBInstanceBody,
			`"admin_password":"sup3r-secret-password"`, `"admin_password":"short"`, 1), "admin_password"},
		{"a password that is not printable ASCII", strings.Replace(createDBInstanceBody,
			`"admin_password":"sup3r-secret-password"`, `"admin_password":"passphrase—long"`, 1),
			"admin_password"},
		{"another engine", strings.Replace(createDBInstanceBody,
			`"engine":"postgres"`, `"engine":"mysql"`, 1), "engine"},
		{"storage below the floor", strings.Replace(createDBInstanceBody,
			`"storage_gb":20`, `"storage_gb":1`, 1), "storage_gb"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			recorder := doWithToken(t, dr.router, http.MethodPost, "/api/v1/rds/db-instances",
				tc.body, dr.tokens.access)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body)
			}
			if !strings.Contains(recorder.Body.String(), `"field":"`+tc.field+`"`) {
				t.Errorf("body = %s, want details naming %q", recorder.Body, tc.field)
			}
			// A refused create must never echo the password back.
			if strings.Contains(recorder.Body.String(), "sup3r-secret-password") {
				t.Error("the refusal echoes the password")
			}
		})
	}
}

// The lifecycle endpoints: each one answers 202 with a task, and 409 from a
// state it does not apply to.
func TestDBInstanceLifecycleEndpoints(t *testing.T) {
	t.Parallel()

	dr := newDBRouter(t)
	engineImageID(t, dr)
	created := createAvailableDBInstance(t, dr)
	base := "/api/v1/rds/db-instances/" + created.ID

	// Start does not apply to a database that is available.
	if recorder := doWithToken(t, dr.router, http.MethodPost, base+"/start", "",
		dr.tokens.access); recorder.Code != http.StatusConflict {
		t.Errorf("start while available: status = %d, want 409 (body %s)", recorder.Code, recorder.Body)
	}

	// Reboot does.
	rebooted := doWithToken(t, dr.router, http.MethodPost, base+"/reboot", "", dr.tokens.access)
	if rebooted.Code != http.StatusAccepted {
		t.Fatalf("reboot: status = %d, want 202 (body %s)", rebooted.Code, rebooted.Body)
	}
	var body dbInstanceBody
	decode(t, rebooted, &body)
	if body.State != "rebooting" {
		t.Errorf("State = %q, want rebooting", body.State)
	}
	if body.TaskID == nil {
		t.Fatal("task_id is null after a reboot")
	}
	if want := "/api/v1/tasks/" + *body.TaskID; rebooted.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", rebooted.Header().Get("Location"), want)
	}
	waitForTaskState(t, dr.router, dr.tokens.access, *body.TaskID, "succeeded")

	// Stop, once the reboot's task is done. The DB instance is rebooting until
	// a reconcile pass republishes it, so this asserts the refusal rather than
	// the stop.
	if recorder := doWithToken(t, dr.router, http.MethodPost, base+"/stop", "",
		dr.tokens.access); recorder.Code != http.StatusConflict {
		t.Errorf("stop while rebooting: status = %d, want 409 (body %s)", recorder.Code, recorder.Body)
	}

	// Delete is reachable from every state, and takes the data with it.
	deleted := doWithToken(t, dr.router, http.MethodDelete, base, "", dr.tokens.access)
	if deleted.Code != http.StatusAccepted {
		t.Fatalf("delete: status = %d, want 202 (body %s)", deleted.Code, deleted.Body)
	}
	decode(t, deleted, &body)
	if body.State != "deleting" {
		t.Errorf("State = %q, want deleting", body.State)
	}
	if body.TaskID == nil {
		t.Fatal("task_id is null after a delete")
	}
	waitForTaskState(t, dr.router, dr.tokens.access, *body.TaskID, "succeeded")

	if recorder := doWithToken(t, dr.router, http.MethodGet, base, "",
		dr.tokens.access); recorder.Code != http.StatusNotFound {
		t.Errorf("GET after the delete: status = %d, want 404", recorder.Code)
	}
	// And the data disk is gone with it.
	for _, volume := range dr.hv.Volumes() {
		if strings.Contains(volume.Name, "-data.qcow2") {
			t.Errorf("the data disk %q survived the delete", volume.Name)
		}
	}
}

// Reads are open to both roles; every write is admin only, because creating a
// database claims the vCPUs, memory and disk of the node.
func TestDBInstanceWritesAreAdminOnly(t *testing.T) {
	t.Parallel()

	dr := newDBRouter(t)
	engineImageID(t, dr)
	created := createAvailableDBInstance(t, dr)
	base := "/api/v1/rds/db-instances/" + created.ID

	issued, err := dr.signer.IssueAccessToken("01J00000000000000000000000", auth.RoleViewer)
	if err != nil {
		t.Fatalf("IssueAccessToken(): %v", err)
	}
	viewer := issued.Token

	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/rds/db-instances", createDBInstanceBody},
		{http.MethodPost, base + "/stop", ""},
		{http.MethodPost, base + "/start", ""},
		{http.MethodPost, base + "/reboot", ""},
		{http.MethodDelete, base, ""},
	} {
		recorder := doWithToken(t, dr.router, test.method, test.path, test.body, viewer)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s %s as a viewer: status = %d, want 403 (body %s)",
				test.method, test.path, recorder.Code, recorder.Body)
		}
	}

	for _, path := range []string{"/api/v1/rds/db-instances", base} {
		if recorder := doWithToken(t, dr.router, http.MethodGet, path, "",
			viewer); recorder.Code != http.StatusOK {
			t.Errorf("GET %s as a viewer: status = %d, want 200", path, recorder.Code)
		}
	}

	// And nothing at all without a token.
	if recorder := do(t, dr.router, http.MethodGet, "/api/v1/rds/db-instances", ""); recorder.Code !=
		http.StatusUnauthorized {
		t.Errorf("GET without a token: status = %d, want 401", recorder.Code)
	}
}

// The list is paged, sorted and searchable like every other collection.
func TestListDBInstances(t *testing.T) {
	t.Parallel()

	dr := newDBRouter(t)
	engineImageID(t, dr)

	for _, identifier := range []string{"orders", "billing"} {
		body := strings.Replace(createDBInstanceBody, `"identifier":"orders"`,
			`"identifier":"`+identifier+`"`, 1)
		recorder := doWithToken(t, dr.router, http.MethodPost, "/api/v1/rds/db-instances",
			body, dr.tokens.access)
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("POST %s: status = %d (body %s)", identifier, recorder.Code, recorder.Body)
		}
	}

	page := listDBInstances(t, dr, "")
	if len(page.Items) != 2 {
		t.Fatalf("GET /rds/db-instances returned %d items, want 2", len(page.Items))
	}

	found := listDBInstances(t, dr, "?q=bill")
	if len(found.Items) != 1 || found.Items[0].Identifier != "billing" {
		t.Errorf("?q=bill returned %+v, want only billing", found.Items)
	}

	first := listDBInstances(t, dr, "?page_size=1")
	if len(first.Items) != 1 || first.NextPageToken == nil {
		t.Fatalf("?page_size=1 returned %d items, token %v", len(first.Items), first.NextPageToken)
	}
	second := listDBInstances(t, dr, "?page_size=1&page_token="+*first.NextPageToken)
	if len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID {
		t.Errorf("the second page repeated the first: %+v", second.Items)
	}

	for _, query := range []string{"?page_size=0", "?page_size=201", "?sort=identifier:asc",
		"?state=terminated", "?page_token=nope"} {
		if recorder := doWithToken(t, dr.router, http.MethodGet, "/api/v1/rds/db-instances"+query,
			"", dr.tokens.access); recorder.Code != http.StatusBadRequest {
			t.Errorf("GET %s: status = %d, want 400 (body %s)", query, recorder.Code, recorder.Body)
		}
	}
}

// A node with the module off answers 503 on every /rds route and says why,
// rather than a 404 that reads like a typo.
func TestDatabasesOff(t *testing.T) {
	t.Parallel()

	logger := logging.Discard()
	db := storetest.New(t)
	signer, err := auth.NewSigner([]byte(strings.Repeat("o", 32)), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewSigner(): %v", err)
	}
	authSvc, err := authservice.New(authservice.Options{
		Logger: logger, Store: db, Signer: signer, RefreshTTL: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("auth.New(): %v", err)
	}

	handler := api.NewHandler(api.Options{
		Logger:       logger,
		GinMode:      gin.TestMode,
		System:       system.New(logger, "test", "fake", nil, nil, nil),
		Auth:         authSvc,
		Signer:       signer,
		RefreshTTL:   7 * 24 * time.Hour,
		CookieSecure: false,
	})
	router := handler.Router()
	tokens := setupUserAndLogin(t, router)

	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/rds/db-instances"},
		{http.MethodPost, "/api/v1/rds/db-instances"},
		{http.MethodGet, "/api/v1/rds/db-instances/db-0a1b2c3d4e5f60718"},
		{http.MethodDelete, "/api/v1/rds/db-instances/db-0a1b2c3d4e5f60718"},
		{http.MethodPost, "/api/v1/rds/db-instances/db-0a1b2c3d4e5f60718/stop"},
	} {
		recorder := doWithToken(t, router, test.method, test.path, "", tokens.access)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s with the module off: status = %d, want 503 (body %s)",
				test.method, test.path, recorder.Code, recorder.Body)
			continue
		}
		if code := errorCode(t, recorder); code != api.CodeUnavailable {
			t.Errorf("%s %s: error code = %q, want %q", test.method, test.path, code, api.CodeUnavailable)
		}
		if !strings.Contains(recorder.Body.String(), "KUMO_DATABASES") {
			t.Errorf("%s %s body = %s, want it to name the variable to set",
				test.method, test.path, recorder.Body)
		}
	}

	// A route that is still authenticated first: an anonymous caller learns
	// nothing about which modules a node runs.
	if recorder := do(t, router, http.MethodGet, "/api/v1/rds/db-instances", ""); recorder.Code !=
		http.StatusUnauthorized {
		t.Errorf("GET without a token: status = %d, want 401", recorder.Code)
	}
}

// A create whose engine never answers leaves the DB instance failed with a
// reason, and the guest alone.
func TestCreateDBInstanceWhoseEngineNeverAnswers(t *testing.T) {
	t.Parallel()

	dr := newDBRouterWith(t, func(context.Context, string) error { return errNothingListening })
	engineImageID(t, dr)

	accepted := doWithToken(t, dr.router, http.MethodPost, "/api/v1/rds/db-instances",
		createDBInstanceBody, dr.tokens.access)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("POST: status = %d (body %s)", accepted.Code, accepted.Body)
	}
	var created dbInstanceBody
	decode(t, accepted, &created)
	waitForTaskState(t, dr.router, dr.tokens.access, *created.TaskID, "failed")

	got := getDBInstance(t, dr, created.ID)
	if got.State != "failed" {
		t.Errorf("State = %q, want failed", got.State)
	}
	if got.StateReason == nil || !strings.Contains(*got.StateReason, "did not answer") {
		t.Errorf("state_reason = %v, want it to say the engine did not answer", got.StateReason)
	}

	// The guest is still there: the VM is fine, and throwing it away would
	// destroy the evidence an operator needs.
	if _, err := dr.hv.GetDomain(t.Context(), got.InstanceID); err != nil {
		t.Errorf("GetDomain(): %v, want the guest left in place", err)
	}

	// And delete is the way out.
	deleted := doWithToken(t, dr.router, http.MethodDelete,
		"/api/v1/rds/db-instances/"+got.ID, "", dr.tokens.access)
	if deleted.Code != http.StatusAccepted {
		t.Fatalf("delete a failed DB instance: status = %d (body %s)", deleted.Code, deleted.Body)
	}
	decode(t, deleted, &got)
	waitForTaskState(t, dr.router, dr.tokens.access, *got.TaskID, "succeeded")
}

// createAvailableDBInstance creates one through the API and waits for it.
func createAvailableDBInstance(t *testing.T, dr dbRouter) dbInstanceBody {
	t.Helper()

	accepted := doWithToken(t, dr.router, http.MethodPost, "/api/v1/rds/db-instances",
		createDBInstanceBody, dr.tokens.access)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("POST /rds/db-instances status = %d (body %s)", accepted.Code, accepted.Body)
	}

	var created dbInstanceBody
	decode(t, accepted, &created)
	waitForTaskState(t, dr.router, dr.tokens.access, *created.TaskID, "succeeded")

	got := getDBInstance(t, dr, created.ID)
	if got.State != stateAvailable {
		t.Fatalf("State = %q (%v), want available", got.State, got.StateReason)
	}
	return got
}

func getDBInstance(t *testing.T, dr dbRouter, dbInstanceID string) dbInstanceBody {
	t.Helper()

	recorder := doWithToken(t, dr.router, http.MethodGet,
		"/api/v1/rds/db-instances/"+dbInstanceID, "", dr.tokens.access)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /rds/db-instances/%s status = %d (body %s)",
			dbInstanceID, recorder.Code, recorder.Body)
	}
	var body dbInstanceBody
	decode(t, recorder, &body)
	return body
}

func listDBInstances(t *testing.T, dr dbRouter, query string) dbInstancePage {
	t.Helper()

	recorder := doWithToken(t, dr.router, http.MethodGet,
		"/api/v1/rds/db-instances"+query, "", dr.tokens.access)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /rds/db-instances%s status = %d (body %s)", query, recorder.Code, recorder.Body)
	}
	var page dbInstancePage
	decode(t, recorder, &page)
	return page
}

func getBody(t *testing.T, dr dbRouter, path string) string {
	t.Helper()
	return doWithToken(t, dr.router, http.MethodGet, path, "", dr.tokens.access).Body.String()
}

// errorCode reads the code out of the API error envelope.
func errorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()

	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode the error envelope: %v", err)
	}
	return envelope.Error.Code
}
