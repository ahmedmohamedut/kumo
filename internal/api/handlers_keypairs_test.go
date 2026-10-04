package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
)

type keyPairBody struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Fingerprint string            `json:"fingerprint"`
	PublicKey   string            `json:"public_key"`
	PrivateKey  string            `json:"private_key"`
	Tags        map[string]string `json:"tags"`
	UsedBy      []string          `json:"used_by"`
}

type keyPairListBody struct {
	Items         []keyPairBody `json:"items"`
	NextPageToken *string       `json:"next_page_token"`
}

func TestKeyPairLifecycle(t *testing.T) {
	t.Parallel()

	router, db := newKeyPairRouter(t)
	login := setupUserAndLogin(t, router)

	created := doWithToken(t, router, http.MethodPost, "/api/v1/key-pairs",
		`{"name":"main","tags":{"env":"dev"}}`, login.access)
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /key-pairs status = %d, want 201 (body %s)", created.Code, created.Body)
	}

	var pair keyPairBody
	decode(t, created, &pair)
	if !strings.HasPrefix(pair.ID, "key-") {
		t.Errorf("id = %q, want a key- prefixed id", pair.ID)
	}
	if pair.Type != "ed25519" {
		t.Errorf("type = %q, want the ed25519 default", pair.Type)
	}
	if !strings.HasPrefix(pair.Fingerprint, "SHA256:") {
		t.Errorf("fingerprint = %q, want the OpenSSH SHA256: form", pair.Fingerprint)
	}
	if !strings.HasPrefix(pair.PublicKey, "ssh-ed25519 ") {
		t.Errorf("public_key = %q, want an authorized_keys line", pair.PublicKey)
	}
	if !strings.HasPrefix(pair.PrivateKey, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Errorf("private_key = %q, want an OpenSSH PEM", pair.PrivateKey)
	}
	if pair.Tags["env"] != "dev" {
		t.Errorf("tags = %v, want env=dev", pair.Tags)
	}

	// The private key shows up exactly once: in the create response only.
	list := doWithToken(t, router, http.MethodGet, "/api/v1/key-pairs", "", login.access)
	if list.Code != http.StatusOK {
		t.Fatalf("GET /key-pairs status = %d, want 200 (body %s)", list.Code, list.Body)
	}
	if strings.Contains(list.Body.String(), "PRIVATE KEY") {
		t.Errorf("the list response leaked a private key: %s", list.Body)
	}
	var page keyPairListBody
	decode(t, list, &page)
	if len(page.Items) != 1 || page.Items[0].ID != pair.ID {
		t.Fatalf("list = %+v, want the created pair", page.Items)
	}
	if page.NextPageToken != nil {
		t.Errorf("next_page_token = %q, want none with a single item", *page.NextPageToken)
	}

	detail := doWithToken(t, router, http.MethodGet, "/api/v1/key-pairs/"+pair.ID, "", login.access)
	if detail.Code != http.StatusOK {
		t.Fatalf("GET /key-pairs/{id} status = %d, want 200 (body %s)", detail.Code, detail.Body)
	}
	var full keyPairBody
	decode(t, detail, &full)
	if full.UsedBy == nil || len(full.UsedBy) != 0 {
		t.Errorf("used_by = %v, want an empty list, not null", full.UsedBy)
	}

	insertInstanceWithKey(t, db, pair.ID)

	// While a live instance uses the key, deleting is a conflict.
	inUse := doWithToken(t, router, http.MethodDelete, "/api/v1/key-pairs/"+pair.ID, "", login.access)
	if inUse.Code != http.StatusConflict {
		t.Fatalf("DELETE in-use status = %d, want 409 (body %s)", inUse.Code, inUse.Body)
	}
	assertErrorCode(t, inUse, api.CodeConflict)

	// force=true overrides, exactly as in AWS.
	forced := doWithToken(t, router, http.MethodDelete, "/api/v1/key-pairs/"+pair.ID+"?force=true", "", login.access)
	if forced.Code != http.StatusNoContent {
		t.Fatalf("DELETE force status = %d, want 204 (body %s)", forced.Code, forced.Body)
	}
	if gone := doWithToken(t, router, http.MethodGet, "/api/v1/key-pairs/"+pair.ID, "", login.access); gone.Code != http.StatusNotFound {
		t.Errorf("GET after delete status = %d, want 404", gone.Code)
	}
}

func TestKeyPairCreateRSA(t *testing.T) {
	t.Parallel()

	router, _ := newKeyPairRouter(t)
	login := setupUserAndLogin(t, router)

	recorder := doWithToken(t, router, http.MethodPost, "/api/v1/key-pairs",
		`{"name":"legacy","type":"rsa"}`, login.access)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", recorder.Code, recorder.Body)
	}

	var pair keyPairBody
	decode(t, recorder, &pair)
	if pair.Type != "rsa" {
		t.Errorf("type = %q, want rsa", pair.Type)
	}
	if !strings.HasPrefix(pair.PublicKey, "ssh-rsa ") {
		t.Errorf("public_key = %q, want an ssh-rsa line", pair.PublicKey)
	}
}

func TestKeyPairCreateValidates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "a missing name", body: `{"type":"ed25519"}`, want: "name"},
		{name: "a name with a space", body: `{"name":"my key"}`, want: "name"},
		{name: "a 65 character name", body: `{"name":"` + strings.Repeat("a", 65) + `"}`, want: "name"},
		{name: "an unknown type", body: `{"name":"k","type":"dsa"}`, want: "type"},
		{name: "too many tags", body: tooManyTagsBody(), want: "tags"},
		{name: "not JSON", body: `{"name":`, want: "body"},
		{name: "not an object", body: `["k"]`, want: "body"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			router, _ := newKeyPairRouter(t)
			login := setupUserAndLogin(t, router)

			recorder := doWithToken(t, router, http.MethodPost, "/api/v1/key-pairs", tt.body, login.access)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body)
			}

			var envelope errorEnvelope
			decode(t, recorder, &envelope)
			if envelope.Error.Code != api.CodeInvalidArgument {
				t.Errorf("code = %q, want %q", envelope.Error.Code, api.CodeInvalidArgument)
			}
			var found bool
			for _, detail := range envelope.Error.Details {
				if detail.Field == tt.want {
					found = true
				}
			}
			if !found {
				t.Errorf("details = %+v, want an error on %q", envelope.Error.Details, tt.want)
			}
		})
	}
}

func TestKeyPairCreateRejectsADuplicateName(t *testing.T) {
	t.Parallel()

	router, _ := newKeyPairRouter(t)
	login := setupUserAndLogin(t, router)

	if r := doWithToken(t, router, http.MethodPost, "/api/v1/key-pairs", `{"name":"main"}`, login.access); r.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201 (body %s)", r.Code, r.Body)
	}

	recorder := doWithToken(t, router, http.MethodPost, "/api/v1/key-pairs/import",
		`{"name":"main","public_key":"`+fixtureEd25519PublicKey+`"}`, login.access)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("duplicate create status = %d, want 409 (body %s)", recorder.Code, recorder.Body)
	}
	assertErrorCode(t, recorder, api.CodeAlreadyExists)
}

func TestKeyPairImport(t *testing.T) {
	t.Parallel()

	router, _ := newKeyPairRouter(t)
	login := setupUserAndLogin(t, router)

	recorder := doWithToken(t, router, http.MethodPost, "/api/v1/key-pairs/import",
		`{"name":"laptop","public_key":"`+fixtureEd25519PublicKey+`"}`, login.access)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want 201 (body %s)", recorder.Code, recorder.Body)
	}

	var pair keyPairBody
	decode(t, recorder, &pair)
	if pair.Fingerprint != fixtureEd25519Fingerprint {
		t.Errorf("fingerprint = %q, want the fixture fingerprint %q", pair.Fingerprint, fixtureEd25519Fingerprint)
	}
	if pair.PrivateKey != "" {
		t.Error("an import response must never carry a private_key field")
	}
}

func TestKeyPairImportRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "garbage", body: `{"name":"k","public_key":"hello"}`, want: "public_key"},
		{
			name: "pasted private key material",
			body: `{"name":"k","public_key":"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjE=\n-----END OPENSSH PRIVATE KEY-----"}`,
			want: "public_key",
		},
		{name: "a missing public key", body: `{"name":"k"}`, want: "public_key"},
		{name: "a bad name", body: `{"name":"bad name","public_key":"` + fixtureEd25519PublicKey + `"}`, want: "name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			router, _ := newKeyPairRouter(t)
			login := setupUserAndLogin(t, router)

			recorder := doWithToken(t, router, http.MethodPost, "/api/v1/key-pairs/import", tt.body, login.access)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body)
			}

			var envelope errorEnvelope
			decode(t, recorder, &envelope)
			var found bool
			for _, detail := range envelope.Error.Details {
				if detail.Field == tt.want {
					found = true
				}
			}
			if !found {
				t.Errorf("details = %+v, want an error on %q", envelope.Error.Details, tt.want)
			}
		})
	}
}

func TestKeyPairUpdateTags(t *testing.T) {
	t.Parallel()

	router, _ := newKeyPairRouter(t)
	login := setupUserAndLogin(t, router)

	created := doWithToken(t, router, http.MethodPost, "/api/v1/key-pairs",
		`{"name":"main","tags":{"env":"dev"}}`, login.access)
	var pair keyPairBody
	decode(t, created, &pair)

	updated := doWithToken(t, router, http.MethodPatch, "/api/v1/key-pairs/"+pair.ID,
		`{"tags":{"env":"prod","owner":"ada"}}`, login.access)
	if updated.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200 (body %s)", updated.Code, updated.Body)
	}
	var patched keyPairBody
	decode(t, updated, &patched)
	if patched.Tags["env"] != "prod" || patched.Tags["owner"] != "ada" {
		t.Errorf("tags = %v, want the replaced set", patched.Tags)
	}

	empty := doWithToken(t, router, http.MethodPatch, "/api/v1/key-pairs/"+pair.ID, `{}`, login.access)
	if empty.Code != http.StatusBadRequest {
		t.Errorf("PATCH without tags status = %d, want 400 (body %s)", empty.Code, empty.Body)
	}

	unknown := doWithToken(t, router, http.MethodPatch, "/api/v1/key-pairs/key-00000000000000000",
		`{"tags":{}}`, login.access)
	if unknown.Code != http.StatusNotFound {
		t.Errorf("PATCH unknown status = %d, want 404", unknown.Code)
	}
}

func TestKeyPairWritesRequireAdmin(t *testing.T) {
	t.Parallel()

	router, _ := newKeyPairRouter(t)

	viewer, err := signerFor(t, router).IssueAccessToken("01J0000000000000000000000V", auth.RoleViewer)
	if err != nil {
		t.Fatalf("IssueAccessToken(): %v", err)
	}

	// The viewer reads fine.
	list := doWithToken(t, router, http.MethodGet, "/api/v1/key-pairs", "", viewer.Token)
	if list.Code != http.StatusOK {
		t.Fatalf("viewer GET /key-pairs status = %d, want 200 (body %s)", list.Code, list.Body)
	}

	for _, route := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/v1/key-pairs", `{"name":"main"}`},
		{http.MethodPost, "/api/v1/key-pairs/import", `{"name":"k","public_key":"k"}`},
		{http.MethodPatch, "/api/v1/key-pairs/key-00000000000000000", `{"tags":{}}`},
		{http.MethodDelete, "/api/v1/key-pairs/key-00000000000000000", ""},
	} {
		recorder := doWithToken(t, router, route.method, route.path, route.body, viewer.Token)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s status = %d, want 403 (body %s)",
				route.method, route.path, recorder.Code, recorder.Body)
		}
	}

	// No token at all is a 401, on reads and writes alike.
	anon := do(t, router, http.MethodGet, "/api/v1/key-pairs", "")
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET /key-pairs status = %d, want 401", anon.Code)
	}
}

func TestKeyPairListRejectsBadArguments(t *testing.T) {
	t.Parallel()

	router, _ := newKeyPairRouter(t)
	login := setupUserAndLogin(t, router)

	for _, path := range []string{
		"/api/v1/key-pairs?page_size=abc",
		"/api/v1/key-pairs?page_size=0",
		"/api/v1/key-pairs?page_size=201",
		"/api/v1/key-pairs?sort=name:asc",
		"/api/v1/key-pairs?page_token=mangled",
	} {
		recorder := doWithToken(t, router, http.MethodGet, path, "", login.access)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400 (body %s)", path, recorder.Code, recorder.Body)
		}
	}
}

// fixtureEd25519PublicKey is the import fixture of the service tests; the
// fingerprint below is what `ssh-keygen -lf` prints for it.
const (
	fixtureEd25519PublicKey   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMSkpB4KwlcQbdHTekHMstz+XeDU0Eugj1nOCRMlwz0e fixture-ed25519"
	fixtureEd25519Fingerprint = "SHA256:nE1g6wUlVBQKTyKOSJF1xO4wZDGZSJaEgxVU2je12jg"
)

func tooManyTagsBody() string {
	var b strings.Builder
	b.WriteString(`{"name":"k","tags":{`)
	for i := range 51 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"t%02d":"v"`, i)
	}
	b.WriteString("}}")
	return b.String()
}

// signerFor returns the signer wired into a router built by newKeyPairRouter;
// the tests re-create it, so it must match the one inside.
func signerFor(t *testing.T, _ *gin.Engine) *auth.Signer {
	t.Helper()

	signer, err := auth.NewSigner([]byte(strings.Repeat("k", 32)), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewSigner(): %v", err)
	}
	return signer
}

// newKeyPairRouter builds a router with a real store, auth and key pair
// service, and returns the store so tests can seed rows below the API.
func newKeyPairRouter(t *testing.T) (*gin.Engine, *store.Store) {
	t.Helper()

	logger := logging.Discard()
	db := storetest.New(t)
	signer := signerFor(t, nil)

	svc, err := authservice.New(authservice.Options{
		Logger:     logger,
		Store:      db,
		Signer:     signer,
		RefreshTTL: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("authservice.New(): %v", err)
	}

	keyPairs, err := keypair.New(keypair.Options{Logger: logger, Store: db})
	if err != nil {
		t.Fatalf("keypair.New(): %v", err)
	}

	handler := api.NewHandler(api.Options{
		Logger:       logger,
		GinMode:      gin.TestMode,
		System:       system.New(logger, "test", "fake", nil, nil, nil),
		Auth:         svc,
		KeyPairs:     keyPairs,
		Signer:       signer,
		RefreshTTL:   7 * 24 * time.Hour,
		CookieSecure: false,
	})

	return handler.Router(), db
}

// insertInstanceWithKey seeds a running instance that references the key
// pair, standing in for the M4 launch workflow.
func insertInstanceWithKey(t *testing.T, db *store.Store, keyPairID string) {
	t.Helper()

	ctx := context.Background()

	if _, err := db.Exec(ctx,
		`INSERT OR IGNORE INTO instance_types (id, vcpus, memory_mib) VALUES ('kumo.small', 1, 2048)`); err != nil {
		t.Fatalf("insert instance type: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT OR IGNORE INTO images (id, name, state, created_at, updated_at)
		 VALUES ('img-00000000000000000', 'img-00000000000000000', 'available',
		         '2026-01-02T09:00:00Z', '2026-01-02T09:00:00Z')`); err != nil {
		t.Fatalf("insert image: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO instances (id, name, state, image_id, instance_type, vcpus, memory_mib,
		  root_disk_gb, key_pair_id, launched_at, updated_at)
		 VALUES ('i-11111111111111111', 'web-1', 'running', 'img-00000000000000000', 'kumo.small',
		  1, 2048, 10, ?1, '2026-01-02T09:05:00Z', '2026-01-02T09:05:00Z')`,
		keyPairID); err != nil {
		t.Fatalf("insert instance: %v", err)
	}
}
