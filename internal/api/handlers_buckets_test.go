package api_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	objectfake "github.com/ahmedmohamedut/kumo/internal/objectstore/fake"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/bucket"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
)

// bucketBody mirrors Bucket and BucketDetail of api/openapi.yaml.
type bucketBody struct {
	Name       string            `json:"name"`
	State      string            `json:"state"`
	CreatedAt  time.Time         `json:"created_at"`
	CreatedBy  *string           `json:"created_by"`
	Tags       map[string]string `json:"tags"`
	Versioning string            `json:"versioning"`
	Endpoint   string            `json:"endpoint"`
	Region     string            `json:"region"`
}

type bucketsBody struct {
	Items []bucketBody `json:"items"`
}

type bucketDeletedBody struct {
	Bucket bucketBody `json:"bucket"`
	TaskID string     `json:"task_id"`
}

// newBucketRouter builds a router with object storage on the fake, or off
// when objects is nil.
func newBucketRouter(t *testing.T, objects *objectfake.Store) (*gin.Engine, *auth.Signer) {
	t.Helper()
	logger := logging.Discard()
	db := storetest.New(t)

	signer, err := auth.NewSigner([]byte(capacitySecret), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	authSvc, err := authservice.New(authservice.Options{
		Logger: logger, Store: db, Signer: signer, RefreshTTL: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatal(err)
	}

	var buckets *bucket.Service
	if objects != nil {
		buckets, err = bucket.New(bucket.Options{
			Logger: logger, Store: db, Objects: objects, Tasks: tasks,
			PublicURL: "https://s3.kumo.example", Region: "us-east-1",
			CORSOrigins: []string{"https://kumo.example"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	handler := api.NewHandler(api.Options{
		Logger:     logger,
		GinMode:    gin.TestMode,
		System:     system.New(logger, "test", "fake", db, nil, nil),
		Auth:       authSvc,
		Buckets:    buckets,
		Tasks:      tasks,
		Signer:     signer,
		RefreshTTL: 7 * 24 * time.Hour,
	})
	return handler.Router(), signer
}

func TestBucketLifecycle(t *testing.T) {
	t.Parallel()
	objects := objectfake.New()
	router, _ := newBucketRouter(t, objects)
	login := setupUserAndLogin(t, router)

	recorder := doWithToken(t, router, http.MethodPost, "/api/v1/s3/buckets",
		`{"name":"app-logs","versioning":true,"tags":{"env":"prod"}}`, login.access)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST /s3/buckets status = %d, want 201 (body %s)", recorder.Code, recorder.Body)
	}
	if got := recorder.Header().Get("Location"); got != "/api/v1/s3/buckets/app-logs" {
		t.Errorf("Location = %q", got)
	}
	var created bucketBody
	decode(t, recorder, &created)
	if created.Name != "app-logs" || created.State != "available" || created.Versioning != "enabled" ||
		created.Endpoint != "https://s3.kumo.example" || created.Tags["env"] != vpcProd ||
		created.CreatedBy == nil || created.CreatedAt.IsZero() {
		t.Errorf("created = %+v", created)
	}

	recorder = doWithToken(t, router, http.MethodGet, "/api/v1/s3/buckets", "", login.access)
	var list bucketsBody
	decode(t, recorder, &list)
	if recorder.Code != http.StatusOK || len(list.Items) != 1 || list.Items[0].Name != "app-logs" {
		t.Errorf("GET /s3/buckets = %d %+v", recorder.Code, list)
	}

	recorder = doWithToken(t, router, http.MethodPatch, "/api/v1/s3/buckets/app-logs",
		`{"versioning":false,"tags":{}}`, login.access)
	var updated bucketBody
	decode(t, recorder, &updated)
	if recorder.Code != http.StatusOK || updated.Versioning != "suspended" || len(updated.Tags) != 0 {
		t.Errorf("PATCH = %d %+v", recorder.Code, updated)
	}

	recorder = doWithToken(t, router, http.MethodDelete, "/api/v1/s3/buckets/app-logs", "", login.access)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204 (body %s)", recorder.Code, recorder.Body)
	}
	recorder = doWithToken(t, router, http.MethodGet, "/api/v1/s3/buckets/app-logs", "", login.access)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE status = %d, want 404", recorder.Code)
	}
}

func TestDeleteBucketThatHoldsObjects(t *testing.T) {
	t.Parallel()
	objects := objectfake.New()
	router, _ := newBucketRouter(t, objects)
	login := setupUserAndLogin(t, router)

	if err := objects.CreateBucket(context.Background(), "full"); err != nil {
		t.Fatal(err)
	}
	if err := objects.PutObject(context.Background(), "full", "k", bytes.NewReader([]byte("x")), 1, ""); err != nil {
		t.Fatal(err)
	}

	recorder := doWithToken(t, router, http.MethodDelete, "/api/v1/s3/buckets/full", "", login.access)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("DELETE of a non-empty bucket = %d, want 409 (body %s)", recorder.Code, recorder.Body)
	}
	if envelope := decodeError(t, recorder.Body.Bytes()); envelope.Error.Code != api.CodeConflict ||
		!strings.Contains(envelope.Error.Message, "force") {
		t.Errorf("error = %+v, want a conflict that mentions force", envelope.Error)
	}

	recorder = doWithToken(t, router, http.MethodDelete, "/api/v1/s3/buckets/full?force=maybe", "", login.access)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("DELETE ?force=maybe = %d, want 400", recorder.Code)
	}

	recorder = doWithToken(t, router, http.MethodDelete, "/api/v1/s3/buckets/full?force=true", "", login.access)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("DELETE ?force=true = %d, want 202 (body %s)", recorder.Code, recorder.Body)
	}
	var deleted bucketDeletedBody
	decode(t, recorder, &deleted)
	if deleted.Bucket.State != "deleting" || deleted.TaskID == "" ||
		recorder.Header().Get("Location") != "/api/v1/tasks/"+deleted.TaskID {
		t.Errorf("forced delete = %+v, Location %q", deleted, recorder.Header().Get("Location"))
	}

	recorder = doWithToken(t, router, http.MethodPatch, "/api/v1/s3/buckets/full", `{"versioning":true}`, login.access)
	if recorder.Code != http.StatusConflict {
		t.Errorf("PATCH while deleting = %d, want 409", recorder.Code)
	}
}

func TestBucketRefusals(t *testing.T) {
	t.Parallel()
	router, _ := newBucketRouter(t, objectfake.New())
	login := setupUserAndLogin(t, router)
	if recorder := doWithToken(t, router, http.MethodPost, "/api/v1/s3/buckets", `{"name":"taken"}`, login.access); recorder.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", recorder.Code, recorder.Body)
	}

	for _, test := range []struct {
		name, method, path, body string
		status                   int
		code, field              string
	}{
		{"create, not JSON", http.MethodPost, "/api/v1/s3/buckets", `{`, http.StatusBadRequest, api.CodeInvalidArgument, "body"},
		{"create, bad name", http.MethodPost, "/api/v1/s3/buckets", `{"name":"Bad_Name"}`, http.StatusBadRequest, api.CodeInvalidArgument, "name"},
		{"create, taken", http.MethodPost, "/api/v1/s3/buckets", `{"name":"taken"}`, http.StatusConflict, api.CodeAlreadyExists, ""},
		{"update, empty", http.MethodPatch, "/api/v1/s3/buckets/taken", `{}`, http.StatusBadRequest, api.CodeInvalidArgument, "body"},
		{"update, missing", http.MethodPatch, "/api/v1/s3/buckets/nope", `{"versioning":true}`, http.StatusNotFound, api.CodeNotFound, ""},
		{"get, missing", http.MethodGet, "/api/v1/s3/buckets/nope", "", http.StatusNotFound, api.CodeNotFound, ""},
		{"delete, missing", http.MethodDelete, "/api/v1/s3/buckets/nope", "", http.StatusNotFound, api.CodeNotFound, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := doWithToken(t, router, test.method, test.path, test.body, login.access)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, test.status, recorder.Body)
			}
			envelope := decodeError(t, recorder.Body.Bytes())
			if envelope.Error.Code != test.code {
				t.Errorf("code = %q, want %q", envelope.Error.Code, test.code)
			}
			if test.field != "" && (len(envelope.Error.Details) == 0 || envelope.Error.Details[0].Field != test.field) {
				t.Errorf("details = %+v, want %q", envelope.Error.Details, test.field)
			}
		})
	}
}

func TestBucketsForAViewer(t *testing.T) {
	t.Parallel()
	router, signer := newBucketRouter(t, objectfake.New())
	login := setupUserAndLogin(t, router)
	if recorder := doWithToken(t, router, http.MethodPost, "/api/v1/s3/buckets", `{"name":"shared"}`, login.access); recorder.Code != http.StatusCreated {
		t.Fatalf("setup: %d", recorder.Code)
	}
	viewer, err := signer.IssueAccessToken("01J0000000000000000000000V", auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/s3/buckets", `{"name":"mine"}`},
		{http.MethodPatch, "/api/v1/s3/buckets/shared", `{"versioning":true}`},
		{http.MethodDelete, "/api/v1/s3/buckets/shared", ""},
		{http.MethodDelete, "/api/v1/s3/buckets/shared?force=true", ""},
	} {
		if recorder := doWithToken(t, router, test.method, test.path, test.body, viewer.Token); recorder.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", test.method, test.path, recorder.Code)
		}
	}
	for _, path := range []string{"/api/v1/s3/buckets", "/api/v1/s3/buckets/shared"} {
		if recorder := doWithToken(t, router, http.MethodGet, path, "", viewer.Token); recorder.Code != http.StatusOK {
			t.Errorf("viewer GET %s = %d, want 200", path, recorder.Code)
		}
	}
	for _, test := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/s3/buckets"},
		{http.MethodPost, "/api/v1/s3/buckets"},
		{http.MethodDelete, "/api/v1/s3/buckets/shared"},
	} {
		if recorder := doWithToken(t, router, test.method, test.path, "", ""); recorder.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", test.method, test.path, recorder.Code)
		}
	}
}

func TestObjectStorageOff(t *testing.T) {
	t.Parallel()
	router, _ := newBucketRouter(t, nil)
	login := setupUserAndLogin(t, router)

	for _, test := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/s3/buckets"},
		{http.MethodPost, "/api/v1/s3/buckets"},
		{http.MethodGet, "/api/v1/s3/buckets/logs"},
		{http.MethodDelete, "/api/v1/s3/buckets/logs"},
	} {
		recorder := doWithToken(t, router, test.method, test.path, "", login.access)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s with the module off = %d, want 503", test.method, test.path, recorder.Code)
			continue
		}
		if envelope := decodeError(t, recorder.Body.Bytes()); envelope.Error.Code != api.CodeUnavailable ||
			!strings.Contains(envelope.Error.Message, "KUMO_OBJECTSTORE") {
			t.Errorf("error = %+v", envelope.Error)
		}
	}
	if recorder := doWithToken(t, router, http.MethodGet, "/api/v1/s3/buckets", "", ""); recorder.Code != http.StatusUnauthorized {
		t.Errorf("anonymous with the module off = %d, want 401 before 503", recorder.Code)
	}
}

func TestObjectStoreDown(t *testing.T) {
	t.Parallel()
	objects := objectfake.New()
	router, _ := newBucketRouter(t, objects)
	login := setupUserAndLogin(t, router)
	objects.SetDown(true)

	recorder := doWithToken(t, router, http.MethodGet, "/api/v1/s3/buckets", "", login.access)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("GET with the server down = %d, want 503 (body %s)", recorder.Code, recorder.Body)
	}
}
