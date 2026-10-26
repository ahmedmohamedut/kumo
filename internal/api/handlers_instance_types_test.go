package api_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/auth"
)

// instanceTypeBody mirrors InstanceType of api/openapi.yaml.
type instanceTypeBody struct {
	ID          string `json:"id"`
	VCPUs       int    `json:"vcpus"`
	MemoryMiB   int    `json:"memory_mib"`
	Description string `json:"description"`
}

// instanceTypeDetailBody mirrors InstanceTypeDetail of api/openapi.yaml.
type instanceTypeDetailBody struct {
	instanceTypeBody
	UsedBy []string `json:"used_by"`
}

func TestCreateInstanceType(t *testing.T) {
	t.Parallel()

	router := newCapacityRouter(t)
	login := setupUserAndLogin(t, router)

	recorder := doWithToken(t, router, http.MethodPost, "/api/v1/instance-types",
		`{"id":"db.large","vcpus":4,"memory_mib":8192}`, login.access)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST /instance-types status = %d, want 201 (body %s)", recorder.Code, recorder.Body)
	}
	if got := recorder.Header().Get("Location"); got != "/api/v1/instance-types/db.large" {
		t.Errorf("Location = %q, want /api/v1/instance-types/db.large", got)
	}

	var created instanceTypeBody
	decode(t, recorder, &created)
	want := instanceTypeBody{ID: "db.large", VCPUs: 4, MemoryMiB: 8192, Description: "4 vCPU, 8 GiB"}
	if created != want {
		t.Errorf("created = %+v, want %+v", created, want)
	}

	// The new type is in the list, in size order between the presets.
	recorder = doWithToken(t, router, http.MethodGet, "/api/v1/instance-types", "", login.access)
	var list instanceTypesBody
	decode(t, recorder, &list)
	ids := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		ids = append(ids, item.ID)
	}
	wantIDs := []string{"kumo.nano", "kumo.micro", "kumo.small", "kumo.medium", "kumo.large",
		"db.large", "kumo.xlarge", "kumo.2xlarge"}
	if !slices.Equal(ids, wantIDs) {
		t.Errorf("GET /instance-types ids = %v, want %v", ids, wantIDs)
	}
}

func TestCreateInstanceTypeRefusals(t *testing.T) {
	t.Parallel()

	router := newCapacityRouter(t)
	login := setupUserAndLogin(t, router)

	for _, test := range []struct {
		name      string
		body      string
		status    int
		code      string
		wantField string
	}{
		{"not JSON", `{`, http.StatusBadRequest, api.CodeInvalidArgument, "body"},
		{"a bad id", `{"id":"Big/One","vcpus":1,"memory_mib":512}`, http.StatusBadRequest, api.CodeInvalidArgument, "id"},
		{"no vcpus", `{"id":"t","memory_mib":512}`, http.StatusBadRequest, api.CodeInvalidArgument, "vcpus"},
		{"too little memory", `{"id":"t","vcpus":1,"memory_mib":64}`, http.StatusBadRequest, api.CodeInvalidArgument, "memory_mib"},
		{"a taken id", `{"id":"kumo.small","vcpus":1,"memory_mib":512}`, http.StatusConflict, api.CodeAlreadyExists, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := doWithToken(t, router, http.MethodPost, "/api/v1/instance-types", test.body, login.access)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, test.status, recorder.Body)
			}
			envelope := decodeError(t, recorder.Body.Bytes())
			if envelope.Error.Code != test.code {
				t.Errorf("code = %q, want %q", envelope.Error.Code, test.code)
			}
			if test.wantField != "" &&
				!slices.ContainsFunc(envelope.Error.Details, func(f api.FieldError) bool { return f.Field == test.wantField }) {
				t.Errorf("details = %+v, want one for %q", envelope.Error.Details, test.wantField)
			}
		})
	}
}

func TestGetInstanceType(t *testing.T) {
	t.Parallel()

	router, db := newCapacityRouterWithStore(t)
	login := setupUserAndLogin(t, router)
	insertCapacityInstance(t, db, "running", 1, 2048, 10)

	recorder := doWithToken(t, router, http.MethodGet, "/api/v1/instance-types/kumo.small", "", login.access)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /instance-types/kumo.small status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}
	var detail instanceTypeDetailBody
	decode(t, recorder, &detail)
	if detail.ID != "kumo.small" || len(detail.UsedBy) != 1 {
		t.Errorf("detail = %+v, want kumo.small used by one instance", detail)
	}

	recorder = doWithToken(t, router, http.MethodGet, "/api/v1/instance-types/kumo.nano", "", login.access)
	decode(t, recorder, &detail)
	if detail.UsedBy == nil || len(detail.UsedBy) != 0 {
		t.Errorf("unused type used_by = %v, want an empty list", detail.UsedBy)
	}

	recorder = doWithToken(t, router, http.MethodGet, "/api/v1/instance-types/nope", "", login.access)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("GET unknown type status = %d, want 404", recorder.Code)
	}
}

func TestUpdateInstanceType(t *testing.T) {
	t.Parallel()

	router := newCapacityRouter(t)
	login := setupUserAndLogin(t, router)

	recorder := doWithToken(t, router, http.MethodPatch, "/api/v1/instance-types/kumo.small",
		`{"vcpus":2,"memory_mib":3072}`, login.access)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}
	var updated instanceTypeBody
	decode(t, recorder, &updated)
	want := instanceTypeBody{ID: "kumo.small", VCPUs: 2, MemoryMiB: 3072, Description: "2 vCPU, 3 GiB"}
	if updated != want {
		t.Errorf("updated = %+v, want %+v", updated, want)
	}

	for _, test := range []struct {
		name   string
		path   string
		body   string
		status int
	}{
		{"an empty body", "/api/v1/instance-types/kumo.small", `{}`, http.StatusBadRequest},
		{"not JSON", "/api/v1/instance-types/kumo.small", `[`, http.StatusBadRequest},
		{"out of bounds", "/api/v1/instance-types/kumo.small", `{"vcpus":1000}`, http.StatusBadRequest},
		{"an unknown type", "/api/v1/instance-types/nope", `{"vcpus":2}`, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := doWithToken(t, router, http.MethodPatch, test.path, test.body, login.access)
			if recorder.Code != test.status {
				t.Errorf("status = %d, want %d (body %s)", recorder.Code, test.status, recorder.Body)
			}
		})
	}
}

func TestDeleteInstanceType(t *testing.T) {
	t.Parallel()

	router, db := newCapacityRouterWithStore(t)
	login := setupUserAndLogin(t, router)
	insertCapacityInstance(t, db, "running", 1, 2048, 10)

	recorder := doWithToken(t, router, http.MethodDelete, "/api/v1/instance-types/kumo.small", "", login.access)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("DELETE a type in use status = %d, want 409 (body %s)", recorder.Code, recorder.Body)
	}
	assertErrorCode(t, recorder, api.CodeConflict)

	recorder = doWithToken(t, router, http.MethodDelete, "/api/v1/instance-types/kumo.nano", "", login.access)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("DELETE an unused type status = %d, want 204 (body %s)", recorder.Code, recorder.Body)
	}

	recorder = doWithToken(t, router, http.MethodDelete, "/api/v1/instance-types/kumo.nano", "", login.access)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("DELETE a deleted type status = %d, want 404", recorder.Code)
	}
}

func TestInstanceTypeWritesNeedAnAdministrator(t *testing.T) {
	t.Parallel()

	router := newCapacityRouter(t)
	signer, err := auth.NewSigner([]byte(capacitySecret), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewSigner(): %v", err)
	}
	viewer, err := signer.IssueAccessToken("01J0000000000000000000000V", auth.RoleViewer)
	if err != nil {
		t.Fatalf("IssueAccessToken(): %v", err)
	}

	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/instance-types", `{"id":"t","vcpus":1,"memory_mib":512}`},
		{http.MethodPatch, "/api/v1/instance-types/kumo.small", `{"vcpus":2}`},
		{http.MethodDelete, "/api/v1/instance-types/kumo.small", ""},
	} {
		recorder := doWithToken(t, router, test.method, test.path, test.body, viewer.Token)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s status = %d, want 403 (body %s)", test.method, test.path, recorder.Code, recorder.Body)
		}
		recorder = doWithToken(t, router, test.method, test.path, test.body, "")
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s status = %d, want 401", test.method, test.path, recorder.Code)
		}
	}

	// Reads stay open to a viewer.
	for _, path := range []string{"/api/v1/instance-types", "/api/v1/instance-types/kumo.small"} {
		if recorder := doWithToken(t, router, http.MethodGet, path, "", viewer.Token); recorder.Code != http.StatusOK {
			t.Errorf("viewer GET %s status = %d, want 200", path, recorder.Code)
		}
	}

	// Nothing a viewer sent changed the type.
	recorder := doWithToken(t, router, http.MethodGet, "/api/v1/instance-types/kumo.small", "", viewer.Token)
	var detail instanceTypeDetailBody
	decode(t, recorder, &detail)
	if detail.VCPUs != 1 {
		t.Errorf("kumo.small vcpus after a viewer's PATCH = %d, want 1", detail.VCPUs)
	}
}
