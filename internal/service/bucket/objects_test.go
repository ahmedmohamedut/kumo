package bucket_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	"github.com/ahmedmohamedut/kumo/internal/objectstore/fake"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/bucket"
)

func TestListObjects(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, bucket.CreateInput{Name: "media"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a.txt", "photos/", "photos/1.jpg", "photos/2.jpg", "photos/raw/x.cr2", "z.txt"} {
		put(t, f.objects, "media", key)
	}

	root, err := f.svc.ListObjects(ctx, "media", bucket.ListObjectsInput{})
	if err != nil {
		t.Fatalf("ListObjects(root): %v", err)
	}
	if !slices.Equal(root.Folders, []string{"photos/"}) || !slices.Equal(keys(root), []string{"a.txt", "z.txt"}) ||
		root.NextCursor != nil {
		t.Errorf("root: %+v", root)
	}

	// The folder's own marker is not listed as an object inside it.
	photos, err := f.svc.ListObjects(ctx, "media", bucket.ListObjectsInput{Prefix: "photos/"})
	if err != nil {
		t.Fatalf("ListObjects(photos/): %v", err)
	}
	if photos.Prefix != "photos/" || !slices.Equal(photos.Folders, []string{"photos/raw/"}) ||
		!slices.Equal(keys(photos), []string{"photos/1.jpg", "photos/2.jpg"}) {
		t.Errorf("photos/: %+v", photos)
	}
	if photos.Objects[0].SizeBytes != 1 || photos.Objects[0].ETag == "" || photos.Objects[0].LastModified.IsZero() {
		t.Errorf("object metadata: %+v", photos.Objects[0])
	}

	first, err := f.svc.ListObjects(ctx, "media", bucket.ListObjectsInput{Limit: 2})
	if err != nil || first.NextCursor == nil {
		t.Fatalf("a page of 2: %+v, %v", first, err)
	}
	rest, err := f.svc.ListObjects(ctx, "media", bucket.ListObjectsInput{Limit: 2, Cursor: *first.NextCursor})
	if err != nil || len(rest.Objects)+len(rest.Folders) == 0 {
		t.Errorf("the next page: %+v, %v", rest, err)
	}

	for _, in := range []bucket.ListObjectsInput{{Limit: -1}, {Limit: 1001}, {Prefix: "/abs"}, {Prefix: "bad\x00"}} {
		var verr *service.ValidationError
		if _, err := f.svc.ListObjects(ctx, "media", in); !errors.As(err, &verr) {
			t.Errorf("ListObjects(%+v): got %v, want a ValidationError", in, err)
		}
	}
	if _, err := f.svc.ListObjects(ctx, "missing", bucket.ListObjectsInput{}); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("ListObjects(missing bucket): got %v", err)
	}
}

func TestPresignUpload(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()
	// Made outside Kumo, so it has no CORS rule until the first upload.
	if err := f.objects.CreateBucket(ctx, "elsewhere"); err != nil {
		t.Fatal(err)
	}

	got, err := f.svc.PresignUpload(ctx, "elsewhere", "docs/report 1.pdf", "application/pdf")
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	if got.Method != http.MethodPut || !strings.HasPrefix(got.URL, fake.PublicURL+"/elsewhere/docs/report%201.pdf") ||
		got.Headers["Content-Type"] != "application/pdf" {
		t.Errorf("PresignUpload: %+v", got)
	}
	if left := time.Until(got.ExpiresAt); left <= 0 || left > bucket.DefaultPresignTTL+time.Second {
		t.Errorf("ExpiresAt is %s away, want about %s", left, bucket.DefaultPresignTTL)
	}
	if cors := f.objects.CORS("elsewhere"); !slices.Equal(cors, origins) {
		t.Errorf("the first upload did not apply the CORS rule: %v", cors)
	}

	// A missing content type is the generic one.
	got, err = f.svc.PresignUpload(ctx, "elsewhere", "blob", "")
	if err != nil || got.Headers["Content-Type"] != "application/octet-stream" {
		t.Errorf("PresignUpload without a type: %+v, %v", got, err)
	}

	for _, tt := range []struct{ key, contentType, field string }{
		{"", "text/plain", "key"},
		{"/abs", "text/plain", "key"},
		{"folder/", "text/plain", "key"},
		{strings.Repeat("k", 1025), "text/plain", "key"},
		{"ok", "not a type", "content_type"},
	} {
		var verr *service.ValidationError
		_, err := f.svc.PresignUpload(ctx, "elsewhere", tt.key, tt.contentType)
		if !errors.As(err, &verr) || verr.Fields[0].Field != tt.field {
			t.Errorf("PresignUpload(%q, %q): got %v, want a %s error", tt.key, tt.contentType, err, tt.field)
		}
	}
	if _, err := f.svc.PresignUpload(ctx, "missing", "k", ""); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("PresignUpload(missing bucket): got %v", err)
	}
}

func TestPresignDownload(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, bucket.CreateInput{Name: "media"}); err != nil {
		t.Fatal(err)
	}
	put(t, f.objects, "media", "photos/cat.jpg")

	got, err := f.svc.PresignDownload(ctx, "media", "photos/cat.jpg")
	if err != nil {
		t.Fatalf("PresignDownload: %v", err)
	}
	if got.Method != http.MethodGet || !strings.Contains(got.URL, "/media/photos/cat.jpg?") ||
		!strings.Contains(got.URL, "filename%3D%22cat.jpg%22") {
		t.Errorf("PresignDownload: %+v", got)
	}
	if _, err := f.svc.PresignDownload(ctx, "media", "photos/dog.jpg"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("PresignDownload(missing key): got %v, want ErrNotFound", err)
	}
}

func TestCreateFolderAndDeleteObjects(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, bucket.CreateInput{Name: "media"}); err != nil {
		t.Fatal(err)
	}

	for in, want := range map[string]string{"photos": "photos/", "docs/2026/": "docs/2026/"} {
		got, err := f.svc.CreateFolder(ctx, "media", in)
		if err != nil || got != want {
			t.Errorf("CreateFolder(%q): %q, %v, want %q", in, got, err, want)
		}
	}
	if o, err := f.objects.HeadObject(ctx, "media", "photos/"); err != nil || o.SizeBytes != 0 {
		t.Errorf("the folder marker: %+v, %v", o, err)
	}
	for _, bad := range []string{"", "/", "/abs"} {
		var verr *service.ValidationError
		if _, err := f.svc.CreateFolder(ctx, "media", bad); !errors.As(err, &verr) {
			t.Errorf("CreateFolder(%q): got %v, want a ValidationError", bad, err)
		}
	}

	put(t, f.objects, "media", "photos/a.jpg")
	// Deleting a folder removes its marker, never what is under it.
	if err := f.svc.DeleteObjects(ctx, "media", []string{"photos/", "never-was"}); err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if _, err := f.objects.HeadObject(ctx, "media", "photos/"); !errors.Is(err, objectstore.ErrNotFound) {
		t.Errorf("the marker survived: %v", err)
	}
	if _, err := f.objects.HeadObject(ctx, "media", "photos/a.jpg"); err != nil {
		t.Errorf("an object below the deleted folder went with it: %v", err)
	}

	var verr *service.ValidationError
	if err := f.svc.DeleteObjects(ctx, "media", nil); !errors.As(err, &verr) {
		t.Errorf("DeleteObjects(none): got %v", err)
	}
	if err := f.svc.DeleteObjects(ctx, "media", make([]string, 1001)); !errors.As(err, &verr) {
		t.Errorf("DeleteObjects(1001): got %v", err)
	}
}

func TestObjectWritesRefusedWhileDeleting(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, bucket.CreateInput{Name: "going"}); err != nil {
		t.Fatal(err)
	}
	put(t, f.objects, "going", "k")
	if _, err := f.svc.ForceDelete(ctx, "going"); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.PresignUpload(ctx, "going", "k2", ""); !errors.Is(err, service.ErrConflict) {
		t.Errorf("PresignUpload: got %v, want ErrConflict", err)
	}
	if _, err := f.svc.CreateFolder(ctx, "going", "f"); !errors.Is(err, service.ErrConflict) {
		t.Errorf("CreateFolder: got %v, want ErrConflict", err)
	}
	if err := f.svc.DeleteObjects(ctx, "going", []string{"k"}); !errors.Is(err, service.ErrConflict) {
		t.Errorf("DeleteObjects: got %v, want ErrConflict", err)
	}
	// Reads stay open: an operator may still download what is about to go.
	if _, err := f.svc.PresignDownload(ctx, "going", "k"); err != nil {
		t.Errorf("PresignDownload while deleting: %v", err)
	}
}

func keys(l bucket.Listing) []string {
	out := make([]string, 0, len(l.Objects))
	for _, o := range l.Objects {
		out = append(out, o.Key)
	}
	return out
}
