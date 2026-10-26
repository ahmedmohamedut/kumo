package rustfs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/objectstore"
)

func TestNewRefusesBadOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want error
	}{
		{"no endpoint", Options{AccessKey: "a", SecretKey: "s"}, ErrNoEndpoint},
		{"not http", Options{Endpoint: "ftp://h:21", AccessKey: "a", SecretKey: "s"}, ErrNoEndpoint},
		{"relative", Options{Endpoint: "127.0.0.1:9000", AccessKey: "a", SecretKey: "s"}, ErrNoEndpoint},
		{"query", Options{Endpoint: "http://h:9000/?x=1", AccessKey: "a", SecretKey: "s"}, ErrNoEndpoint},
		{"bad public URL", Options{Endpoint: "http://h:9000", PublicURL: "s3.example", AccessKey: "a", SecretKey: "s"}, ErrNoEndpoint},
		{"no access key", Options{Endpoint: "http://h:9000", SecretKey: "s"}, ErrNoCredentials},
		{"no secret", Options{Endpoint: "http://h:9000", AccessKey: "a"}, ErrNoCredentials},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.opts); !errors.Is(err, tt.want) {
				t.Errorf("New: got %v, want %v", err, tt.want)
			}
		})
	}
}

// TestPresignUsesThePublicURL checks a URL is signed for the address a browser
// reaches, not the one kumod uses, and carries no SDK checksum parameters a
// browser could not satisfy.
func TestPresignUsesThePublicURL(t *testing.T) {
	store, err := New(Options{
		Endpoint:  "http://127.0.0.1:9000",
		PublicURL: "https://s3.kumo.example/",
		AccessKey: "AKIDEXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	ctx := context.Background()

	put, err := store.PresignPut(ctx, "photos", "2026/a b.jpg",
		objectstore.PresignPutInput{TTL: 15 * time.Minute, ContentType: "image/jpeg"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(put.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "s3.kumo.example" || u.Path != "/photos/2026/a b.jpg" {
		t.Errorf("PresignPut URL: got %s", put.URL)
	}
	if put.Method != http.MethodPut || put.Headers["Content-Type"] != "image/jpeg" {
		t.Errorf("PresignPut: got %s with %v", put.Method, put.Headers)
	}
	if _, ok := put.Headers["Host"]; ok {
		t.Errorf("PresignPut: Host is in %v", put.Headers)
	}
	q := u.Query()
	if q.Get("X-Amz-Expires") != "900" || q.Get("X-Amz-Credential") == "" || q.Get("X-Amz-Signature") == "" {
		t.Errorf("PresignPut query: got %v", q)
	}
	for key := range q {
		if strings.Contains(strings.ToLower(key), "checksum") {
			t.Errorf("PresignPut: the URL carries %s, which a browser upload cannot satisfy", key)
		}
	}
	if !put.ExpiresAt.Equal(now.Add(15 * time.Minute)) {
		t.Errorf("PresignPut ExpiresAt: got %s", put.ExpiresAt)
	}

	get, err := store.PresignGet(ctx, "photos", "2026/a b.jpg",
		objectstore.PresignGetInput{TTL: time.Minute, Filename: "a b.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	gu, _ := url.Parse(get.URL)
	if gu.Host != "s3.kumo.example" ||
		gu.Query().Get("response-content-disposition") != `attachment; filename="a b.jpg"; filename*=UTF-8''a%20b.jpg` {
		t.Errorf("PresignGet: got %s", get.URL)
	}

	for _, ttl := range []time.Duration{0, -time.Second, 8 * 24 * time.Hour} {
		if _, err := store.PresignGet(ctx, "photos", "k", objectstore.PresignGetInput{TTL: ttl}); err == nil {
			t.Errorf("PresignGet with TTL %s: want an error", ttl)
		}
	}
}

// TestErrorMapping drives the client against canned S3 error responses.
func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		call   func(*Store) error
		want   error
	}{
		{"no such bucket", 404, "NoSuchBucket", func(s *Store) error {
			_, err := s.ListObjects(context.Background(), "b", objectstore.ListInput{})
			return err
		}, objectstore.ErrNotFound},
		{"taken", 409, "BucketAlreadyOwnedByYou", func(s *Store) error {
			return s.CreateBucket(context.Background(), "b")
		}, objectstore.ErrAlreadyExists},
		{"taken elsewhere", 409, "BucketAlreadyExists", func(s *Store) error {
			return s.CreateBucket(context.Background(), "b")
		}, objectstore.ErrAlreadyExists},
		{"not empty", 409, "BucketNotEmpty", func(s *Store) error {
			return s.DeleteBucket(context.Background(), "b")
		}, objectstore.ErrBucketNotEmpty},
		{"invalid name", 400, "InvalidBucketName", func(s *Store) error {
			return s.CreateBucket(context.Background(), "B")
		}, objectstore.ErrInvalidName},
		{"bad credentials", 403, "InvalidAccessKeyId", func(s *Store) error {
			return s.Ping(context.Background())
		}, objectstore.ErrUnavailable},
		{"bad signature", 403, "SignatureDoesNotMatch", func(s *Store) error {
			return s.Ping(context.Background())
		}, objectstore.ErrUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + tt.code +
					`</Code><Message>canned</Message></Error>`))
			}))
			defer srv.Close()

			store, err := New(Options{Endpoint: srv.URL, AccessKey: "a", SecretKey: "s"})
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.call(store); !errors.Is(err, tt.want) {
				t.Errorf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestUnreachableIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL
	srv.Close()

	store, err := New(Options{Endpoint: endpoint, AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.Ping(ctx); !errors.Is(err, objectstore.ErrUnavailable) {
		t.Errorf("Ping of a closed port: got %v, want ErrUnavailable", err)
	}
}

func TestPutObjectBounds(t *testing.T) {
	store, err := New(Options{Endpoint: "http://127.0.0.1:1", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutObject(context.Background(), "b", "k", strings.NewReader(""), maxPutBytes+1, ""); err == nil {
		t.Error("PutObject over the bound: want an error")
	}
}
