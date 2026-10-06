package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/subnet"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/service/vpc"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
)

// networkRouter is the router under test plus what a test needs to reach below
// the API surface.
type networkRouter struct {
	router  *gin.Engine
	vpcs    *vpc.Service
	subnets *subnet.Service
	store   *store.Store
	hv      *fake.Fake
	signer  *auth.Signer
}

// newNetworkRouter builds a router with real VPC and subnet services on a fake
// hypervisor. The VPC CIDRs the tests use stay clear of the host's own LAN:
// Create scans the real routing table, and API tests cannot swap that probe the
// way the service tests do, so every fixture lives on the uncommon 10.200/16
// blocks.
func newNetworkRouter(t *testing.T) networkRouter {
	t.Helper()

	logger := logging.Discard()
	db := storetest.New(t)

	signer, err := auth.NewSigner([]byte(strings.Repeat("n", 32)), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewSigner(): %v", err)
	}
	authSvc, err := authservice.New(authservice.Options{
		Logger: logger, Store: db, Signer: signer, RefreshTTL: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("authservice.New(): %v", err)
	}
	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}
	hv := fake.New(fake.Config{})

	subs, err := subnet.New(subnet.Options{Logger: logger, Store: db})
	if err != nil {
		t.Fatalf("subnet.New(): %v", err)
	}
	vpcs, err := vpc.New(vpc.Options{
		Logger:     logger,
		Store:      db,
		Hypervisor: hv,
		Tasks:      tasks,
		Subnets:    subs,
	})
	if err != nil {
		t.Fatalf("vpc.New(): %v", err)
	}

	handler := api.NewHandler(api.Options{
		Logger:       logger,
		GinMode:      gin.TestMode,
		System:       system.New(logger, "test", "fake", nil, nil, nil),
		Auth:         authSvc,
		VPCs:         vpcs,
		Subnets:      subs,
		Tasks:        tasks,
		Signer:       signer,
		RefreshTTL:   7 * 24 * time.Hour,
		CookieSecure: false,
	})

	return networkRouter{router: handler.Router(), vpcs: vpcs, subnets: subs, store: db, hv: hv, signer: signer}
}

// The fixture names and ranges every test creates. Constants keep the repeated
// strings of the endpoints in one place — and off goconst's radar — while the
// ranges stay clear of the host LAN (see newNetworkRouter).
const (
	vpcProd    = "prod"
	vpcStaging = "staging"

	cidrProd    = "10.200.0.0/16"
	cidrStaging = "10.201.0.0/16"

	// tagEnv is the tag key the tests read back from responses.
	tagEnv = "env"

	// The ids of resources that do not exist. Any id with the right shape is a
	// 404, so one value is enough for every route.
	missingVPCID    = "vpc-00000000000000000"
	missingSubnetID = "subnet-00000000000000000"
)

// vpcCreateJSON is a valid POST /vpcs body for a name and a range.
func vpcCreateJSON(name, cidr string) string {
	return fmt.Sprintf(`{"name":%q,"cidr":%q}`, name, cidr)
}

// vpcCreateJSONWithTags is a POST /vpcs body with a tag set.
func vpcCreateJSONWithTags(name, cidr string, tags map[string]string) string {
	return fmt.Sprintf(`{"name":%q,"cidr":%q,"tags":%s}`, name, cidr, jsonObject(tags))
}

// patchTagsJSON is the PATCH /vpcs/{id} and /subnets/{id} body that replaces
// the whole tag set.
func patchTagsJSON(tags map[string]string) string {
	return fmt.Sprintf(`{"tags":%s}`, jsonObject(tags))
}

// jsonObject renders a tag set the way encoding/json would, for the body
// builders above. The key order is irrelevant: the handlers parse the JSON.
func jsonObject(tags map[string]string) string {
	if len(tags) == 0 {
		return "{}"
	}
	entries := make([]string, 0, len(tags))
	for key, value := range tags {
		entries = append(entries, fmt.Sprintf("%q:%q", key, value))
	}
	return "{" + strings.Join(entries, ",") + "}"
}

// vpcBody is the shape of a VPC in a response.
type vpcBody struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	CIDR           string            `json:"cidr"`
	Mode           string            `json:"mode"`
	GatewayIP      string            `json:"gateway_ip"`
	Netmask        string            `json:"netmask"`
	BridgeName     string            `json:"bridge_name"`
	LibvirtNetwork string            `json:"libvirt_network"`
	IsDefault      bool              `json:"is_default"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	Tags           map[string]string `json:"tags"`
}

// createdVPCResponse adds the task the create queues.
type createdVPCResponse struct {
	vpcBody
	TaskID string `json:"task_id"`
}

// vpcDetailResponse adds the counts of what lives inside.
type vpcDetailResponse struct {
	vpcBody
	SubnetCount   int64 `json:"subnet_count"`
	InstanceCount int64 `json:"instance_count"`
}

func TestVPCRoutesRequireAuth(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "list", method: http.MethodGet, path: "/api/v1/vpcs"},
		{name: "create", method: http.MethodPost, path: "/api/v1/vpcs",
			body: vpcCreateJSON(vpcProd, cidrProd)},
		{name: "get", method: http.MethodGet, path: "/api/v1/vpcs/" + missingVPCID},
		{name: "patch", method: http.MethodPatch, path: "/api/v1/vpcs/" + missingVPCID,
			body: `{"tags":{}}`},
		{name: "delete", method: http.MethodDelete, path: "/api/v1/vpcs/" + missingVPCID},
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

func TestVPCWritesAreAdminOnly(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	setupUser(t, ir.router)

	issued, err := ir.signer.IssueAccessToken("01J00000000000000000000000", auth.RoleViewer)
	if err != nil {
		t.Fatalf("IssueAccessToken(): %v", err)
	}
	viewer := session{access: issued.Token}

	// Writes are refused for a viewer.
	if recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/vpcs",
		vpcCreateJSON(vpcProd, cidrProd), viewer.access); recorder.Code != http.StatusForbidden {
		t.Errorf("POST /vpcs as a viewer: status = %d, want 403 (body %s)", recorder.Code, recorder.Body)
	}
	if recorder := doWithToken(t, ir.router, http.MethodPatch, "/api/v1/vpcs/"+missingVPCID,
		`{"tags":{}}`, viewer.access); recorder.Code != http.StatusForbidden {
		t.Errorf("PATCH /vpcs as a viewer: status = %d, want 403", recorder.Code)
	}
	if recorder := doWithToken(t, ir.router, http.MethodDelete, "/api/v1/vpcs/"+missingVPCID,
		"", viewer.access); recorder.Code != http.StatusForbidden {
		t.Errorf("DELETE /vpcs as a viewer: status = %d, want 403", recorder.Code)
	}

	// Reads are open to both roles.
	if list := doWithToken(t, ir.router, http.MethodGet, "/api/v1/vpcs", "", viewer.access); list.Code != http.StatusOK {
		t.Errorf("GET /vpcs as a viewer: status = %d, want 200", list.Code)
	}
	if detail := doWithToken(t, ir.router, http.MethodGet, "/api/v1/vpcs/"+missingVPCID, "",
		viewer.access); detail.Code != http.StatusNotFound {
		t.Errorf("GET /vpcs/{id} as a viewer: status = %d, want 404 for an unknown id", detail.Code)
	}
}

func TestCreateVPCQueuesTheNetworkTask(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)

	recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/vpcs",
		vpcCreateJSONWithTags(vpcProd, cidrProd, map[string]string{tagEnv: vpcProd}), login.access)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("POST /vpcs status = %d, want 202 (body %s)", recorder.Code, recorder.Body)
	}

	var created createdVPCResponse
	decode(t, recorder, &created)

	if !strings.HasPrefix(created.ID, "vpc-") || len(created.ID) != 21 {
		t.Errorf("ID = %q, want a vpc- id of 17 hex", created.ID)
	}
	if created.Name != vpcProd || created.CIDR != cidrProd || created.Mode != "nat" {
		t.Errorf("body = %+v, want the stored name, cidr and nat mode", created)
	}
	if created.IsDefault {
		t.Error("is_default = true for a plain create")
	}
	if created.GatewayIP != "10.200.0.1" || created.Netmask != "255.255.0.0" {
		t.Errorf("gateway/netmask = %q/%q, want 10.200.0.1/255.255.0.0",
			created.GatewayIP, created.Netmask)
	}
	if created.Tags[tagEnv] != vpcProd {
		t.Errorf("Tags = %v, want %s=%s", created.Tags, tagEnv, vpcProd)
	}

	// The Location header names the task that defines and starts the network.
	if !strings.HasPrefix(created.TaskID, "task-") {
		t.Errorf("TaskID = %q, want a task- id", created.TaskID)
	}
	if got := recorder.Header().Get("Location"); got != "/api/v1/tasks/"+created.TaskID {
		t.Errorf("Location = %q, want /api/v1/tasks/%s", got, created.TaskID)
	}
}

func TestCreateVPCValidates(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "an empty name", body: fmt.Sprintf(`{"cidr":%q}`, cidrProd), want: "name"},
		{name: "an uppercase name", body: fmt.Sprintf(`{"name":%q,"cidr":%q}`, "Prod", cidrProd), want: "name"},
		{name: "an unknown mode", body: fmt.Sprintf(`{"name":%q,"cidr":%q,"mode":%q}`, "k", cidrProd, "bridged"), want: "mode"},
		{name: "a public cidr", body: `{"name":"k","cidr":"8.8.8.0/24"}`, want: "cidr"},
		{name: "not JSON", body: `{"name":`, want: "body"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/vpcs", tt.body, login.access)
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

func TestCreateVPCRefusesDuplicatesAndOverlaps(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)

	first := doWithToken(t, ir.router, http.MethodPost, "/api/v1/vpcs",
		vpcCreateJSON(vpcProd, cidrProd), login.access)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first POST /vpcs status = %d, want 202 (body %s)", first.Code, first.Body)
	}

	// The same name is a 409 regardless of the range.
	duplicate := doWithToken(t, ir.router, http.MethodPost, "/api/v1/vpcs",
		vpcCreateJSON(vpcProd, cidrStaging), login.access)
	if duplicate.Code != http.StatusConflict {
		t.Errorf("duplicate POST /vpcs status = %d, want 409", duplicate.Code)
	}
	assertErrorCode(t, duplicate, api.CodeAlreadyExists)

	// A range wholly inside the first VPC is a 409 too.
	overlap := doWithToken(t, ir.router, http.MethodPost, "/api/v1/vpcs",
		vpcCreateJSON(vpcStaging, "10.200.128.0/24"), login.access)
	if overlap.Code != http.StatusConflict {
		t.Errorf("overlapping POST /vpcs status = %d, want 409", overlap.Code)
	}
	assertErrorCode(t, overlap, api.CodeConflict)
	if strings.Contains(overlap.Body.String(), "10.200.128.0/24") == false {
		t.Errorf("the conflict body does not name the CIDR: %s", overlap.Body)
	}
}

func TestGetVPCReportsCounts(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)

	created := doWithToken(t, ir.router, http.MethodPost, "/api/v1/vpcs",
		vpcCreateJSON(vpcProd, cidrProd), login.access)
	var vpc createdVPCResponse
	decode(t, created, &vpc)

	// Before anything is launched inside, both counts are zero.
	empty := doWithToken(t, ir.router, http.MethodGet, "/api/v1/vpcs/"+vpc.ID, "", login.access)
	if empty.Code != http.StatusOK {
		t.Fatalf("GET /vpcs/{id} status = %d, want 200 (body %s)", empty.Code, empty.Body)
	}
	var detail vpcDetailResponse
	decode(t, empty, &detail)
	if detail.SubnetCount != 0 || detail.InstanceCount != 0 {
		t.Errorf("counts = %d/%d, want 0/0", detail.SubnetCount, detail.InstanceCount)
	}

	// A subnet and an instance inside it show up.
	sub, err := ir.subnets.Create(t.Context(), subnet.CreateInput{
		VPCID: vpc.ID, Name: "app-a", CIDR: "10.200.1.0/24",
	})
	if err != nil {
		t.Fatalf("subnet.Create(): %v", err)
	}
	insertInterface(t, ir, sub.ID)

	populated := doWithToken(t, ir.router, http.MethodGet, "/api/v1/vpcs/"+vpc.ID, "", login.access)
	if populated.Code != http.StatusOK {
		t.Fatalf("GET /vpcs/{id} after seeding status = %d, want 200", populated.Code)
	}
	decode(t, populated, &detail)
	if detail.SubnetCount != 1 || detail.InstanceCount != 1 {
		t.Errorf("counts = %d/%d, want 1/1", detail.SubnetCount, detail.InstanceCount)
	}

	// An unknown id is a 404 in the envelope.
	missing := doWithToken(t, ir.router, http.MethodGet, "/api/v1/vpcs/"+missingVPCID, "",
		login.access)
	if missing.Code != http.StatusNotFound {
		t.Errorf("GET /vpcs/{missing} status = %d, want 404", missing.Code)
	}
	assertErrorCode(t, missing, api.CodeNotFound)
}

func TestPatchVPCTags(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)

	created := doWithToken(t, ir.router, http.MethodPost, "/api/v1/vpcs",
		vpcCreateJSONWithTags(vpcProd, cidrProd, map[string]string{tagEnv: "dev"}), login.access)
	var vpc createdVPCResponse
	decode(t, created, &vpc)

	patched := doWithToken(t, ir.router, http.MethodPatch, "/api/v1/vpcs/"+vpc.ID,
		patchTagsJSON(map[string]string{tagEnv: vpcProd, "owner": "ada"}), login.access)
	if patched.Code != http.StatusOK {
		t.Fatalf("PATCH /vpcs/{id} status = %d, want 200 (body %s)", patched.Code, patched.Body)
	}
	var body vpcBody
	decode(t, patched, &body)
	if body.Tags[tagEnv] != vpcProd || body.Tags["owner"] != "ada" {
		t.Errorf("Tags = %v, want the replaced set", body.Tags)
	}
	if body.Name != vpcProd || body.CIDR != cidrProd {
		t.Errorf("PATCH moved immutable fields: %+v", body)
	}

	// A null tags field reads as "the request updates nothing".
	noop := doWithToken(t, ir.router, http.MethodPatch, "/api/v1/vpcs/"+vpc.ID,
		`{"tags":null}`, login.access)
	if noop.Code != http.StatusBadRequest {
		t.Errorf("PATCH with null tags status = %d, want 400", noop.Code)
	}
	assertErrorCode(t, noop, api.CodeInvalidArgument)

	// An empty map clears the set.
	cleared := doWithToken(t, ir.router, http.MethodPatch, "/api/v1/vpcs/"+vpc.ID,
		patchTagsJSON(nil), login.access)
	// Decode into a fresh variable: json.Unmarshal reuses a populated map, so
	// the explicit {} would look like the old set leaking through.
	var clearedBody vpcBody
	decode(t, cleared, &clearedBody)
	if len(clearedBody.Tags) != 0 {
		t.Errorf("Tags = %v, want empty", clearedBody.Tags)
	}
}

func TestDeleteVPCQueuesTheTeardown(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)

	created := doWithToken(t, ir.router, http.MethodPost, "/api/v1/vpcs",
		vpcCreateJSON(vpcProd, cidrProd), login.access)
	var vpc createdVPCResponse
	decode(t, created, &vpc)

	deleted := doWithToken(t, ir.router, http.MethodDelete, "/api/v1/vpcs/"+vpc.ID, "", login.access)
	if deleted.Code != http.StatusAccepted {
		t.Fatalf("DELETE /vpcs/{id} status = %d, want 202 (body %s)", deleted.Code, deleted.Body)
	}

	var gone createdVPCResponse
	decode(t, deleted, &gone)
	if gone.ID != vpc.ID {
		t.Errorf("body = %+v, want the deleted VPC", gone)
	}
	if !strings.HasPrefix(gone.TaskID, "task-") {
		t.Errorf("TaskID = %q, want a task- id", gone.TaskID)
	}
	if got := deleted.Header().Get("Location"); got != "/api/v1/tasks/"+gone.TaskID {
		t.Errorf("Location = %q, want /api/v1/tasks/%s", got, gone.TaskID)
	}

	// An unknown id is a 404.
	missing := doWithToken(t, ir.router, http.MethodDelete, "/api/v1/vpcs/"+missingVPCID, "",
		login.access)
	if missing.Code != http.StatusNotFound {
		t.Errorf("DELETE /vpcs/{missing} status = %d, want 404", missing.Code)
	}
}

func TestDeleteVPCRefusesWhileSubnetsExist(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)

	created := doWithToken(t, ir.router, http.MethodPost, "/api/v1/vpcs",
		vpcCreateJSON(vpcProd, cidrProd), login.access)
	var vpc createdVPCResponse
	decode(t, created, &vpc)

	if _, err := ir.subnets.Create(t.Context(), subnet.CreateInput{
		VPCID: vpc.ID, Name: "app-a", CIDR: "10.200.1.0/24",
	}); err != nil {
		t.Fatalf("subnet.Create(): %v", err)
	}

	refused := doWithToken(t, ir.router, http.MethodDelete, "/api/v1/vpcs/"+vpc.ID, "", login.access)
	if refused.Code != http.StatusConflict {
		t.Errorf("DELETE /vpcs/{id} with a subnet status = %d, want 409 (body %s)", refused.Code, refused.Body)
	}
	assertErrorCode(t, refused, api.CodeConflict)
	if strings.Contains(refused.Body.String(), "subnet") == false {
		t.Errorf("the refusal does not mention the subnet: %s", refused.Body)
	}
}

func TestListVPCs(t *testing.T) {
	t.Parallel()

	ir := newNetworkRouter(t)
	login := setupUserAndLogin(t, ir.router)

	for _, fixture := range []struct{ name, cidr string }{
		{vpcProd, cidrProd},
		{vpcStaging, cidrStaging},
	} {
		created := doWithToken(t, ir.router, http.MethodPost, "/api/v1/vpcs",
			vpcCreateJSON(fixture.name, fixture.cidr), login.access)
		if created.Code != http.StatusAccepted {
			t.Fatalf("POST /vpcs %s status = %d, want 202 (body %s)", fixture.name, created.Code, created.Body)
		}
	}

	all := doWithToken(t, ir.router, http.MethodGet, "/api/v1/vpcs", "", login.access)
	if all.Code != http.StatusOK {
		t.Fatalf("GET /vpcs status = %d, want 200 (body %s)", all.Code, all.Body)
	}
	var page vpcListPage
	decode(t, all, &page)
	if len(page.Items) != 2 {
		t.Errorf("items = %+v, want both VPCs", page.Items)
	}
	names := make(map[string]bool, len(page.Items))
	for _, v := range page.Items {
		names[v.Name] = true
	}
	if !names[vpcProd] || !names[vpcStaging] {
		t.Errorf("items = %+v, want %s and %s", page.Items, vpcProd, vpcStaging)
	}

	// The q= filter narrows the page to matching names.
	filtered := doWithToken(t, ir.router, http.MethodGet, "/api/v1/vpcs?q=stag", "", login.access)
	if filtered.Code != http.StatusOK {
		t.Fatalf("GET /vpcs?q=stag status = %d, want 200", filtered.Code)
	}
	decode(t, filtered, &page)
	if len(page.Items) != 1 || page.Items[0].Name != vpcStaging {
		t.Errorf("filtered items = %+v, want only %s", page.Items, vpcStaging)
	}
}

type vpcListPage struct {
	Items         []vpcBody `json:"items"`
	NextPageToken *string   `json:"next_page_token"`
}
