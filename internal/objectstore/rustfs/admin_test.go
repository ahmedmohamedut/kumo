package rustfs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/objectstore"
)

const testSecret = "admin-secret"

func TestPolicyDocument(t *testing.T) {
	for _, tt := range []struct {
		level         objectstore.AccessLevel
		bucketActions []string
		objectActions []string
	}{
		{objectstore.AccessReadOnly,
			[]string{"s3:ListBucket", "s3:GetBucketLocation"},
			[]string{"s3:GetObject"}},
		{objectstore.AccessReadWrite,
			[]string{"s3:ListBucket", "s3:GetBucketLocation", "s3:ListBucketMultipartUploads"},
			[]string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"}},
	} {
		doc, err := policyDocument(objectstore.Grant{Buckets: []string{"logs", "app"}, Level: tt.level})
		if err != nil {
			t.Fatal(err)
		}
		var policy struct {
			Statement []struct {
				Effect   string
				Action   []string
				Resource []string
			}
		}
		if err := json.Unmarshal(doc, &policy); err != nil {
			t.Fatal(err)
		}
		if len(policy.Statement) != 2 {
			t.Fatalf("%s: %s", tt.level, doc)
		}
		bucket, object := policy.Statement[0], policy.Statement[1]
		if !slices.Equal(bucket.Action, tt.bucketActions) ||
			!slices.Equal(bucket.Resource, []string{"arn:aws:s3:::app", "arn:aws:s3:::logs"}) {
			t.Errorf("%s bucket statement: %+v", tt.level, bucket)
		}
		if !slices.Equal(object.Action, tt.objectActions) ||
			!slices.Equal(object.Resource, []string{"arn:aws:s3:::app/*", "arn:aws:s3:::logs/*"}) {
			t.Errorf("%s object statement: %+v", tt.level, object)
		}
		if strings.Contains(string(doc), "DeleteBucket") || strings.Contains(string(doc), "s3:*") {
			t.Errorf("%s: the policy reaches past objects: %s", tt.level, doc)
		}
	}
	if _, err := policyDocument(objectstore.Grant{Level: objectstore.AccessReadOnly}); err == nil {
		t.Error("a grant without buckets: want an error")
	}
}

// adminServer records the admin calls it gets and answers each with a canned
// response keyed by the call name.
type adminServer struct {
	mu        sync.Mutex
	calls     []string
	bodies    map[string][]byte
	responses map[string]func(w http.ResponseWriter)
}

func newAdminServer(t *testing.T) (*adminServer, *Store) {
	t.Helper()
	a := &adminServer{bodies: map[string][]byte{}, responses: map[string]func(http.ResponseWriter){}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := strings.TrimPrefix(r.URL.Path, adminPrefix)
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.calls = append(a.calls, r.Method+" "+call+"?"+r.URL.RawQuery)
		a.bodies[call] = body
		respond := a.responses[call]
		a.mu.Unlock()
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if respond != nil {
			respond(w)
		}
	}))
	t.Cleanup(srv.Close)
	store, err := New(Options{Endpoint: srv.URL, AccessKey: "admin", SecretKey: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	return a, store
}

func fail(status int, code string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"Code":"` + code + `","Message":"canned"}`))
	}
}

func TestCreateAccessKey(t *testing.T) {
	a, store := newAdminServer(t)
	spec := objectstore.AccessKeySpec{
		AccessKey: "KUMOABCDEFGHIJKLMNOP", SecretKey: "s3cr3t-value",
		Grant: objectstore.Grant{Buckets: []string{"logs"}, Level: objectstore.AccessReadOnly},
	}
	if err := store.CreateAccessKey(context.Background(), spec); err != nil {
		t.Fatalf("CreateAccessKey: %v", err)
	}
	want := []string{
		"PUT add-canned-policy?name=kumo-kumoabcdefghijklmnop",
		"PUT add-user?accessKey=KUMOABCDEFGHIJKLMNOP",
		"PUT set-user-or-group-policy?isGroup=false&policyName=kumo-kumoabcdefghijklmnop&userOrGroup=KUMOABCDEFGHIJKLMNOP",
	}
	if !slices.Equal(a.calls, want) {
		t.Errorf("calls:\n%v\nwant\n%v", a.calls, want)
	}
	// The secret travels encrypted with the admin key, never in clear.
	sealed := a.bodies["add-user"]
	if strings.Contains(string(sealed), spec.SecretKey) {
		t.Fatal("the user's secret was sent in clear")
	}
	plain, err := decryptAdmin(testSecret, sealed)
	if err != nil || string(plain) != `{"secretKey":"s3cr3t-value","status":"enabled"}` {
		t.Errorf("the add-user body: %s, %v", plain, err)
	}
}

func TestCreateAccessKeyRollsBack(t *testing.T) {
	for _, tt := range []struct {
		failing string
		undo    []string
	}{
		{"add-user", []string{"DELETE remove-canned-policy?name=kumo-kumo1"}},
		{"set-user-or-group-policy", []string{"DELETE remove-user?accessKey=KUMO1", "DELETE remove-canned-policy?name=kumo-kumo1"}},
	} {
		t.Run(tt.failing, func(t *testing.T) {
			a, store := newAdminServer(t)
			a.responses[tt.failing] = fail(http.StatusInternalServerError, "XMinioInternalError")
			err := store.CreateAccessKey(context.Background(), objectstore.AccessKeySpec{
				AccessKey: "KUMO1", SecretKey: "s",
				Grant: objectstore.Grant{Buckets: []string{"b"}, Level: objectstore.AccessReadWrite},
			})
			if !errors.Is(err, ErrAdmin) {
				t.Fatalf("got %v, want ErrAdmin", err)
			}
			undone := a.calls[len(a.calls)-len(tt.undo):]
			if !slices.Equal(undone, tt.undo) {
				t.Errorf("undo calls: %v, want %v", undone, tt.undo)
			}
		})
	}
}

func TestDeleteAccessKeyToleratesMissing(t *testing.T) {
	a, store := newAdminServer(t)
	a.responses["remove-user"] = fail(http.StatusNotFound, "XMinioAdminNoSuchUser")
	a.responses["remove-canned-policy"] = fail(http.StatusNotFound, "XMinioAdminNoSuchPolicy")
	if err := store.DeleteAccessKey(context.Background(), "KUMO1"); err != nil {
		t.Errorf("DeleteAccessKey of a missing key: %v", err)
	}

	a.responses["remove-user"] = fail(http.StatusForbidden, "AccessDenied")
	if err := store.DeleteAccessKey(context.Background(), "KUMO1"); !errors.Is(err, objectstore.ErrUnavailable) {
		t.Errorf("DeleteAccessKey refused: got %v, want ErrUnavailable", err)
	}
}

func TestListAccessKeys(t *testing.T) {
	a, store := newAdminServer(t)
	sealed, err := encryptAdmin(testSecret, []byte(`{"KUMOB":{"status":"enabled"},"KUMOA":{"status":"enabled"}}`))
	if err != nil {
		t.Fatal(err)
	}
	a.responses["list-users"] = func(w http.ResponseWriter) { _, _ = w.Write(sealed) }

	got, err := store.ListAccessKeys(context.Background())
	if err != nil || !slices.Equal(got, []string{"KUMOA", "KUMOB"}) {
		t.Errorf("ListAccessKeys: %v, %v", got, err)
	}

	a.responses["list-users"] = func(w http.ResponseWriter) {
		_, _ = w.Write([]byte("not sealed at all, long enough to pass the size check"))
	}
	if _, err := store.ListAccessKeys(context.Background()); !errors.Is(err, ErrAdminCipher) {
		t.Errorf("ListAccessKeys of a bad body: got %v, want ErrAdminCipher", err)
	}
}
