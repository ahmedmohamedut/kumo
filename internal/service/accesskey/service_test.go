package accesskey_test

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	"github.com/ahmedmohamedut/kumo/internal/objectstore/fake"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/accesskey"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
)

const adminID = "0190a0a0-0000-7000-8000-000000000001"

type fixture struct {
	svc     *accesskey.Service
	db      *store.Store
	objects *fake.Store
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
	f := fixture{db: db}
	if objects == nil {
		f.objects = fake.New()
		objects = f.objects
	}
	for _, b := range []string{"logs", "media", "backups"} {
		if err := objects.CreateBucket(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	f.svc, err = accesskey.New(accesskey.Options{Logger: logging.Discard(), Store: db, Objects: objects})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCreate(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()

	created, err := f.svc.Create(ctx, accesskey.CreateInput{
		Name: "web-app", Level: objectstore.AccessReadWrite, Buckets: []string{"media", "logs", "media"}, CreatedBy: adminID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !regexp.MustCompile(`^KUMO[A-Z2-7]{16}$`).MatchString(created.AccessKey.AccessKey) {
		t.Errorf("access key id %q", created.AccessKey.AccessKey)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{40}$`).MatchString(created.SecretKey) {
		t.Errorf("secret %q is not 40 URL-safe characters", created.SecretKey)
	}
	if created.Name != "web-app" || created.Level != objectstore.AccessReadWrite || created.State != accesskey.StateActive ||
		!slices.Equal(created.Buckets, []string{"logs", "media"}) || created.CreatedBy == nil || *created.CreatedBy != "ada" {
		t.Errorf("created: %+v", created.AccessKey)
	}

	secret, grant, ok := f.objects.AccessKey(created.AccessKey.AccessKey)
	if !ok || secret != created.SecretKey || grant.Level != objectstore.AccessReadWrite ||
		!slices.Equal(grant.Buckets, []string{"logs", "media"}) {
		t.Errorf("on the object store: %q %+v %v", secret, grant, ok)
	}

	// The secret is returned once and never readable again.
	got, err := f.svc.Get(ctx, created.AccessKey.AccessKey)
	if err != nil || !reflect.DeepEqual(got, created.AccessKey) {
		t.Errorf("Get: %+v, %v", got, err)
	}

	// Two keys never share an id or a secret.
	second, err := f.svc.Create(ctx, accesskey.CreateInput{Name: "batch", Level: objectstore.AccessReadOnly, Buckets: []string{"logs"}})
	if err != nil || second.AccessKey.AccessKey == created.AccessKey.AccessKey || second.SecretKey == created.SecretKey {
		t.Errorf("second key: %+v, %v", second, err)
	}

	if _, err := f.svc.Create(ctx, accesskey.CreateInput{Name: "web-app", Level: objectstore.AccessReadOnly, Buckets: []string{"logs"}}); !errors.Is(err, service.ErrAlreadyExists) {
		t.Errorf("Create(taken name): got %v", err)
	}
}

func TestCreateRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	tooMany := make([]string, 101)
	for i := range tooMany {
		tooMany[i] = "b" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	for _, tt := range []struct {
		name  string
		in    accesskey.CreateInput
		field string
	}{
		{"no name", accesskey.CreateInput{Level: objectstore.AccessReadOnly, Buckets: []string{"logs"}}, "name"},
		{"bad name", accesskey.CreateInput{Name: "web app", Level: objectstore.AccessReadOnly, Buckets: []string{"logs"}}, "name"},
		{"bad level", accesskey.CreateInput{Name: "a", Level: "admin", Buckets: []string{"logs"}}, "level"},
		{"no buckets", accesskey.CreateInput{Name: "a", Level: objectstore.AccessReadOnly}, "buckets"},
		{"unknown bucket", accesskey.CreateInput{Name: "a", Level: objectstore.AccessReadOnly, Buckets: []string{"logs", "nope"}}, "buckets"},
		{"too many buckets", accesskey.CreateInput{Name: "a", Level: objectstore.AccessReadOnly, Buckets: tooMany}, "buckets"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var verr *service.ValidationError
			_, err := f.svc.Create(context.Background(), tt.in)
			if !errors.As(err, &verr) || verr.Fields[0].Field != tt.field {
				t.Fatalf("got %v, want a %s error", err, tt.field)
			}
		})
	}
	if keys, _ := f.objects.ListAccessKeys(context.Background()); len(keys) != 0 {
		t.Errorf("a refused create reached the object store: %v", keys)
	}
}

func TestListShowsKeysGoneFromTheObjectStore(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()
	kept, _ := f.svc.Create(ctx, accesskey.CreateInput{Name: "b-kept", Level: objectstore.AccessReadOnly, Buckets: []string{"logs"}})
	lost, _ := f.svc.Create(ctx, accesskey.CreateInput{Name: "a-lost", Level: objectstore.AccessReadOnly, Buckets: []string{"logs"}})
	// An operator removes one with another tool; one made elsewhere is not Kumo's.
	if err := f.objects.DeleteAccessKey(ctx, lost.AccessKey.AccessKey); err != nil {
		t.Fatal(err)
	}
	if err := f.objects.CreateAccessKey(ctx, objectstore.AccessKeySpec{
		AccessKey: "OUTSIDE", SecretKey: "s", Grant: objectstore.Grant{Buckets: []string{"logs"}, Level: objectstore.AccessReadOnly},
	}); err != nil {
		t.Fatal(err)
	}

	list, err := f.svc.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List: %+v, %v", list, err)
	}
	if list[0].Name != "a-lost" || list[0].State != accesskey.StateMissing ||
		list[1].AccessKey != kept.AccessKey.AccessKey || list[1].State != accesskey.StateActive {
		t.Errorf("List: %+v", list)
	}

	// A missing key cannot be re-scoped, only deleted.
	ro := objectstore.AccessReadWrite
	if _, err := f.svc.Update(ctx, lost.AccessKey.AccessKey, accesskey.UpdateInput{Level: &ro}); !errors.Is(err, service.ErrConflict) {
		t.Errorf("Update of a missing key: got %v", err)
	}
	if err := f.svc.Delete(ctx, lost.AccessKey.AccessKey); err != nil {
		t.Errorf("Delete of a missing key: %v", err)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()
	key, err := f.svc.Create(ctx, accesskey.CreateInput{Name: "app", Level: objectstore.AccessReadOnly, Buckets: []string{"logs"}})
	if err != nil {
		t.Fatal(err)
	}
	id := key.AccessKey.AccessKey
	if _, err := f.svc.Create(ctx, accesskey.CreateInput{Name: "other", Level: objectstore.AccessReadOnly, Buckets: []string{"logs"}}); err != nil {
		t.Fatal(err)
	}

	rw := objectstore.AccessReadWrite
	buckets := []string{"media", "backups"}
	name := "app-v2"
	got, err := f.svc.Update(ctx, id, accesskey.UpdateInput{Name: &name, Level: &rw, Buckets: &buckets})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Name != "app-v2" || got.Level != rw || !slices.Equal(got.Buckets, []string{"backups", "media"}) {
		t.Errorf("Update: %+v", got)
	}
	secret, grant, _ := f.objects.AccessKey(id)
	if secret != key.SecretKey || grant.Level != rw || !slices.Equal(grant.Buckets, []string{"backups", "media"}) {
		t.Errorf("the policy on the object store: %+v (secret kept: %v)", grant, secret == key.SecretKey)
	}

	taken := "other"
	if _, err := f.svc.Update(ctx, id, accesskey.UpdateInput{Name: &taken}); !errors.Is(err, service.ErrAlreadyExists) {
		t.Errorf("Update to a taken name: got %v", err)
	}
	unknown := []string{"nope"}
	var verr *service.ValidationError
	if _, err := f.svc.Update(ctx, id, accesskey.UpdateInput{Buckets: &unknown}); !errors.As(err, &verr) {
		t.Errorf("Update to an unknown bucket: got %v", err)
	}
	if _, err := f.svc.Update(ctx, "KUMONOPE", accesskey.UpdateInput{Name: &name}); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Update of a missing key: got %v", err)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()
	key, err := f.svc.Create(ctx, accesskey.CreateInput{Name: "app", Level: objectstore.AccessReadOnly, Buckets: []string{"logs"}})
	if err != nil {
		t.Fatal(err)
	}
	id := key.AccessKey.AccessKey
	if err := f.svc.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, ok := f.objects.AccessKey(id); ok {
		t.Error("the key still works on the object store")
	}
	if _, err := f.svc.Get(ctx, id); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get after Delete: got %v", err)
	}
	if err := f.svc.Delete(ctx, id); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Delete twice: got %v", err)
	}
}

func TestUnavailable(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.objects.SetDown(true)
	if _, err := f.svc.List(context.Background()); !errors.Is(err, service.ErrUnavailable) {
		t.Errorf("List: got %v", err)
	}
}
