package bucket_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	"github.com/ahmedmohamedut/kumo/internal/objectstore/fake"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/bucket"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

const (
	adminID   = "0190a0a0-0000-7000-8000-000000000001"
	publicURL = "https://s3.kumo.example"
)

var origins = []string{"https://kumo.example"}

type fixture struct {
	svc     *bucket.Service
	db      *store.Store
	objects *fake.Store
	tasks   *task.Service
}

func newFixture(t *testing.T, objects objectstore.ObjectStore) fixture {
	t.Helper()
	db := storetest.New(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.CreateUser(context.Background(), sqlc.CreateUserParams{
		ID: adminID, Username: "ada", PasswordHash: "x", Role: "admin", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{db: db, tasks: tasks}
	if objects == nil {
		f.objects = fake.New()
		objects = f.objects
	}
	f.svc, err = bucket.New(bucket.Options{
		Logger: logging.Discard(), Store: db, Objects: objects, Tasks: tasks,
		PublicURL: publicURL, Region: "us-east-1", CORSOrigins: origins,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestNewRequiresDependencies(t *testing.T) {
	t.Parallel()
	db := storetest.New(t)
	tasks, _ := task.New(task.Options{Store: db})
	full := bucket.Options{Logger: logging.Discard(), Store: db, Objects: fake.New(), Tasks: tasks}

	for name, tt := range map[string]struct {
		edit func(*bucket.Options)
		want error
	}{
		"logger":  {func(o *bucket.Options) { o.Logger = nil }, bucket.ErrNoLogger},
		"store":   {func(o *bucket.Options) { o.Store = nil }, bucket.ErrNoStore},
		"objects": {func(o *bucket.Options) { o.Objects = nil }, bucket.ErrNoObjectStore},
		"tasks":   {func(o *bucket.Options) { o.Tasks = nil }, bucket.ErrNoTasks},
	} {
		opts := full
		tt.edit(&opts)
		if _, err := bucket.New(opts); !errors.Is(err, tt.want) {
			t.Errorf("%s: got %v, want %v", name, err, tt.want)
		}
	}
}

func TestCreate(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()

	got, err := f.svc.Create(ctx, bucket.CreateInput{
		Name: "app-logs", Versioning: true, Tags: map[string]string{"env": "prod"}, CreatedBy: adminID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Name != "app-logs" || got.State != bucket.StateAvailable || got.CreatedAt.IsZero() {
		t.Errorf("Create: got %+v", got)
	}
	if got.CreatedBy == nil || *got.CreatedBy != "ada" {
		t.Errorf("CreatedBy: got %v, want ada", got.CreatedBy)
	}
	if got.Versioning != objectstore.VersioningEnabled || got.Endpoint != publicURL || got.Region != "us-east-1" {
		t.Errorf("detail: versioning %q, endpoint %q, region %q", got.Versioning, got.Endpoint, got.Region)
	}
	if got.Tags["env"] != "prod" {
		t.Errorf("Tags: got %v", got.Tags)
	}
	if cors := f.objects.CORS("app-logs"); !slices.Equal(cors, origins) {
		t.Errorf("CORS: got %v, want %v", cors, origins)
	}

	plain, err := f.svc.Create(ctx, bucket.CreateInput{Name: "plain", CreatedBy: adminID})
	if err != nil {
		t.Fatalf("Create(plain): %v", err)
	}
	if plain.Versioning != objectstore.VersioningOff || len(plain.Tags) != 0 || plain.Tags == nil {
		t.Errorf("Create(plain): got %+v", plain)
	}

	if _, err := f.svc.Create(ctx, bucket.CreateInput{Name: "app-logs"}); !errors.Is(err, service.ErrAlreadyExists) {
		t.Errorf("Create(taken): got %v, want ErrAlreadyExists", err)
	}
	if err := f.objects.CreateBucket(ctx, "made-elsewhere"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(ctx, bucket.CreateInput{Name: "made-elsewhere"}); !errors.Is(err, service.ErrAlreadyExists) {
		t.Errorf("Create(taken outside Kumo): got %v, want ErrAlreadyExists", err)
	}
}

func TestCreateRefusesInvalidInput(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)

	manyTags := map[string]string{}
	for i := range 51 {
		manyTags[string(rune('a'+i%26))+string(rune('a'+i/26))] = ""
	}
	tests := []struct {
		name  string
		in    bucket.CreateInput
		field string
	}{
		{"empty", bucket.CreateInput{}, "name"},
		{"too short", bucket.CreateInput{Name: "ab"}, "name"},
		{"too long", bucket.CreateInput{Name: string(bytes.Repeat([]byte("a"), 64))}, "name"},
		{"uppercase", bucket.CreateInput{Name: "Logs"}, "name"},
		{"underscore", bucket.CreateInput{Name: "app_logs"}, "name"},
		{"leading dash", bucket.CreateInput{Name: "-logs"}, "name"},
		{"trailing dot", bucket.CreateInput{Name: "logs."}, "name"},
		{"double dot", bucket.CreateInput{Name: "app..logs"}, "name"},
		{"ip address", bucket.CreateInput{Name: "192.168.1.10"}, "name"},
		{"reserved prefix", bucket.CreateInput{Name: "xn--logs"}, "name"},
		{"reserved suffix", bucket.CreateInput{Name: "logs-s3alias"}, "name"},
		{"too many tags", bucket.CreateInput{Name: "logs", Tags: manyTags}, "tags"},
		{"empty tag key", bucket.CreateInput{Name: "logs", Tags: map[string]string{"": "x"}}, "tags"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.Create(context.Background(), tt.in)
			var verr *service.ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("got %v, want a ValidationError", err)
			}
			if len(verr.Fields) == 0 || verr.Fields[0].Field != tt.field {
				t.Errorf("fields: got %+v, want %s", verr.Fields, tt.field)
			}
		})
	}

	buckets, err := f.objects.ListBuckets(context.Background())
	if err != nil || len(buckets) != 0 {
		t.Errorf("a refused create reached the object store: %v, %v", buckets, err)
	}
}

// corsFails is an object store whose CORS call fails, to drive a create's
// rollback.
type corsFails struct{ *fake.Store }

var errCORS = errors.New("cors refused")

func (corsFails) SetCORS(context.Context, string, []string) error { return errCORS }

func TestCreateRollsBack(t *testing.T) {
	t.Parallel()
	objects := corsFails{fake.New()}
	f := newFixture(t, objects)
	ctx := context.Background()

	if _, err := f.svc.Create(ctx, bucket.CreateInput{Name: "half-made", CreatedBy: adminID}); !errors.Is(err, errCORS) {
		t.Fatalf("Create: got %v, want the CORS error", err)
	}
	if _, err := objects.HeadBucket(ctx, "half-made"); !errors.Is(err, objectstore.ErrNotFound) {
		t.Errorf("the half-made bucket is still there: %v", err)
	}
	if _, err := f.db.GetBucketRow(ctx, sqlc.GetBucketRowParams{Name: "half-made"}); err == nil {
		t.Error("a row was recorded for a create that failed")
	}
}

func TestListJoinsTheObjectStoreWithKumosRows(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()

	if _, err := f.svc.Create(ctx, bucket.CreateInput{
		Name: "kumo-made", Tags: map[string]string{"team": "web"}, CreatedBy: adminID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.objects.CreateBucket(ctx, "elsewhere"); err != nil {
		t.Fatal(err)
	}

	list, err := f.svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Name != "elsewhere" || list[1].Name != "kumo-made" {
		t.Fatalf("List: got %+v", list)
	}
	if list[0].CreatedBy != nil || list[0].State != bucket.StateAvailable || len(list[0].Tags) != 0 {
		t.Errorf("a bucket made outside Kumo: got %+v", list[0])
	}
	if list[1].CreatedBy == nil || *list[1].CreatedBy != "ada" || list[1].Tags["team"] != "web" {
		t.Errorf("a bucket made in Kumo: got %+v", list[1])
	}
}

func TestListDropsTheRowOfABucketDeletedElsewhere(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()

	if _, err := f.svc.Create(ctx, bucket.CreateInput{
		Name: "reused", Tags: map[string]string{"owner": "team-a"}, CreatedBy: adminID,
	}); err != nil {
		t.Fatal(err)
	}
	// Another client deletes it and someone recreates the name.
	if err := f.objects.DeleteBucket(ctx, "reused"); err != nil {
		t.Fatal(err)
	}
	if list, err := f.svc.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("List after an outside delete: %+v, %v", list, err)
	}
	if err := f.objects.CreateBucket(ctx, "reused"); err != nil {
		t.Fatal(err)
	}
	list, err := f.svc.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %+v, %v", list, err)
	}
	if list[0].CreatedBy != nil || len(list[0].Tags) != 0 {
		t.Errorf("the recreated bucket inherited the old metadata: %+v", list[0])
	}
}

func TestGet(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()

	for _, name := range []string{"missing", "NOT-A-NAME"} {
		if _, err := f.svc.Get(ctx, name); !errors.Is(err, service.ErrNotFound) {
			t.Errorf("Get(%s): got %v, want ErrNotFound", name, err)
		}
	}
	if err := f.objects.CreateBucket(ctx, "elsewhere"); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Get(ctx, "elsewhere")
	if err != nil || got.Name != "elsewhere" || got.Endpoint != publicURL {
		t.Errorf("Get(elsewhere): %+v, %v", got, err)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()
	if err := f.objects.CreateBucket(ctx, "elsewhere"); err != nil {
		t.Fatal(err)
	}
	enabled, disabled := true, false

	// Turning versioning off on a bucket that never had it changes nothing.
	got, err := f.svc.Update(ctx, "elsewhere", bucket.UpdateInput{Versioning: &disabled})
	if err != nil || got.Versioning != objectstore.VersioningOff {
		t.Fatalf("Update(versioning off on a new bucket): %+v, %v", got.Versioning, err)
	}

	// Tagging a bucket made outside Kumo adopts it.
	got, err = f.svc.Update(ctx, "elsewhere", bucket.UpdateInput{
		Versioning: &enabled, SetTags: true, Tags: map[string]string{"env": "dev"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Versioning != objectstore.VersioningEnabled || got.Tags["env"] != "dev" || got.CreatedBy != nil {
		t.Errorf("Update: got %+v", got)
	}

	got, err = f.svc.Update(ctx, "elsewhere", bucket.UpdateInput{Versioning: &disabled})
	if err != nil || got.Versioning != objectstore.VersioningSuspended || got.Tags["env"] != "dev" {
		t.Errorf("Update(suspend): %+v, %v", got, err)
	}

	got, err = f.svc.Update(ctx, "elsewhere", bucket.UpdateInput{SetTags: true, Tags: map[string]string{}})
	if err != nil || len(got.Tags) != 0 {
		t.Errorf("Update(tags {}): %+v, %v", got.Tags, err)
	}

	if _, err := f.svc.Update(ctx, "missing", bucket.UpdateInput{Versioning: &enabled}); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Update(missing): got %v, want ErrNotFound", err)
	}
	var verr *service.ValidationError
	if _, err := f.svc.Update(ctx, "elsewhere", bucket.UpdateInput{
		SetTags: true, Tags: map[string]string{"": "x"},
	}); !errors.As(err, &verr) {
		t.Errorf("Update(bad tags): got %v, want a ValidationError", err)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()

	if _, err := f.svc.Create(ctx, bucket.CreateInput{Name: "empty", Tags: map[string]string{"a": "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Delete(ctx, "empty"); err != nil {
		t.Fatalf("Delete(empty): %v", err)
	}
	if _, err := f.svc.Get(ctx, "empty"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get after Delete: got %v", err)
	}
	if tags, _ := f.db.ListResourceTags(ctx, sqlc.ListResourceTagsParams{ResourceID: bucket.ResourceID("empty")}); len(tags) != 0 {
		t.Errorf("the tags outlived the bucket: %v", tags)
	}

	if _, err := f.svc.Create(ctx, bucket.CreateInput{Name: "full"}); err != nil {
		t.Fatal(err)
	}
	put(t, f.objects, "full", "a.txt")
	if err := f.svc.Delete(ctx, "full"); !errors.Is(err, service.ErrConflict) {
		t.Errorf("Delete(non-empty): got %v, want ErrConflict", err)
	}
	if err := f.svc.Delete(ctx, "missing"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Delete(missing): got %v, want ErrNotFound", err)
	}
}

func TestForceDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()

	if _, err := f.svc.Create(ctx, bucket.CreateInput{Name: "full", Tags: map[string]string{"a": "b"}}); err != nil {
		t.Fatal(err)
	}
	put(t, f.objects, "full", "a.txt")
	put(t, f.objects, "full", "dir/b.txt")

	deleted, err := f.svc.ForceDelete(ctx, "full")
	if err != nil {
		t.Fatalf("ForceDelete: %v", err)
	}
	if deleted.Bucket.State != bucket.StateDeleting || deleted.TaskID == "" {
		t.Errorf("ForceDelete: got %+v", deleted)
	}
	queued, err := f.tasks.Get(ctx, deleted.TaskID)
	if err != nil || queued.Type != task.TypeBucketDelete || queued.ResourceID == nil ||
		*queued.ResourceID != bucket.ResourceID("full") {
		t.Errorf("the queued task: %+v, %v", queued, err)
	}

	// While it is deleting, other writes are refused.
	enabled := true
	if _, err := f.svc.Update(ctx, "full", bucket.UpdateInput{Versioning: &enabled}); !errors.Is(err, service.ErrConflict) {
		t.Errorf("Update while deleting: got %v, want ErrConflict", err)
	}
	if err := f.svc.Delete(ctx, "full"); !errors.Is(err, service.ErrConflict) {
		t.Errorf("Delete while deleting: got %v, want ErrConflict", err)
	}

	run := worker.Task{ID: deleted.TaskID, Type: task.TypeBucketDelete, Payload: payload(t, "full")}
	if err := f.svc.DeleteHandler(ctx, run); err != nil {
		t.Fatalf("DeleteHandler: %v", err)
	}
	if _, err := f.objects.HeadBucket(ctx, "full"); !errors.Is(err, objectstore.ErrNotFound) {
		t.Errorf("the bucket survived its delete task: %v", err)
	}
	if _, err := f.db.GetBucketRow(ctx, sqlc.GetBucketRowParams{Name: "full"}); err == nil {
		t.Error("the row survived the delete task")
	}
	// A retried task finds nothing to do.
	if err := f.svc.DeleteHandler(ctx, run); err != nil {
		t.Errorf("DeleteHandler a second time: %v", err)
	}

	if err := f.svc.DeleteHandler(ctx, worker.Task{ID: "task-x", Payload: json.RawMessage(`{}`)}); !errors.Is(err, bucket.ErrBadTaskPayload) {
		t.Errorf("DeleteHandler without a name: got %v", err)
	}
}

// TestForceDeleteAdoptsABucketMadeElsewhere checks a forced delete works on a
// bucket Kumo has no row for.
func TestForceDeleteAdoptsABucketMadeElsewhere(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()
	if err := f.objects.CreateBucket(ctx, "elsewhere"); err != nil {
		t.Fatal(err)
	}
	put(t, f.objects, "elsewhere", "k")

	deleted, err := f.svc.ForceDelete(ctx, "elsewhere")
	if err != nil || deleted.Bucket.State != bucket.StateDeleting {
		t.Fatalf("ForceDelete: %+v, %v", deleted, err)
	}
	if err := f.svc.DeleteHandler(ctx, worker.Task{ID: deleted.TaskID, Payload: payload(t, "elsewhere")}); err != nil {
		t.Fatalf("DeleteHandler: %v", err)
	}
	if _, err := f.objects.HeadBucket(ctx, "elsewhere"); !errors.Is(err, objectstore.ErrNotFound) {
		t.Errorf("the bucket survived: %v", err)
	}
}

func TestUnavailableObjectStore(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()
	f.objects.SetDown(true)

	if _, err := f.svc.List(ctx); !errors.Is(err, service.ErrUnavailable) {
		t.Errorf("List: got %v, want ErrUnavailable", err)
	}
	if _, err := f.svc.Create(ctx, bucket.CreateInput{Name: "logs"}); !errors.Is(err, service.ErrUnavailable) {
		t.Errorf("Create: got %v, want ErrUnavailable", err)
	}
	if _, err := f.svc.Get(ctx, "logs"); !errors.Is(err, service.ErrUnavailable) {
		t.Errorf("Get: got %v, want ErrUnavailable", err)
	}
}

func put(t *testing.T, objects *fake.Store, name, key string) {
	t.Helper()
	if err := objects.PutObject(context.Background(), name, key, bytes.NewReader([]byte("x")), 1, ""); err != nil {
		t.Fatal(err)
	}
}

func payload(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
