//go:build rustfs

package rustfs_test

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

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
