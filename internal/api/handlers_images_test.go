package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/ahmedmohamedut/kumo/internal/imagefmt/qcow2test"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// imagesPool is the pool the API tests import into.
const imagesPool = "kumo-images"

// imageBody is the shape of an image in a response. The pointer fields are how
// the tests tell null from an empty string, which is the whole point of the
// pending state.
type imageBody struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	State       string            `json:"state"`
	StateReason *string           `json:"state_reason"`
	Progress    int               `json:"progress"`
	Format      string            `json:"format"`
	SourceURL   *string           `json:"source_url"`
	SizeBytes   *int64            `json:"size_bytes"`
	MinDiskGB   int               `json:"min_disk_gb"`
	Tags        map[string]string `json:"tags"`
	TaskID      string            `json:"task_id"`
	Checksum    *image.Checksum   `json:"checksum"`
}

// imagePage is a listing response.
type imagePage struct {
	Items         []imageBody `json:"items"`
	NextPageToken *string     `json:"next_page_token"`
}

type taskBody struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	ResourceID *string `json:"resource_id"`
	State      string  `json:"state"`
	Progress   int     `json:"progress"`
	Error      *string `json:"error"`
}

// imageRouter is the router under test plus what a test needs to reach below the
// API surface.
type imageRouter struct {
	router *gin.Engine
	svc    *image.Service
	store  *store.Store
	hv     *fake.Fake
	signer *auth.Signer
}

// newImageRouter builds a router with real services and a fake hypervisor. The
// downloader allows loopback, which is what httptest listens on.
func newImageRouter(t *testing.T) imageRouter {
	t.Helper()

	logger := logging.Discard()
	db := storetest.New(t)

	signer, err := auth.NewSigner([]byte(strings.Repeat("i", 32)), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewSigner(): %v", err)
	}
	authSvc, err := authservice.New(authservice.Options{
		Logger: logger, Store: db, Signer: signer, RefreshTTL: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("authservice.New(): %v", err)
	}
	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}

	hv := fake.New(fake.Config{})
	if err := hv.EnsureInfra(t.Context(), hypervisor.InfraConfig{
		ImagesPool: imagesPool, InstancesPool: "kumo-instances", DataDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("EnsureInfra(): %v", err)
	}

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

	handler := api.NewHandler(api.Options{
		Logger:       logger,
		GinMode:      gin.TestMode,
		System:       system.New(logger, "test", "fake", nil, nil),
		Auth:         authSvc,
		Images:       images,
		Tasks:        tasks,
		Signer:       signer,
		RefreshTTL:   7 * 24 * time.Hour,
		CookieSecure: false,
	})

	return imageRouter{router: handler.Router(), svc: images, store: db, hv: hv, signer: signer}
}

// tinyImageServer serves a valid tiny qcow2 and reports its SHA-256.
func tinyImageServer(t *testing.T) (url, sum string) {
	t.Helper()

	body := qcow2test.Tiny(1 << 30)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	digest := sha256.Sum256(body)
	return server.URL + "/ubuntu.img", hex.EncodeToString(digest[:])
}

// waitForTaskState polls a task until it reaches one of the wanted states, so the
// tests do not depend on how fast the worker is.
func waitForTaskState(
	t *testing.T, router *gin.Engine, token, taskID string, want ...string,
) taskBody {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	var last taskBody
	for {
		recorder := doWithToken(t, router, http.MethodGet, "/api/v1/tasks/"+taskID, "", token)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /tasks/%s status = %d, want 200 (body %s)", taskID, recorder.Code, recorder.Body)
		}
		decode(t, recorder, &last)

		for _, state := range want {
			if last.State == state {
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s stayed in %q, want one of %v", taskID, last.State, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startWorker runs a real worker pool against the image service, so the tests
// exercise the same path the server does: a task row claimed, run and finished.
func startWorker(t *testing.T, svc *image.Service, db *store.Store) {
	t.Helper()

	pool := worker.New(worker.Options{
		Logger:       logging.Discard(),
		Store:        db,
		Registry:     worker.Registry(svc.Handler()),
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
}

func TestImageImportIsAsynchronousAndReportsItsTask(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)
	startWorker(t, ir.svc, ir.store)
	login := setupUserAndLogin(t, ir.router)
	url, sum := tinyImageServer(t)

	accepted := doWithToken(t, ir.router, http.MethodPost, "/api/v1/images/import", fmt.Sprintf(
		`{"name":"ubuntu-2404","description":"the LTS image","source_url":%q,`+
			`"checksum":{"algorithm":"sha256","value":%q},"os_family":"linux",`+
			`"os_version":"24.04","architecture":"x86_64","default_user":"ubuntu",`+
			`"min_disk_gb":8,"tags":{"env":"dev"}}`, url, sum),
		login.access)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("POST /images/import status = %d, want 202 (body %s)",
			accepted.Code, accepted.Body)
	}

	var imported imageBody
	decode(t, accepted, &imported)
	if imported.State != "pending" {
		t.Errorf("state = %q, want pending: the download must not have run in the request", imported.State)
	}
	if !strings.HasPrefix(imported.ID, "img-") {
		t.Errorf("id = %q, want an img- prefixed id", imported.ID)
	}
	if !strings.HasPrefix(imported.TaskID, "task-") {
		t.Errorf("task_id = %q, want a task- prefixed id", imported.TaskID)
	}

	// The Location header is what the console polls.
	location := accepted.Header().Get("Location")
	if location != "/api/v1/tasks/"+imported.TaskID {
		t.Errorf("Location = %q, want /api/v1/tasks/%s", location, imported.TaskID)
	}

	// The task answers 200 right away, and it is still unfinished: the download
	// is running in the worker, not in the request.
	polled := doWithToken(t, ir.router, http.MethodGet, location, "", login.access)
	if polled.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200 (body %s)", location, polled.Code, polled.Body)
	}
	var found taskBody
	decode(t, polled, &found)
	if found.Type != "image.import" {
		t.Errorf("task type = %q, want image.import", found.Type)
	}
	if found.State != "pending" && found.State != "running" {
		t.Errorf("task state = %q, want pending or running while the import runs", found.State)
	}

	// Running the task makes the image available and the task succeed.
	waitForTaskState(t, ir.router, login.access, imported.TaskID, "succeeded")

	polled = doWithToken(t, ir.router, http.MethodGet, location, "", login.access)
	decode(t, polled, &found)
	if found.State != "succeeded" {
		t.Errorf("task state = %q, want succeeded after the worker ran", found.State)
	}
	if found.Progress != 100 {
		t.Errorf("task progress = %d, want 100", found.Progress)
	}

	got := doWithToken(t, ir.router, http.MethodGet, "/api/v1/images/"+imported.ID, "", login.access)
	if got.Code != http.StatusOK {
		t.Fatalf("GET /images/{id} status = %d, want 200 (body %s)", got.Code, got.Body)
	}
	var available imageBody
	decode(t, got, &available)
	if available.State != "available" {
		t.Errorf("state = %q, want available", available.State)
	}
	if available.Progress != 100 || available.Format != "qcow2" {
		t.Errorf("progress/format = %d/%q, want 100/qcow2", available.Progress, available.Format)
	}
	if available.SizeBytes == nil || *available.SizeBytes <= 0 {
		t.Errorf("size_bytes = %v, want the file size", available.SizeBytes)
	}
	if available.Tags["env"] != "dev" {
		t.Errorf("tags = %v, want env=dev", available.Tags)
	}
}

func TestImageImportRejectsBadRequests(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)
	login := setupUserAndLogin(t, ir.router)
	url, _ := tinyImageServer(t)

	tests := []struct {
		name string
		body string
		code string
	}{
		{
			name: "malformed body",
			body: `{`,
			code: "invalid_argument",
		},
		{
			name: "missing source url",
			body: `{"name":"broken"}`,
			code: "invalid_argument",
		},
		{
			name: "file scheme",
			body: `{"name":"broken","source_url":"file:///etc/shadow"}`,
			code: "invalid_argument",
		},
		{
			name: "metadata url",
			body: `{"name":"broken","source_url":"http://169.254.169.254/latest/meta-data/"}`,
			code: "invalid_argument",
		},
		{
			name: "bad name",
			body: fmt.Sprintf(`{"name":"not a name","source_url":%q}`, url),
			code: "invalid_argument",
		},
		{
			name: "short checksum",
			body: fmt.Sprintf(
				`{"name":"broken","source_url":%q,"checksum":{"algorithm":"sha256","value":"ab"}}`, url),
			code: "invalid_argument",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/images/import",
				tt.body, login.access)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body)
			}
			assertErrorCode(t, recorder, tt.code)
		})
	}
}

func TestImageImportRejectsADuplicateName(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)
	login := setupUserAndLogin(t, ir.router)
	url, sum := tinyImageServer(t)
	body := fmt.Sprintf(`{"name":"ubuntu-2404","source_url":%q,`+
		`"checksum":{"algorithm":"sha256","value":%q}}`, url, sum)

	first := doWithToken(t, ir.router, http.MethodPost, "/api/v1/images/import", body, login.access)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first import status = %d, want 202 (body %s)", first.Code, first.Body)
	}

	second := doWithToken(t, ir.router, http.MethodPost, "/api/v1/images/import", body, login.access)
	if second.Code != http.StatusConflict {
		t.Fatalf("second import status = %d, want 409 (body %s)", second.Code, second.Body)
	}
	assertErrorCode(t, second, "already_exists")
}

func TestImageLifecycleThroughTheAPI(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)
	startWorker(t, ir.svc, ir.store)
	login := setupUserAndLogin(t, ir.router)
	url, sum := tinyImageServer(t)

	accepted := doWithToken(t, ir.router, http.MethodPost, "/api/v1/images/import", fmt.Sprintf(
		`{"name":"ubuntu-2404","source_url":%q,"checksum":{"algorithm":"sha256","value":%q}}`,
		url, sum), login.access)
	var imported imageBody
	decode(t, accepted, &imported)
	waitForTaskState(t, ir.router, login.access, imported.TaskID, "succeeded")

	t.Run("patch renames and retags", func(t *testing.T) {
		recorder := doWithToken(t, ir.router, http.MethodPatch, "/api/v1/images/"+imported.ID,
			`{"name":"ubuntu-24.04-lts","description":"the LTS image","tags":{"env":"prod"}}`,
			login.access)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PATCH status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
		}
		var updated imageBody
		decode(t, recorder, &updated)
		if updated.Name != "ubuntu-24.04-lts" || updated.Tags["env"] != "prod" {
			t.Errorf("patched image = %q %v, want the new name and env=prod", updated.Name, updated.Tags)
		}
	})

	t.Run("patch with nothing set is rejected", func(t *testing.T) {
		recorder := doWithToken(t, ir.router, http.MethodPatch, "/api/v1/images/"+imported.ID,
			`{}`, login.access)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("PATCH {} status = %d, want 400 (body %s)", recorder.Code, recorder.Body)
		}
	})

	t.Run("delete answers 202 and the volume goes", func(t *testing.T) {
		recorder := doWithToken(t, ir.router, http.MethodDelete, "/api/v1/images/"+imported.ID,
			"", login.access)
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("DELETE status = %d, want 202 (body %s)", recorder.Code, recorder.Body)
		}

		var deleted imageBody
		decode(t, recorder, &deleted)
		if deleted.State != "deleting" {
			t.Errorf("state = %q, want deleting", deleted.State)
		}
		if location := recorder.Header().Get("Location"); location != "/api/v1/tasks/"+deleted.TaskID {
			t.Errorf("Location = %q, want /api/v1/tasks/%s", location, deleted.TaskID)
		}

		waitForTaskState(t, ir.router, login.access, deleted.TaskID, "succeeded")

		gone := doWithToken(t, ir.router, http.MethodGet, "/api/v1/images/"+imported.ID,
			"", login.access)
		if gone.Code != http.StatusNotFound {
			t.Errorf("GET after the delete status = %d, want 404", gone.Code)
		}
		if _, err := ir.hv.ImageInfo(t.Context(), imported.ID); !strings.Contains(
			fmt.Sprint(err), "not found") {
			t.Errorf("ImageInfo() after the delete: error = %v, want a not found error", err)
		}
	})
}

func TestDeleteImageRefusesAnImageInUse(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)
	startWorker(t, ir.svc, ir.store)
	login := setupUserAndLogin(t, ir.router)
	url, sum := tinyImageServer(t)

	accepted := doWithToken(t, ir.router, http.MethodPost, "/api/v1/images/import", fmt.Sprintf(
		`{"name":"in-use","source_url":%q,"checksum":{"algorithm":"sha256","value":%q}}`,
		url, sum), login.access)
	var imported imageBody
	decode(t, accepted, &imported)
	waitForTaskState(t, ir.router, login.access, imported.TaskID, "succeeded")
	insertRunningInstance(t, ir.store, imported.ID)

	recorder := doWithToken(t, ir.router, http.MethodDelete, "/api/v1/images/"+imported.ID,
		"", login.access)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("DELETE status = %d, want 409 (body %s)", recorder.Code, recorder.Body)
	}
	assertErrorCode(t, recorder, "conflict")
	if !strings.Contains(recorder.Body.String(), imported.ID) {
		t.Errorf("the conflict message does not name the image: %s", recorder.Body)
	}
}

func TestRetryAFailedImageImport(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)
	login := setupUserAndLogin(t, ir.router)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)

	accepted := doWithToken(t, ir.router, http.MethodPost, "/api/v1/images/import",
		fmt.Sprintf(`{"name":"flaky","source_url":%q}`, broken.URL+"/image.img"), login.access)
	var imported imageBody
	decode(t, accepted, &imported)

	// The task fails, which the image records as its reason.
	if err := ir.svc.ImportHandler(t.Context(), worker.Task{
		ID: "task-test", Type: task.TypeImageImport, ResourceID: imported.ID,
		Payload: []byte(fmt.Sprintf(`{"image_id":%q}`, imported.ID)),
	}); err == nil {
		t.Fatal("the import of a 500 URL succeeded, want an error")
	}

	failed := doWithToken(t, ir.router, http.MethodGet, "/api/v1/images/"+imported.ID,
		"", login.access)
	var body imageBody
	decode(t, failed, &body)
	if body.State != "failed" || body.StateReason == nil {
		t.Errorf("state = %q with reason %v, want failed with a reason", body.State, body.StateReason)
	}

	// Point the row at a healthy server and retry it.
	url, sum := tinyImageServer(t)
	if _, err := ir.store.Exec(t.Context(),
		`UPDATE images SET source_url = ? WHERE id = ?`, url, imported.ID); err != nil {
		t.Fatalf("update the source URL: %v", err)
	}
	if _, err := ir.store.Exec(t.Context(),
		`UPDATE images SET checksum_algo = 'sha256', checksum_value = ? WHERE id = ?`,
		sum, imported.ID); err != nil {
		t.Fatalf("update the checksum: %v", err)
	}

	retried := doWithToken(t, ir.router, http.MethodPost,
		"/api/v1/images/"+imported.ID+"/retry", "", login.access)
	if retried.Code != http.StatusAccepted {
		t.Fatalf("POST retry status = %d, want 202 (body %s)", retried.Code, retried.Body)
	}
	var queued imageBody
	decode(t, retried, &queued)
	if queued.State != "pending" {
		t.Errorf("state = %q, want pending after a retry", queued.State)
	}

	// The worker only starts now, so the retry task is the one that runs.
	startWorker(t, ir.svc, ir.store)
	waitForTaskState(t, ir.router, login.access, queued.TaskID, "succeeded")

	got := doWithToken(t, ir.router, http.MethodGet, "/api/v1/images/"+imported.ID, "", login.access)
	decode(t, got, &queued)
	if queued.State != "available" {
		t.Errorf("state = %q, want available after the retry", queued.State)
	}

	// Retrying an available image is a conflict, not a second download.
	again := doWithToken(t, ir.router, http.MethodPost,
		"/api/v1/images/"+imported.ID+"/retry", "", login.access)
	if again.Code != http.StatusConflict {
		t.Errorf("retry of an available image status = %d, want 409", again.Code)
	}
}

func TestImageCatalogIsServed(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)
	login := setupUserAndLogin(t, ir.router)

	recorder := doWithToken(t, ir.router, http.MethodGet, "/api/v1/images/catalog", "", login.access)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /images/catalog status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), "source_url") {
		t.Errorf("the catalog body has no source_url field: %s", recorder.Body)
	}

	// /images/catalog must not be swallowed by /images/:id.
	var catalog struct {
		Items []image.CatalogImage `json:"items"`
	}
	decode(t, recorder, &catalog)
	if len(catalog.Items) == 0 {
		t.Error("the catalog is empty")
	}
}

func TestListImagesFiltersAndPages(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)
	login := setupUserAndLogin(t, ir.router)
	url, sum := tinyImageServer(t)

	imported := map[string]string{}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		accepted := doWithToken(t, ir.router, http.MethodPost, "/api/v1/images/import", fmt.Sprintf(
			`{"name":%q,"source_url":%q,"checksum":{"algorithm":"sha256","value":%q}}`,
			name, url, sum), login.access)
		var body imageBody
		decode(t, accepted, &body)
		imported[name] = body.ID
	}

	all := doWithToken(t, ir.router, http.MethodGet, "/api/v1/images", "", login.access)
	if all.Code != http.StatusOK {
		t.Fatalf("GET /images status = %d, want 200 (body %s)", all.Code, all.Body)
	}
	var page imagePage
	decode(t, all, &page)
	if len(page.Items) != 3 {
		t.Errorf("listed %d images, want 3", len(page.Items))
	}

	filtered := doWithToken(t, ir.router, http.MethodGet, "/api/v1/images?q=gam", "", login.access)
	decode(t, filtered, &page)
	if len(page.Items) != 1 || page.Items[0].Name != "gamma" {
		t.Errorf("filtered page = %+v, want only gamma", page.Items)
	}

	paged := doWithToken(t, ir.router, http.MethodGet,
		"/api/v1/images?page_size=2&sort=created_at:asc", "", login.access)
	decode(t, paged, &page)
	if len(page.Items) != 2 || page.NextPageToken == nil {
		t.Fatalf("first page = %d items with token %v, want 2 items and a token",
			len(page.Items), page.NextPageToken)
	}

	next := doWithToken(t, ir.router, http.MethodGet,
		"/api/v1/images?page_size=2&sort=created_at:asc&page_token="+*page.NextPageToken,
		"", login.access)
	var rest imagePage
	decode(t, next, &rest)
	if len(rest.Items) != 1 {
		t.Errorf("second page = %d items, want the remaining 1", len(rest.Items))
	}

	badSort := doWithToken(t, ir.router, http.MethodGet, "/api/v1/images?sort=name", "", login.access)
	if badSort.Code != http.StatusBadRequest {
		t.Errorf("sort=name status = %d, want 400", badSort.Code)
	}
	badSize := doWithToken(t, ir.router, http.MethodGet,
		"/api/v1/images?page_size=100000", "", login.access)
	if badSize.Code != http.StatusBadRequest {
		t.Errorf("page_size=100000 status = %d, want 400", badSize.Code)
	}
}

func TestTaskEndpoints(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)
	login := setupUserAndLogin(t, ir.router)

	t.Run("an unknown task is a 404", func(t *testing.T) {
		recorder := doWithToken(t, ir.router, http.MethodGet,
			"/api/v1/tasks/task-00000000000000000", "", login.access)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body)
		}
	})

	t.Run("the page can be narrowed to one image", func(t *testing.T) {
		url, sum := tinyImageServer(t)

		accepted := doWithToken(t, ir.router, http.MethodPost, "/api/v1/images/import", fmt.Sprintf(
			`{"name":"tracked","source_url":%q,"checksum":{"algorithm":"sha256","value":%q}}`,
			url, sum), login.access)
		var imported imageBody
		decode(t, accepted, &imported)

		recorder := doWithToken(t, ir.router, http.MethodGet,
			"/api/v1/tasks?resource_id="+imported.ID, "", login.access)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
		}
		var page struct {
			Items []taskBody `json:"items"`
		}
		decode(t, recorder, &page)
		if len(page.Items) != 1 {
			t.Fatalf("listed %d tasks, want 1", len(page.Items))
		}
		if page.Items[0].ResourceID == nil || *page.Items[0].ResourceID != imported.ID {
			t.Errorf("resource_id = %v, want %q", page.Items[0].ResourceID, imported.ID)
		}

		// The payload is internal: it must not leak into the API.
		if strings.Contains(recorder.Body.String(), "image_id") {
			t.Errorf("the task list leaked the payload: %s", recorder.Body)
		}
	})

	t.Run("bad paging arguments are rejected", func(t *testing.T) {
		for _, query := range []string{"?sort=nope", "?page_size=0", "?page_size=abc", "?page_token=zzz"} {
			recorder := doWithToken(t, ir.router, http.MethodGet, "/api/v1/tasks"+query,
				"", login.access)
			if recorder.Code != http.StatusBadRequest {
				t.Errorf("GET /tasks%s status = %d, want 400 (body %s)",
					query, recorder.Code, recorder.Body)
			}
		}
	})
}

func TestImageRoutesRequireAuthentication(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)

	tests := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/images", ""},
		{http.MethodGet, "/api/v1/images/catalog", ""},
		{http.MethodGet, "/api/v1/images/img-00000000000000000", ""},
		{http.MethodPost, "/api/v1/images/import", `{"name":"x","source_url":"https://example.com/a.img"}`},
		{http.MethodDelete, "/api/v1/images/img-00000000000000000", ""},
		{http.MethodGet, "/api/v1/tasks", ""},
		{http.MethodGet, "/api/v1/tasks/task-00000000000000000", ""},
	}

	for _, tt := range tests {
		recorder := do(t, ir.router, tt.method, tt.path, tt.body)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: status = %d, want 401",
				tt.method, tt.path, recorder.Code)
		}
	}
}

func TestImageWritesAreAdminOnly(t *testing.T) {
	t.Parallel()

	ir := newImageRouter(t)
	setupUser(t, ir.router)

	issued, err := ir.signer.IssueAccessToken("01J00000000000000000000000", auth.RoleViewer)
	if err != nil {
		t.Fatalf("IssueAccessToken(): %v", err)
	}
	viewer := session{access: issued.Token}

	recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/images/import",
		`{"name":"denied","source_url":"https://example.com/a.img"}`, viewer.access)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("import as a viewer: status = %d, want 403 (body %s)", recorder.Code, recorder.Body)
	}

	// Reading is open to both roles.
	if list := doWithToken(t, ir.router, http.MethodGet, "/api/v1/images", "", viewer.access); list.Code != http.StatusOK {
		t.Errorf("GET /images as a viewer: status = %d, want 200", list.Code)
	}
	if catalog := doWithToken(t, ir.router, http.MethodGet, "/api/v1/images/catalog", "",
		viewer.access); catalog.Code != http.StatusOK {
		t.Errorf("GET /images/catalog as a viewer: status = %d, want 200", catalog.Code)
	}
}

// insertRunningInstance seeds an instance that boots from an image.
func insertRunningInstance(t *testing.T, db *store.Store, imageID string) {
	t.Helper()

	if _, err := db.Exec(t.Context(),
		`INSERT OR IGNORE INTO instance_types (id, vcpus, memory_mib) VALUES ('kumo.small', 1, 1024)`); err != nil {
		t.Fatalf("insert the instance type: %v", err)
	}
	if _, err := db.Exec(t.Context(), `
		INSERT INTO instances (id, name, state, image_id, instance_type, vcpus, memory_mib,
			root_disk_gb, launched_at, updated_at)
		VALUES ('i-11111111111111111', 'web-1', 'running', ?, 'kumo.small', 1, 1024, 10,
			'2026-01-02T09:05:00Z', '2026-01-02T09:05:00Z')`, imageID); err != nil {
		t.Fatalf("insert the instance: %v", err)
	}
}
