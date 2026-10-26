// Package storetest is the behavior every objectstore.ObjectStore must
// share. The fake runs it in every `go test`; the RustFS client runs it
// against a real server when KUMO_TEST_S3_ENDPOINT is set, which is how the
// fake is kept honest.
package storetest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/objectstore"
)

// Options tunes the suite to the backend.
type Options struct {
	// PublicURL is the address presigned URLs must start with.
	PublicURL string
	// FollowPresigned sends real requests to the presigned URLs. Only a real
	// server can answer them.
	FollowPresigned bool
}

// Run exercises store. Every bucket it creates is named kumo-test-<random>
// and removed when the test ends, so it is safe against a server that holds
// other data.
func Run(t *testing.T, store objectstore.ObjectStore, opts Options) {
	t.Helper()
	ctx := context.Background()

	if err := store.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	name := bucketName(t)
	if err := store.CreateBucket(ctx, name); err != nil {
		t.Fatalf("CreateBucket(%s): %v", name, err)
	}
	t.Cleanup(func() { cleanup(t, store, name) })

	t.Run("buckets", func(t *testing.T) {
		if err := store.CreateBucket(ctx, name); !errors.Is(err, objectstore.ErrAlreadyExists) {
			t.Errorf("CreateBucket twice: got %v, want ErrAlreadyExists", err)
		}

		buckets, err := store.ListBuckets(ctx)
		if err != nil {
			t.Fatalf("ListBuckets: %v", err)
		}
		i := slices.IndexFunc(buckets, func(b objectstore.Bucket) bool { return b.Name == name })
		if i < 0 {
			t.Fatalf("ListBuckets: %s missing from %v", name, buckets)
		}
		if buckets[i].CreatedAt.IsZero() {
			t.Errorf("ListBuckets: %s has no creation time", name)
		}

		got, err := store.HeadBucket(ctx, name)
		if err != nil || got.Name != name || got.CreatedAt.IsZero() {
			t.Errorf("HeadBucket: got %+v, %v", got, err)
		}

		missing := bucketName(t)
		if _, err := store.HeadBucket(ctx, missing); !errors.Is(err, objectstore.ErrNotFound) {
			t.Errorf("HeadBucket(missing): got %v, want ErrNotFound", err)
		}
		if err := store.DeleteBucket(ctx, missing); !errors.Is(err, objectstore.ErrNotFound) {
			t.Errorf("DeleteBucket(missing): got %v, want ErrNotFound", err)
		}
	})

	t.Run("versioning", func(t *testing.T) {
		v, err := store.GetVersioning(ctx, name)
		if err != nil || v != objectstore.VersioningOff {
			t.Fatalf("GetVersioning of a new bucket: got %q, %v", v, err)
		}
		for _, step := range []struct {
			enabled bool
			want    objectstore.Versioning
		}{
			{true, objectstore.VersioningEnabled},
			{false, objectstore.VersioningSuspended},
		} {
			if err := store.SetVersioning(ctx, name, step.enabled); err != nil {
				t.Fatalf("SetVersioning(%v): %v", step.enabled, err)
			}
			if v, err := store.GetVersioning(ctx, name); err != nil || v != step.want {
				t.Errorf("GetVersioning after SetVersioning(%v): got %q, %v, want %q", step.enabled, v, err, step.want)
			}
		}
	})

	t.Run("cors", func(t *testing.T) {
		const origin = "https://console.example"
		if err := store.SetCORS(ctx, name, []string{origin}); err != nil {
			t.Fatalf("SetCORS: %v", err)
		}
		if opts.FollowPresigned {
			if got := preflight(t, opts.PublicURL+"/"+name+"/k", origin); got != origin {
				t.Errorf("preflight with a rule: Access-Control-Allow-Origin %q, want %q", got, origin)
			}
		}
		if err := store.SetCORS(ctx, name, nil); err != nil {
			t.Fatalf("SetCORS(none): %v", err)
		}
		if opts.FollowPresigned {
			if got := preflight(t, opts.PublicURL+"/"+name+"/k", origin); got != "" {
				t.Errorf("preflight without rules: Access-Control-Allow-Origin %q, want none", got)
			}
		}
		// The rules stay on for the presign subtest, as the console needs them.
		if err := store.SetCORS(ctx, name, []string{origin}); err != nil {
			t.Fatalf("SetCORS again: %v", err)
		}
	})

	keys := []string{"a.txt", "dir/b.txt", "dir/d.txt", "dir/sub/c.txt", "z/"}
	for _, key := range keys {
		body := []byte("hello " + key)
		if strings.HasSuffix(key, "/") {
			body = nil
		}
		if err := store.PutObject(ctx, name, key, bytes.NewReader(body), int64(len(body)), "text/plain"); err != nil {
			t.Fatalf("PutObject(%s): %v", key, err)
		}
	}

	t.Run("objects", func(t *testing.T) {
		o, err := store.HeadObject(ctx, name, "dir/b.txt")
		if err != nil {
			t.Fatalf("HeadObject: %v", err)
		}
		if o.SizeBytes != int64(len("hello dir/b.txt")) || o.ContentType != "text/plain" ||
			o.ETag == "" || o.LastModified.IsZero() {
			t.Errorf("HeadObject: got %+v", o)
		}
		if _, err := store.HeadObject(ctx, name, "nope"); !errors.Is(err, objectstore.ErrNotFound) {
			t.Errorf("HeadObject(missing): got %v, want ErrNotFound", err)
		}

		root, err := store.ListObjects(ctx, name, objectstore.ListInput{Delimiter: "/"})
		if err != nil {
			t.Fatalf("ListObjects(root): %v", err)
		}
		assertKeys(t, "root objects", objectKeys(root), []string{"a.txt"})
		assertKeys(t, "root prefixes", root.CommonPrefixes, []string{"dir/", "z/"})
		if root.NextCursor != "" {
			t.Errorf("root: a complete listing has cursor %q", root.NextCursor)
		}

		dir, err := store.ListObjects(ctx, name, objectstore.ListInput{Prefix: "dir/", Delimiter: "/"})
		if err != nil {
			t.Fatalf("ListObjects(dir/): %v", err)
		}
		assertKeys(t, "dir/ objects", objectKeys(dir), []string{"dir/b.txt", "dir/d.txt"})
		assertKeys(t, "dir/ prefixes", dir.CommonPrefixes, []string{"dir/sub/"})

		var all []string
		cursor := ""
		for range len(keys) + 1 {
			page, err := store.ListObjects(ctx, name, objectstore.ListInput{Limit: 2, Cursor: cursor})
			if err != nil {
				t.Fatalf("ListObjects(page): %v", err)
			}
			all = append(all, objectKeys(page)...)
			if cursor = page.NextCursor; cursor == "" {
				break
			}
		}
		assertKeys(t, "paged keys", all, keys)
	})

	t.Run("presign", func(t *testing.T) {
		put, err := store.PresignPut(ctx, name, "up/hello world.txt",
			objectstore.PresignPutInput{TTL: 5 * time.Minute, ContentType: "text/plain"})
		if err != nil {
			t.Fatalf("PresignPut: %v", err)
		}
		if put.Method != http.MethodPut || !strings.HasPrefix(put.URL, opts.PublicURL+"/") {
			t.Errorf("PresignPut: got %s %s, want PUT under %s", put.Method, put.URL, opts.PublicURL)
		}
		if _, ok := put.Headers["Host"]; ok {
			t.Errorf("PresignPut: headers carry Host: %v", put.Headers)
		}
		if time.Until(put.ExpiresAt) <= 0 || time.Until(put.ExpiresAt) > 5*time.Minute+time.Second {
			t.Errorf("PresignPut: expires at %s", put.ExpiresAt)
		}

		get, err := store.PresignGet(ctx, name, "up/hello world.txt",
			objectstore.PresignGetInput{TTL: time.Minute, Filename: "hello world.txt"})
		if err != nil {
			t.Fatalf("PresignGet: %v", err)
		}
		if get.Method != http.MethodGet || !strings.HasPrefix(get.URL, opts.PublicURL+"/") {
			t.Errorf("PresignGet: got %s %s", get.Method, get.URL)
		}

		if !opts.FollowPresigned {
			return
		}
		body := "uploaded by a browser"
		follow(t, put, strings.NewReader(body), http.StatusOK)
		resp := follow(t, get, nil, http.StatusOK)
		if resp != body {
			t.Errorf("download through the presigned URL: got %q, want %q", resp, body)
		}

		wrong := put
		wrong.Headers = map[string]string{"Content-Type": "application/json"}
		follow(t, wrong, strings.NewReader("{}"), http.StatusForbidden)
	})

	t.Run("delete", func(t *testing.T) {
		if err := store.DeleteBucket(ctx, name); !errors.Is(err, objectstore.ErrBucketNotEmpty) {
			t.Errorf("DeleteBucket(non-empty): got %v, want ErrBucketNotEmpty", err)
		}
		if err := store.DeleteObjects(ctx, name, []string{"a.txt", "never-existed"}); err != nil {
			t.Fatalf("DeleteObjects: %v", err)
		}
		if _, err := store.HeadObject(ctx, name, "a.txt"); !errors.Is(err, objectstore.ErrNotFound) {
			t.Errorf("HeadObject after delete: got %v, want ErrNotFound", err)
		}

		n, err := store.EmptyBucket(ctx, name)
		if err != nil {
			t.Fatalf("EmptyBucket: %v", err)
		}
		if n < len(keys)-1 {
			t.Errorf("EmptyBucket: removed %d entries, want at least %d", n, len(keys)-1)
		}
		if err := store.DeleteBucket(ctx, name); err != nil {
			t.Errorf("DeleteBucket after EmptyBucket: %v", err)
		}
		if _, err := store.EmptyBucket(ctx, name); !errors.Is(err, objectstore.ErrNotFound) {
			t.Errorf("EmptyBucket(deleted): got %v, want ErrNotFound", err)
		}
	})
}

// cleanup empties and removes a test bucket. The suite turns versioning on,
// so this is also the test that EmptyBucket clears old versions and delete
// markers: DeleteBucket refuses a bucket that still holds any.
func cleanup(t *testing.T, store objectstore.ObjectStore, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.EmptyBucket(ctx, name); err != nil {
		if !errors.Is(err, objectstore.ErrNotFound) {
			t.Errorf("cleanup: empty %s: %v", name, err)
		}
		return
	}
	if err := store.DeleteBucket(ctx, name); err != nil && !errors.Is(err, objectstore.ErrNotFound) {
		t.Errorf("cleanup: delete %s: %v", name, err)
	}
}

// preflight sends the OPTIONS request a browser sends before a cross-origin
// PUT and returns the origin the server allows.
func preflight(t *testing.T, rawURL, origin string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodOptions, rawURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPut)
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight %s: %v", rawURL, err)
	}
	resp.Body.Close() //nolint:errcheck,gosec // headers only
	return resp.Header.Get("Access-Control-Allow-Origin")
}

// follow sends the request a browser would and returns the body.
func follow(t *testing.T, p objectstore.Presigned, body io.Reader, want int) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), p.Method, p.URL, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", p.Method, p.URL, err)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", p.Method, p.URL, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Errorf("%s through a presigned URL: got %d (%s), want %d", p.Method, resp.StatusCode, data, want)
	}
	return string(data)
}

func bucketName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "kumo-test-" + hex.EncodeToString(b)
}

func objectKeys(page objectstore.ObjectPage) []string {
	keys := make([]string, 0, len(page.Objects))
	for _, o := range page.Objects {
		keys = append(keys, o.Key)
	}
	return keys
}

func assertKeys(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}
