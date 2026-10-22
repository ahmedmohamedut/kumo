// Tests for the subnet endpoints. A subnet is a synchronous row, so these go
// straight over HTTP: they pin the status codes and the error codes of the
// contract surface. The counting and rejection rules live in the service and
// have their own tests; here every create rides on a real VPC made through
// POST /vpcs, so the fixtures share the router and the store the test owns.
package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/auth"
)

// The subnet a test creates where one fixture is enough. Constants keep the
// repeated strings of the endpoints in one place and off goconst's radar.
const (
	subnetApp     = "app-a"
	subnetAppCIDR = "10.200.0.0/24"

	// seedTime is a fixed timestamp for the raw rows insertInterface writes.
	seedTime = "2026-01-02T09:00:00Z"
)

// subnetBody is the shape of a subnet in a response.
type subnetBody struct {
	ID           string            `json:"id"`
	VPCID        string            `json:"vpc_id"`
	Name         string            `json:"name"`
	CIDR         string            `json:"cidr"`
	TotalIPs     int64             `json:"total_ips"`
	UsableIPs    int64             `json:"usable_ips"`
	AllocatedIPs int64             `json:"allocated_ips"`
	IsDefault    bool              `json:"is_default"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	Tags         map[string]string `json:"tags"`
}

// subnetListPage is one page of a subnet listing.
type subnetListPage struct {
	Items         []subnetBody `json:"items"`
	NextPageToken *string      `json:"next_page_token"`
}

// createVPCFixture makes a VPC through POST /vpcs and returns its id. The
// callers stay on the 10.200/10.201 blocks the network router is built around.
func createVPCFixture(t *testing.T, ir networkRouter, s session, name, cidr string) string {
	t.Helper()

	recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/vpcs",
		vpcCreateJSON(name, cidr), s.access)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("POST /vpcs %s status = %d, want 202 (body %s)", name, recorder.Code, recorder.Body)
	}
	var body createdVPCResponse
	decode(t, recorder, &body)
	return body.ID
}

// createSubnetThroughAPI makes a subnet inside vpcID through POST /subnets and
// returns its body, so the tests can feed the result straight into the next
// request.
func createSubnetThroughAPI(t *testing.T, ir networkRouter, s session, vpcID, name, cidr string) subnetBody {
	t.Helper()

	recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/subnets",
		fmt.Sprintf(`{"vpc_id":%q,"name":%q,"cidr":%q}`, vpcID, name, cidr), s.access)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST /subnets %s status = %d, want 201 (body %s)", name, recorder.Code, recorder.Body)
	}
	var body subnetBody
	decode(t, recorder, &body)
	return body
}

// insertInterface stands in for a launch: an instance row plus one network
// interface allocated from a subnet. It runs at most once per store per test,
// because the fixed ids, MAC and addresses it uses are unique per database.
func insertInterface(t *testing.T, ir networkRouter, subnetID string) string {
	t.Helper()

	ctx := context.Background()
	if _, err := ir.store.Exec(ctx,
		`INSERT OR IGNORE INTO instance_types (id, vcpus, memory_mib) VALUES ('kumo.small', 1, 2048)`); err != nil {
		t.Fatalf("insert instance type: %v", err)
	}

	imageID := "img-" + strings.Repeat("0", 17)
	if _, err := ir.store.Exec(ctx,
		`INSERT INTO images (id, name, state, created_at, updated_at)
		 VALUES (?1, ?1, 'available', ?2, ?2)`,
		imageID, seedTime); err != nil {
		t.Fatalf("insert image: %v", err)
	}

	instanceID := "i-" + strings.Repeat("1", 17)
	if _, err := ir.store.Exec(ctx,
		`INSERT INTO instances (id, name, state, image_id, instance_type, vcpus, memory_mib,
		   root_disk_gb, mac_address, private_ip, launched_at, updated_at)
		 VALUES (?1, 'web-1', 'running', ?2, 'kumo.small', 1, 2048, 10, '52:54:00:00:00:01', '10.0.0.10',
		   ?3, ?3)`,
		instanceID, imageID, seedTime); err != nil {
		t.Fatalf("insert instance: %v", err)
	}

	if _, err := ir.store.Exec(ctx,
		`INSERT INTO network_interfaces
		   (id, instance_id, subnet_id, device_index, mac_address, private_ip, created_at)
		 VALUES ('eni-`+strings.Repeat("1", 17)+`', ?1, ?2, 0, '52:54:00:00:00:01', '10.0.0.10', ?3)`,
		instanceID, subnetID, seedTime); err != nil {
		t.Fatalf("insert network interface: %v", err)
	}

	return instanceID
}

func TestSubnetRoutesRequireAuth(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "list", method: http.MethodGet, path: "/api/v1/subnets"},
		{name: "create", method: http.MethodPost, path: "/api/v1/subnets",
			body: fmt.Sprintf(`{"vpc_id":%q,"name":%q,"cidr":%q}`, missingVPCID, subnetApp, subnetAppCIDR)},
		{name: "get", method: http.MethodGet, path: "/api/v1/subnets/" + missingSubnetID},
		{name: "patch", method: http.MethodPatch, path: "/api/v1/subnets/" + missingSubnetID,
			body: `{"tags":{}}`},
		{name: "delete", method: http.MethodDelete, path: "/api/v1/subnets/" + missingSubnetID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := do(t, ir.router, tt.method, tt.path, tt.body)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s status = %d, want 401 (body %s)",
					tt.method, tt.path, recorder.Code, recorder.Body)
			}
			assertErrorCode(t, recorder, api.CodeUnauthenticated)
		})
	}
}

func TestSubnetWritesAreAdminOnly(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	setupUser(t, ir.router)

	issued, err := ir.signer.IssueAccessToken("01J00000000000000000000000", auth.RoleViewer)
	if err != nil {
		t.Fatalf("IssueAccessToken(): %v", err)
	}
	viewer := session{access: issued.Token}

	// Writes are refused for a viewer.
	if recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/subnets",
		fmt.Sprintf(`{"vpc_id":%q,"name":%q,"cidr":%q}`, missingVPCID, subnetApp, subnetAppCIDR),
		viewer.access); recorder.Code != http.StatusForbidden {
		t.Errorf("POST /subnets as a viewer: status = %d, want 403 (body %s)", recorder.Code, recorder.Body)
	}
	if recorder := doWithToken(t, ir.router, http.MethodPatch, "/api/v1/subnets/"+missingSubnetID,
		`{"tags":{}}`, viewer.access); recorder.Code != http.StatusForbidden {
		t.Errorf("PATCH /subnets as a viewer: status = %d, want 403", recorder.Code)
	}
	if recorder := doWithToken(t, ir.router, http.MethodDelete, "/api/v1/subnets/"+missingSubnetID,
		"", viewer.access); recorder.Code != http.StatusForbidden {
		t.Errorf("DELETE /subnets as a viewer: status = %d, want 403", recorder.Code)
	}

	// Reads are open to both roles.
	if list := doWithToken(t, ir.router, http.MethodGet, "/api/v1/subnets", "", viewer.access); list.Code != http.StatusOK {
		t.Errorf("GET /subnets as a viewer: status = %d, want 200", list.Code)
	}
	if detail := doWithToken(t, ir.router, http.MethodGet, "/api/v1/subnets/"+missingSubnetID, "",
		viewer.access); detail.Code != http.StatusNotFound {
		t.Errorf("GET /subnets/{id} as a viewer: status = %d, want 404 for an unknown id", detail.Code)
	}
}

func TestCreateSubnetReturnsCounts(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)
	vpcID := createVPCFixture(t, ir, login, vpcProd, cidrProd)

	recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/subnets",
		fmt.Sprintf(`{"vpc_id":%q,"name":%q,"cidr":%q,"tags":%s}`,
			vpcID, subnetApp, subnetAppCIDR, jsonObject(map[string]string{tagEnv: vpcProd})),
		login.access)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST /subnets status = %d, want 201 (body %s)", recorder.Code, recorder.Body)
	}

	var body subnetBody
	decode(t, recorder, &body)

	if !strings.HasPrefix(body.ID, "subnet-") || len(body.ID) != 24 {
		t.Errorf("ID = %q, want a subnet- id of 17 hex", body.ID)
	}
	if body.VPCID != vpcID || body.Name != subnetApp || body.CIDR != subnetAppCIDR {
		t.Errorf("subnet = %q/%q/%q, want the stored vpc, name and cidr",
			body.VPCID, body.Name, body.CIDR)
	}
	// The VPC gateway 10.200.0.1 lives inside this /24, so one address of the
	// 254 usable ones is reserved.
	if body.TotalIPs != 256 || body.UsableIPs != 253 || body.AllocatedIPs != 0 {
		t.Errorf("counts = %d/%d/%d, want 256/253/0", body.TotalIPs, body.UsableIPs, body.AllocatedIPs)
	}
	if !body.IsDefault {
		t.Error("is_default = false for the first subnet of the default VPC")
	}
	if body.Tags[tagEnv] != vpcProd {
		t.Errorf("Tags = %v, want %s=%s", body.Tags, tagEnv, vpcProd)
	}
}

func TestCreateSubnetValidates(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)
	vpcID := createVPCFixture(t, ir, login, vpcProd, cidrProd)

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "an empty name", body: fmt.Sprintf(`{"vpc_id":%q,"cidr":%q}`, vpcID, subnetAppCIDR), want: "name"},
		{name: "an uppercase name", body: fmt.Sprintf(`{"vpc_id":%q,"name":%q,"cidr":%q}`, vpcID, "App-a", subnetAppCIDR), want: "name"},
		{name: "a cidr outside the parent", body: fmt.Sprintf(`{"vpc_id":%q,"name":%q,"cidr":%q}`, vpcID, "k", "10.202.0.0/24"), want: "cidr"},
		{name: "not JSON", body: fmt.Sprintf(`{"vpc_id":%q,"name":`, vpcID), want: "body"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/subnets", tt.body, login.access)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body)
			}
			assertErrorCode(t, recorder, api.CodeInvalidArgument)

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

func TestCreateSubnetUnknownVPC(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)

	recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/subnets",
		fmt.Sprintf(`{"vpc_id":%q,"name":%q,"cidr":%q}`, missingVPCID, subnetApp, subnetAppCIDR), login.access)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("POST /subnets with an unknown vpc status = %d, want 404 (body %s)", recorder.Code, recorder.Body)
	}
	assertErrorCode(t, recorder, api.CodeNotFound)
}

func TestCreateSubnetRefusesAnOverlappingSibling(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)
	vpcID := createVPCFixture(t, ir, login, vpcProd, cidrProd)

	createSubnetThroughAPI(t, ir, login, vpcID, subnetApp, subnetAppCIDR)

	recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/subnets",
		fmt.Sprintf(`{"vpc_id":%q,"name":%q,"cidr":%q}`, vpcID, "app-b", "10.200.0.128/25"), login.access)
	if recorder.Code != http.StatusConflict {
		t.Errorf("overlapping POST /subnets status = %d, want 409 (body %s)", recorder.Code, recorder.Body)
	}
	assertErrorCode(t, recorder, api.CodeConflict)
	if !strings.Contains(recorder.Body.String(), "overlaps") {
		t.Errorf("the refusal does not say the range overlaps: %s", recorder.Body)
	}
}

func TestCreateSubnetDuplicateNameScopedToTheVPC(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)
	prodID := createVPCFixture(t, ir, login, vpcProd, cidrProd)
	stagingID := createVPCFixture(t, ir, login, vpcStaging, cidrStaging)

	createSubnetThroughAPI(t, ir, login, prodID, subnetApp, subnetAppCIDR)

	// The same name in the same VPC is a 409...
	duplicate := doWithToken(t, ir.router, http.MethodPost, "/api/v1/subnets",
		fmt.Sprintf(`{"vpc_id":%q,"name":%q,"cidr":%q}`, prodID, subnetApp, "10.200.1.0/24"), login.access)
	if duplicate.Code != http.StatusConflict {
		t.Errorf("duplicate POST /subnets status = %d, want 409 (body %s)", duplicate.Code, duplicate.Body)
	}
	assertErrorCode(t, duplicate, api.CodeAlreadyExists)

	// ...and in another VPC it is free.
	sibling := createSubnetThroughAPI(t, ir, login, stagingID, subnetApp, "10.201.0.0/24")
	if sibling.VPCID != stagingID {
		t.Errorf("sibling = %+v, want the staging VPC", sibling)
	}
}

func TestListSubnetsFiltersByVPC(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)
	prodID := createVPCFixture(t, ir, login, vpcProd, cidrProd)
	stagingID := createVPCFixture(t, ir, login, vpcStaging, cidrStaging)

	createSubnetThroughAPI(t, ir, login, prodID, "alpha", subnetAppCIDR)
	createSubnetThroughAPI(t, ir, login, prodID, "bravo", "10.200.1.0/24")
	createSubnetThroughAPI(t, ir, login, stagingID, "charlie", "10.201.0.0/24")

	// The vpc_id filter keeps a page to its parent VPC.
	filtered := doWithToken(t, ir.router, http.MethodGet, "/api/v1/subnets?vpc_id="+prodID, "", login.access)
	if filtered.Code != http.StatusOK {
		t.Fatalf("GET /subnets?vpc_id status = %d, want 200", filtered.Code)
	}
	var page subnetListPage
	decode(t, filtered, &page)
	if got := subnetNames(page.Items); len(got) != 2 || !got["alpha"] || !got["bravo"] {
		t.Errorf("vpc-filtered items = %+v, want the two subnets of prod", page.Items)
	}

	// Without the filter every subnet is visible.
	all := doWithToken(t, ir.router, http.MethodGet, "/api/v1/subnets", "", login.access)
	if all.Code != http.StatusOK {
		t.Fatalf("GET /subnets status = %d, want 200", all.Code)
	}
	decode(t, all, &page)
	if len(page.Items) != 3 {
		t.Errorf("unfiltered items = %+v, want all three subnets", page.Items)
	}

	// A name substring narrows it further.
	q := doWithToken(t, ir.router, http.MethodGet, "/api/v1/subnets?q=char", "", login.access)
	if q.Code != http.StatusOK {
		t.Fatalf("GET /subnets?q=char status = %d, want 200", q.Code)
	}
	decode(t, q, &page)
	if len(page.Items) != 1 || page.Items[0].Name != "charlie" {
		t.Errorf("q-filtered items = %+v, want charlie alone", page.Items)
	}
}

// subnetNames folds a page of subnets into a set of names, so assertions do
// not depend on the timestamps a page sorts by.
func subnetNames(items []subnetBody) map[string]bool {
	names := make(map[string]bool, len(items))
	for _, item := range items {
		names[item.Name] = true
	}
	return names
}

func TestPatchSubnetTags(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)
	vpcID := createVPCFixture(t, ir, login, vpcProd, cidrProd)
	created := createSubnetThroughAPI(t, ir, login, vpcID, subnetApp, subnetAppCIDR)

	patched := doWithToken(t, ir.router, http.MethodPatch, "/api/v1/subnets/"+created.ID,
		patchTagsJSON(map[string]string{tagEnv: vpcProd, "owner": "ada"}), login.access)
	if patched.Code != http.StatusOK {
		t.Fatalf("PATCH /subnets/{id} status = %d, want 200 (body %s)", patched.Code, patched.Body)
	}
	var body subnetBody
	decode(t, patched, &body)
	if body.Tags[tagEnv] != vpcProd || body.Tags["owner"] != "ada" {
		t.Errorf("Tags = %v, want the replaced set", body.Tags)
	}
	if body.Name != subnetApp || body.CIDR != subnetAppCIDR || body.VPCID != vpcID {
		t.Errorf("PATCH moved immutable fields: %+v", body)
	}

	// A null tags field reads as "the request updates nothing".
	noop := doWithToken(t, ir.router, http.MethodPatch, "/api/v1/subnets/"+created.ID,
		`{"tags":null}`, login.access)
	if noop.Code != http.StatusBadRequest {
		t.Errorf("PATCH with null tags status = %d, want 400", noop.Code)
	}
	assertErrorCode(t, noop, api.CodeInvalidArgument)

	// An empty map clears the set.
	cleared := doWithToken(t, ir.router, http.MethodPatch, "/api/v1/subnets/"+created.ID,
		patchTagsJSON(nil), login.access)
	// A fresh variable: json.Unmarshal reuses a populated map, which would keep
	// the previous keys visible against an explicit {}.
	var clearedBody subnetBody
	decode(t, cleared, &clearedBody)
	if len(clearedBody.Tags) != 0 {
		t.Errorf("Tags = %v, want empty", clearedBody.Tags)
	}
}

func TestDeleteSubnet(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)
	vpcID := createVPCFixture(t, ir, login, vpcProd, cidrProd)
	created := createSubnetThroughAPI(t, ir, login, vpcID, subnetApp, subnetAppCIDR)

	deleted := doWithToken(t, ir.router, http.MethodDelete, "/api/v1/subnets/"+created.ID, "", login.access)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("DELETE /subnets/{id} status = %d, want 204 (body %s)", deleted.Code, deleted.Body)
	}

	// The row reads as gone...
	gone := doWithToken(t, ir.router, http.MethodGet, "/api/v1/subnets/"+created.ID, "", login.access)
	if gone.Code != http.StatusNotFound {
		t.Errorf("GET /subnets/{id} after delete status = %d, want 404", gone.Code)
	}

	// ...and so does a second delete.
	again := doWithToken(t, ir.router, http.MethodDelete, "/api/v1/subnets/"+created.ID, "", login.access)
	if again.Code != http.StatusNotFound {
		t.Errorf("second DELETE /subnets/{id} status = %d, want 404", again.Code)
	}
	assertErrorCode(t, again, api.CodeNotFound)
}

func TestDeleteSubnetRefusesWhileInterfacesExist(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)
	vpcID := createVPCFixture(t, ir, login, vpcProd, cidrProd)
	created := createSubnetThroughAPI(t, ir, login, vpcID, subnetApp, subnetAppCIDR)
	insertInterface(t, ir, created.ID)

	refused := doWithToken(t, ir.router, http.MethodDelete, "/api/v1/subnets/"+created.ID, "", login.access)
	if refused.Code != http.StatusConflict {
		t.Errorf("DELETE /subnets/{id} with an interface status = %d, want 409 (body %s)", refused.Code, refused.Body)
	}
	assertErrorCode(t, refused, api.CodeConflict)
	if !strings.Contains(refused.Body.String(), "interface") {
		t.Errorf("the refusal does not mention the interface: %s", refused.Body)
	}

	// The row survives the refusal.
	alive := doWithToken(t, ir.router, http.MethodGet, "/api/v1/subnets/"+created.ID, "", login.access)
	if alive.Code != http.StatusOK {
		t.Errorf("GET /subnets/{id} after the refusal status = %d, want 200", alive.Code)
	}
}
