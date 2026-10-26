//go:build rustfs

package rustfs_test

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	"github.com/ahmedmohamedut/kumo/internal/objectstore/rustfs"
	"github.com/ahmedmohamedut/kumo/internal/objectstore/storetest"
)

// TestConformance runs the shared suite against a real server, which is what
// keeps the fake honest. `make test-rustfs` runs it.
func TestConformance(t *testing.T) {
	store, endpoint := testStore(t)
	storetest.Run(t, store, storetest.Options{PublicURL: endpoint, FollowPresigned: true})
}

// TestCORSKeepsForeignRules checks SetCORS replaces only Kumo's own rule: an
// operator who wrote CORS rules for their own site keeps them.
func TestCORSKeepsForeignRules(t *testing.T) {
	store, endpoint := testStore(t)
	ctx := context.Background()
	name := "kumo-test-cors-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := store.CreateBucket(ctx, name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.DeleteBucket(context.Background(), name); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	if err := rustfs.PutRawCORS(ctx, store, name, "operator-site", "https://shop.example"); err != nil {
		t.Fatalf("put the operator's rule: %v", err)
	}
	for _, origins := range [][]string{{"https://kumo.example"}, {"https://kumo2.example"}} {
		if err := store.SetCORS(ctx, name, origins); err != nil {
			t.Fatalf("SetCORS(%v): %v", origins, err)
		}
	}
	for origin, want := range map[string]bool{
		"https://shop.example":  true,
		"https://kumo2.example": true,
		"https://kumo.example":  false,
	} {
		got := allows(t, endpoint+"/"+name+"/k", origin)
		if got != want {
			t.Errorf("after two SetCORS: %s allowed = %v, want %v", origin, got, want)
		}
	}

	if err := store.SetCORS(ctx, name, nil); err != nil {
		t.Fatalf("SetCORS(none): %v", err)
	}
	if !allows(t, endpoint+"/"+name+"/k", "https://shop.example") {
		t.Error("removing Kumo's rule removed the operator's too")
	}
}

func testStore(t *testing.T) (*rustfs.Store, string) {
	t.Helper()
	endpoint := os.Getenv("KUMO_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("KUMO_TEST_S3_ENDPOINT is not set")
	}
	store, err := rustfs.New(rustfs.Options{
		Endpoint:  endpoint,
		AccessKey: os.Getenv("KUMO_TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("KUMO_TEST_S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, endpoint
}

func allows(t *testing.T, rawURL, origin string) bool {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodOptions, rawURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPut)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck,gosec // headers only
	return resp.Header.Get("Access-Control-Allow-Origin") == origin
}

// TestAccessKeyIsEnforced is the security check of access keys: a read-only
// key reads its bucket and nothing else, a read-write key writes there but
// cannot delete the bucket, and a deleted key stops working.
func TestAccessKeyIsEnforced(t *testing.T) {
	store, endpoint := testStore(t)
	ctx := context.Background()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	mine, other := "kumo-test-key-a-"+suffix, "kumo-test-key-b-"+suffix
	for _, name := range []string{mine, other} {
		if err := store.CreateBucket(ctx, name); err != nil {
			t.Fatal(err)
		}
		if err := store.PutObject(ctx, name, "k", strings.NewReader("x"), 1, ""); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = store.EmptyBucket(context.Background(), name)
			_ = store.DeleteBucket(context.Background(), name)
		})
	}

	accessKey, secret := "KUMOTEST"+strings.ToUpper(suffix), "kumo-test-secret-"+suffix
	if err := store.CreateAccessKey(ctx, objectstore.AccessKeySpec{
		AccessKey: accessKey, SecretKey: secret,
		Grant: objectstore.Grant{Buckets: []string{mine}, Level: objectstore.AccessReadOnly},
	}); err != nil {
		t.Fatalf("CreateAccessKey: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteAccessKey(context.Background(), accessKey) })

	as, err := rustfs.New(rustfs.Options{Endpoint: endpoint, AccessKey: accessKey, SecretKey: secret})
	if err != nil {
		t.Fatal(err)
	}
	expect := func(what string, err error, allowed bool) {
		t.Helper()
		switch {
		case allowed && err != nil:
			t.Errorf("%s: refused (%v), want allowed", what, err)
		case !allowed && err == nil:
			t.Errorf("%s: allowed, want refused", what)
		}
	}

	_, err = as.HeadObject(ctx, mine, "k")
	expect("read-only: read its bucket", err, true)
	_, err = as.ListObjects(ctx, mine, objectstore.ListInput{})
	expect("read-only: list its bucket", err, true)
	expect("read-only: write its bucket", as.PutObject(ctx, mine, "new", strings.NewReader("x"), 1, ""), false)
	_, err = as.HeadObject(ctx, other, "k")
	expect("read-only: read another bucket", err, false)

	if err := store.SetAccessKeyPolicy(ctx, accessKey, objectstore.Grant{
		Buckets: []string{mine}, Level: objectstore.AccessReadWrite,
	}); err != nil {
		t.Fatalf("SetAccessKeyPolicy: %v", err)
	}
	expect("read-write: write its bucket", as.PutObject(ctx, mine, "new", strings.NewReader("x"), 1, ""), true)
	expect("read-write: delete in its bucket", as.DeleteObjects(ctx, mine, []string{"new"}), true)
	expect("read-write: write another bucket", as.PutObject(ctx, other, "new", strings.NewReader("x"), 1, ""), false)
	expect("read-write: delete its bucket", as.DeleteBucket(ctx, mine), false)
	expect("read-write: create a bucket", as.CreateBucket(ctx, "kumo-test-key-c-"+suffix), false)

	if err := store.DeleteAccessKey(ctx, accessKey); err != nil {
		t.Fatalf("DeleteAccessKey: %v", err)
	}
	_, err = as.HeadObject(ctx, mine, "k")
	expect("deleted key: read its bucket", err, false)
}
