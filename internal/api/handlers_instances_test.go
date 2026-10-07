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
	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

const (
	instancesPool = "kumo-instances"
	networkName   = "default"
)

// instanceBody is the shape of an instance in a response. UserData is a pointer
// so a test can tell "hidden from a viewer" from "there was none".
type instanceBody struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	ImageID           string            `json:"image_id"`
	ImageName         *string           `json:"image_name"`
	InstanceType      string            `json:"instance_type"`
	State             string            `json:"state"`
	StateReason       *string           `json:"state_reason"`
	PrivateIP         *string           `json:"private_ip"`
	MacAddress        *string           `json:"mac_address"`
	VpcID             *string           `json:"vpc_id"`
	SubnetID          *string           `json:"subnet_id"`
	NetworkInterfaces []interfaceBody   `json:"network_interfaces"`
	KeyPairID         *string           `json:"key_pair_id"`
	KeyPairName       *string           `json:"key_pair_name"`
	RootDiskGB        int               `json:"root_disk_gb"`
	UserData          *string           `json:"user_data"`
	Tags              map[string]string `json:"tags"`
	TaskID            *string           `json:"task_id"`
	LaunchedAt        *time.Time        `json:"launched_at"`
	CreatedAt         time.Time         `json:"created_at"`
}

// interfaceBody is one element of network_interfaces.
type interfaceBody struct {
	ID          string `json:"id"`
	DeviceIndex int    `json:"device_index"`
	MACAddress  string `json:"mac_address"`
	PrivateIP   string `json:"private_ip"`
	SubnetID    string `json:"subnet_id"`
	VpcID       string `json:"vpc_id"`
}

// instancePage is a listing response.
type instancePage struct {
	Items         []instanceBody `json:"items"`
	NextPageToken *string        `json:"next_page_token"`
}

// instanceRouter is the router under test plus what a test needs below the API.
type instanceRouter struct {
	router    *gin.Engine
	instances *instance.Service
	images    *image.Service
	store     *store.Store
	hv        *fake.Fake
	signer    *auth.Signer
	tokens    session
}

// newInstanceRouter builds a router with the real services and a fake
// hypervisor, and logs the first user in.
func newInstanceRouter(t *testing.T) instanceRouter {
	t.Helper()

	logger := logging.Discard()
	db := storetest.New(t)

	signer, err := auth.NewSigner([]byte(strings.Repeat("i", 32)), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewSigner(): %v", err)
	}
	authSvc, err := authservice.New(authservice.Options{
		Logger: logger, Store: db, Signer: signer, RefreshTTL: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("auth.New(): %v", err)
	}
	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}

	hv := fake.New(fake.Config{})
	if err := hv.EnsureInfra(t.Context(), hypervisor.InfraConfig{
		ImagesPool: imagesPool, InstancesPool: instancesPool, Network: networkName,
		DataDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("EnsureInfra(): %v", err)
	}
	seedDefaultNetwork(t, db, hv)

	images, err := image.New(image.Options{
		Logger:     logger,
		Store:      db,
		Hypervisor: hv,
		Tasks:      tasks,
		Downloader: imagefetch.New(imagefetch.Options{
			AllowPrivateNetworks: true,
			MaxBytes:             4 << 20,
		}),
		MaxBytes:   4 << 20,
		ImagesPool: imagesPool,
		TempDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("image.New(): %v", err)
	}
	types, err := instancetype.New(instancetype.Options{Store: db})
	if err != nil {
		t.Fatalf("instancetype.New(): %v", err)
	}
	// serve.go seeds the presets on every boot; a test that launches anything has
	// to do the same or the launch is refused for an instance type that the
	// product ships with.
	if err := types.Seed(t.Context()); err != nil {
		t.Fatalf("Seed(): %v", err)
	}
	pairs, err := keypair.New(keypair.Options{Logger: logger, Store: db})
	if err != nil {
		t.Fatalf("keypair.New(): %v", err)
	}
	sys := system.New(logger, "test", "fake", db, hv, db)
	limits := capacity.Limits{MemoryReserveMiB: 1024, VCPUOvercommit: 4}
	sys.SetLimits(limits)

	instances, err := instance.New(instance.Options{
		Logger: logger, Store: db, Hypervisor: hv, Volumes: hv, Tasks: tasks,
		Images: images, Types: types, KeyPairs: pairs, Capacity: sys,
		InstancesPool: instancesPool, ImagesPool: imagesPool,
		ConsoleDir: t.TempDir(), Network: networkName, Limits: limits,
	})
	if err != nil {
		t.Fatalf("instance.New(): %v", err)
	}

	handler := api.NewHandler(api.Options{
		Logger:       logger,
		GinMode:      gin.TestMode,
		System:       system.New(logger, "test", "fake", nil, nil, nil),
		Auth:         authSvc,
		Images:       images,
		Instances:    instances,
		Tasks:        tasks,
		Signer:       signer,
		RefreshTTL:   7 * 24 * time.Hour,
		CookieSecure: false,
	})

	router := handler.Router()

	// One pool for both services, started here so every test that imports an
	// image gets a task runner without asking for one.
	ir := instanceRouter{
		router:    router,
		instances: instances,
		images:    images,
		store:     db,
		hv:        hv,
		signer:    signer,
		tokens:    setupUserAndLogin(t, router),
	}
	startInstanceWorkers(t, ir)

	return ir
}

// seedDefaultNetwork stands in for the M4b.2 bootstrap: a default VPC row, its
// default subnet and the fake network that backs them, so a launch resolves
// them exactly as serve.go would. The subnet is a /25 inside the VPC's /24, so
// the lowest free address is .2 — the prefix the launch tests assert.
func seedDefaultNetwork(t *testing.T, db *store.Store, hv *fake.Fake) {
	t.Helper()

	vpcID := id.VPC()
	subnetID := id.Subnet()

	if _, err := db.Exec(t.Context(),
		`INSERT INTO vpcs (id, name, cidr, mode, gateway_ip, netmask, bridge_name, libvirt_network, is_default, created_at, updated_at)
		 VALUES (?1, 'default', '192.168.100.0/24', 'nat', '192.168.100.1', '255.255.255.0', ?2, ?1, 1,
		        '2026-01-02T09:00:00Z', '2026-01-02T09:00:00Z')`,
		vpcID, hypervisor.BridgeName(vpcID)); err != nil {
		t.Fatalf("seed the default VPC: %v", err)
	}
	if _, err := db.Exec(t.Context(),
		`INSERT INTO subnets (id, vpc_id, name, cidr, is_default, created_at, updated_at)
		 VALUES (?1, ?2, 'default', '192.168.100.0/25', 1, '2026-01-02T09:00:00Z', '2026-01-02T09:00:00Z')`,
		subnetID, vpcID); err != nil {
		t.Fatalf("seed the default subnet: %v", err)
	}

	if err := hv.DefineNetwork(t.Context(), hypervisor.NetworkSpec{
		Name: vpcID, Bridge: hypervisor.BridgeName(vpcID),
		CIDR: "192.168.100.0/24", Gateway: "192.168.100.1",
		Mode: hypervisor.NetworkModeNAT, Domain: "default",
	}); err != nil {
		t.Fatalf("DefineNetwork(): %v", err)
	}
	if err := hv.StartNetwork(t.Context(), vpcID); err != nil {
		t.Fatalf("StartNetwork(): %v", err)
	}
}

// startInstanceWorkers runs one pool for both the image and the launch
// handlers, which is the wiring internal/cli/serve.go builds: a single worker
// draining one tasks table.
func startInstanceWorkers(t *testing.T, ir instanceRouter) {
	t.Helper()

	pool := worker.New(worker.Options{
		Logger:       logging.Discard(),
		Store:        ir.store,
		Registry:     mergeRegistries(ir.images.Handler(), ir.instances.Handler()),
		Workers:      1,
		PollInterval: 5 * time.Millisecond,
	})
	if err := pool.Start(t.Context()); err != nil {
		t.Fatalf("worker.Start(): %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := pool.Stop(ctx); err != nil {
			t.Errorf("worker.Stop(): %v", err)
		}
	})
}

// mergeRegistries is internal/cli/serve.go's mergeHandlers, which is not
// exported: the pool takes one registry, and the tests need both services in it.
func mergeRegistries(registries ...worker.Registry) worker.Registry {
	merged := worker.Registry{}
	for _, registry := range registries {
		for taskType, handler := range registry {
			merged[taskType] = handler
		}
	}
	return merged
}

// availableImageID imports a tiny image through the API and waits for the task,
// so the launch below is the one a user would do.
func availableImageID(t *testing.T, ir instanceRouter) string {
	t.Helper()

	url, sum := tinyImageServer(t)
	recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/images/import", fmt.Sprintf(
		`{"name":"ubuntu-2404","source_url":%q,"checksum":{"algorithm":"sha256","value":%q},`+
			`"os_family":"linux","architecture":"x86_64","default_user":"ubuntu","min_disk_gb":8}`,
		url, sum), ir.tokens.access)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("POST /images/import status = %d, want 202 (body %s)", recorder.Code, recorder.Body)
	}

	var imported imageBody
	decode(t, recorder, &imported)
	waitForTaskState(t, ir.router, ir.tokens.access, imported.TaskID, "succeeded")

	return imported.ID
}

func TestLaunchInstanceIsAsynchronousAndEndsRunning(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)

	accepted := doWithToken(t, ir.router, http.MethodPost, "/api/v1/instances", fmt.Sprintf(
		`{"name":"web-01","image_id":%q,"instance_type":"kumo.small","root_disk_gb":8,`+
			`"user_data":"#cloud-config\npackages: [curl]\n","tags":{"env":"dev"}}`, imageID),
		ir.tokens.access)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("POST /instances status = %d, want 202 (body %s)", accepted.Code, accepted.Body)
	}

	var launched instanceBody
	decode(t, accepted, &launched)
	if launched.State != string(instance.StatePending) {
		t.Errorf("state = %q, want pending: the boot is a task, not this response", launched.State)
	}
	if launched.TaskID == nil {
		t.Fatal("task_id is missing from the response")
	}
	if got, want := accepted.Header().Get("Location"), "/api/v1/tasks/"+*launched.TaskID; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	// The address and the MAC are allocated with the row, before the guest
	// exists: dnsmasq has to know the pairing when the guest first asks.
	if launched.PrivateIP == nil || !strings.HasPrefix(*launched.PrivateIP, "192.168.100.") {
		t.Errorf("private_ip = %v, want an address out of the default subnet", launched.PrivateIP)
	}
	if launched.MacAddress == nil {
		t.Error("mac_address is null in the launch response")
	}
	if launched.VpcID == nil || launched.SubnetID == nil {
		t.Errorf("vpc_id/subnet_id = %v/%v, want the default network placement",
			launched.VpcID, launched.SubnetID)
	}
	if len(launched.NetworkInterfaces) != 1 {
		t.Fatalf("network_interfaces = %d entries, want 1", len(launched.NetworkInterfaces))
	}
	eni := launched.NetworkInterfaces[0]
	if eni.MACAddress != *launched.MacAddress || eni.PrivateIP != *launched.PrivateIP {
		t.Errorf("interface = %+v, want the same MAC and IP as the instance", eni)
	}

	// The task runs, and the instance ends up with the runtime data the launch
	// found rather than just a row that exists.
	waitForTaskState(t, ir.router, ir.tokens.access, *launched.TaskID, "succeeded")

	detail := doWithToken(t, ir.router, http.MethodGet, "/api/v1/instances/"+launched.ID, "", ir.tokens.access)
	if detail.Code != http.StatusOK {
		t.Fatalf("GET /instances/%s status = %d, want 200 (body %s)", launched.ID, detail.Code, detail.Body)
	}
	var found instanceBody
	decode(t, detail, &found)
	if found.State != string(instance.StateRunning) {
		t.Errorf("state = %q, want running", found.State)
	}
	if found.PrivateIP == nil || !strings.HasPrefix(*found.PrivateIP, "192.168.100.") {
		t.Errorf("private_ip = %v, want an address out of the subnet the fake serves", found.PrivateIP)
	}
	if found.MacAddress == nil {
		t.Error("mac_address is null after a launch")
	}
	if found.VpcID == nil || found.SubnetID == nil {
		t.Error("vpc_id/subnet_id are null after a launch")
	}
	if len(found.NetworkInterfaces) != 1 {
		t.Errorf("network_interfaces = %d entries after a launch, want 1", len(found.NetworkInterfaces))
	}
	if found.LaunchedAt == nil {
		t.Error("launched_at is null after a launch")
	}
	if found.ImageName == nil || *found.ImageName != "ubuntu-2404" {
		t.Errorf("image_name = %v, want the image the instance was launched from", found.ImageName)
	}
	if found.UserData == nil || !strings.Contains(*found.UserData, "curl") {
		t.Errorf("user_data = %v, want an admin to get it back", found.UserData)
	}
	if found.Tags["env"] != "dev" {
		t.Errorf("tags = %v, want the launch tags", found.Tags)
	}
}

func TestLaunchInstanceHidesUserDataFromAViewer(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)

	viewer, err := ir.signer.IssueAccessToken("01J0000000000000000000000V", auth.RoleViewer)
	if err != nil {
		t.Fatalf("IssueAccessToken(): %v", err)
	}

	launched := instanceBody{}
	recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/instances", fmt.Sprintf(
		`{"name":"web-01","image_id":%q,"instance_type":"kumo.small","root_disk_gb":8,`+
			`"user_data":"#cloud-config\nruncmd: [rm -rf /]\n"}`, imageID), ir.tokens.access)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("POST /instances status = %d, want 202 (body %s)", recorder.Code, recorder.Body)
	}
	decode(t, recorder, &launched)

	detail := doWithToken(t, ir.router, http.MethodGet, "/api/v1/instances/"+launched.ID, "", viewer.Token)
	if detail.Code != http.StatusOK {
		t.Fatalf("viewer GET /instances/%s status = %d, want 200 (body %s)",
			launched.ID, detail.Code, detail.Body)
	}

	var found instanceBody
	decode(t, detail, &found)
	if found.UserData != nil {
		t.Fatalf("user_data = %q for a viewer, want null", *found.UserData)
	}
	// The state is whatever the worker has done with it by now; the user data is
	// hidden either way, which is the point of this test.
	if found.State != string(instance.StatePending) && found.State != string(instance.StateRunning) {
		t.Errorf("state = %q, want pending or running", found.State)
	}
}

func TestLaunchInstanceWritesRequireAdmin(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)

	viewer, err := ir.signer.IssueAccessToken("01J0000000000000000000000V", auth.RoleViewer)
	if err != nil {
		t.Fatalf("IssueAccessToken(): %v", err)
	}

	// Reading is open to a viewer.
	list := doWithToken(t, ir.router, http.MethodGet, "/api/v1/instances", "", viewer.Token)
	if list.Code != http.StatusOK {
		t.Fatalf("viewer GET /instances status = %d, want 200 (body %s)", list.Code, list.Body)
	}

	write := doWithToken(t, ir.router, http.MethodPost, "/api/v1/instances", fmt.Sprintf(
		`{"name":"web-01","image_id":%q,"instance_type":"kumo.small"}`, imageID), viewer.Token)
	if write.Code != http.StatusForbidden {
		t.Fatalf("viewer POST /instances status = %d, want 403 (body %s)", write.Code, write.Body)
	}

	anon := do(t, ir.router, http.MethodGet, "/api/v1/instances", "")
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET /instances status = %d, want 401", anon.Code)
	}
}

func TestLaunchInstanceRefusesCountAboveOne(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)

	for _, test := range []struct {
		name  string
		count string
		field string
	}{
		// A count of 0 is the absent count: like root_disk_gb, it means the default.
		{name: "two", count: `2`, field: "count"},
		{name: "ten", count: `10`, field: "count"},
		{name: "negative", count: `-1`, field: "count"},
		// A body the decoder refuses is reported against the body, not the field.
		{name: "not a number", count: `"two"`, field: "body"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/instances", fmt.Sprintf(
				`{"name":"web-01","image_id":%q,"instance_type":"kumo.small","count":%s}`,
				imageID, test.count), ir.tokens.access)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body)
			}

			envelope := decodeError(t, recorder.Body.Bytes())
			if envelope.Error.Code != "invalid_argument" {
				t.Errorf("code = %q, want invalid_argument", envelope.Error.Code)
			}
			if !hasDetail(envelope, test.field) {
				t.Errorf("details = %+v, want one about %q", envelope.Error.Details, test.field)
			}
		})
	}
}

func TestLaunchInstanceRefusesAnUnknownImage(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)

	recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/instances",
		`{"name":"web-01","image_id":"img-00000000000000000","instance_type":"kumo.small"}`,
		ir.tokens.access)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body)
	}
	if !hasDetail(decodeError(t, recorder.Body.Bytes()), "image_id") {
		t.Errorf("details = %+v, want one about image_id", decodeError(t, recorder.Body.Bytes()).Error.Details)
	}

	// A name that is not an image id at all is the same refusal.
	named := doWithToken(t, ir.router, http.MethodPost, "/api/v1/instances",
		`{"name":"web-01","image_id":"ubuntu","instance_type":"kumo.small"}`, ir.tokens.access)
	if named.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", named.Code, named.Body)
	}
}

func TestListInstancesPagesAndFilters(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)

	for _, name := range []string{"web-01", "web-02", "db-01"} {
		recorder := doWithToken(t, ir.router, http.MethodPost, "/api/v1/instances", fmt.Sprintf(
			`{"name":%q,"image_id":%q,"instance_type":"kumo.small","root_disk_gb":8}`,
			name, imageID), ir.tokens.access)
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("POST /instances %s status = %d, want 202 (body %s)",
				name, recorder.Code, recorder.Body)
		}
	}

	all := doWithToken(t, ir.router, http.MethodGet, "/api/v1/instances", "", ir.tokens.access)
	if all.Code != http.StatusOK {
		t.Fatalf("GET /instances status = %d, want 200 (body %s)", all.Code, all.Body)
	}
	var page instancePage
	decode(t, all, &page)
	if len(page.Items) != 3 {
		t.Fatalf("the page holds %d instances, want 3", len(page.Items))
	}
	// The libvirt uuid is a backend detail the reconciler matches domains with,
	// not something a user acts on, so it stays out of the response.
	if strings.Contains(all.Body.String(), "libvirt_uuid") {
		t.Error("the listing carries libvirt_uuid, want the backend detail left out")
	}
	for _, item := range page.Items {
		if item.UserData != nil {
			t.Errorf("user_data = %q in a listing, want it left out", *item.UserData)
		}
	}

	// A page of one walks the token, and the last page has no next token.
	first := doWithToken(t, ir.router, http.MethodGet, "/api/v1/instances?page_size=1", "", ir.tokens.access)
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", first.Code, first.Body)
	}
	var one instancePage
	decode(t, first, &one)
	if len(one.Items) != 1 {
		t.Fatalf("a page of 1 holds %d instances", len(one.Items))
	}
	if one.NextPageToken == nil {
		t.Fatal("next_page_token is missing on the first page")
	}
	second := doWithToken(t, ir.router, http.MethodGet,
		"/api/v1/instances?page_size=1&page_token="+*one.NextPageToken, "", ir.tokens.access)
	if second.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", second.Code, second.Body)
	}
	var next instancePage
	decode(t, second, &next)
	if len(next.Items) != 1 {
		t.Fatalf("the second page holds %d instances, want 1", len(next.Items))
	}
	if next.Items[0].ID == one.Items[0].ID {
		t.Error("the second page repeated the first instance")
	}
	if next.NextPageToken == nil {
		t.Fatal("next_page_token is missing on the second of three pages")
	}

	last := doWithToken(t, ir.router, http.MethodGet,
		"/api/v1/instances?page_size=1&page_token="+*next.NextPageToken, "", ir.tokens.access)
	if last.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", last.Code, last.Body)
	}
	var final instancePage
	decode(t, last, &final)
	if final.NextPageToken != nil {
		t.Errorf("next_page_token = %q on the last page, want null", *final.NextPageToken)
	}

	// The launches run in the background, so the state filters are only
	// meaningful once the worker has finished with them.
	waitForInstanceState(t, ir, "running", 3)

	// The filters the console offers.
	for _, test := range []struct {
		query string
		want  int
	}{
		{query: "?q=web", want: 2},
		{query: "?q=WEB", want: 2},
		// The worker in the harness has already launched everything, so the three
		// instances are running rather than pending.
		{query: "?state=pending", want: 0},
		{query: "?state=running", want: 3},
		{query: "?q=nothing-here", want: 0},
		{query: "?sort=launched_at:asc", want: 3},
	} {
		recorder := doWithToken(t, ir.router, http.MethodGet, "/api/v1/instances"+test.query, "", ir.tokens.access)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /instances%s status = %d, want 200 (body %s)",
				test.query, recorder.Code, recorder.Body)
		}
		var filtered instancePage
		decode(t, recorder, &filtered)
		if len(filtered.Items) != test.want {
			t.Errorf("GET /instances%s holds %d instances, want %d",
				test.query, len(filtered.Items), test.want)
		}
	}

	for _, query := range []string{
		"?page_size=0",
		fmt.Sprintf("?page_size=%d", instance.MaxPageSize+1),
		"?page_size=many",
		"?sort=nonsense",
	} {
		recorder := doWithToken(t, ir.router, http.MethodGet, "/api/v1/instances"+query, "", ir.tokens.access)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("GET /instances%s status = %d, want 400 (body %s)", query, recorder.Code, recorder.Body)
		}
	}
}

func TestGetInstanceReportsAnUnknownIDAsNotFound(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)

	recorder := doWithToken(t, ir.router, http.MethodGet,
		"/api/v1/instances/i-00000000000000000", "", ir.tokens.access)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body)
	}

	// An id of the wrong shape finds no row and is a 404 like any other missing
	// resource, the same as the other resources of the API.
	malformed := doWithToken(t, ir.router, http.MethodGet, "/api/v1/instances/web", "", ir.tokens.access)
	if malformed.Code != http.StatusNotFound {
		t.Errorf("a malformed id status = %d, want 404 (body %s)", malformed.Code, malformed.Body)
	}
}

// waitForInstanceState polls the listing until it holds count instances in want.
func waitForInstanceState(t *testing.T, ir instanceRouter, want string, count int) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		recorder := doWithToken(t, ir.router, http.MethodGet,
			"/api/v1/instances?state="+want+"&page_size=100", "", ir.tokens.access)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /instances?state=%s status = %d, want 200 (body %s)",
				want, recorder.Code, recorder.Body)
		}
		var page instancePage
		decode(t, recorder, &page)
		if len(page.Items) == count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d instances are %s, want %d", len(page.Items), want, count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// hasDetail reports whether the envelope carries a field error about field.
func hasDetail(envelope errorEnvelope, field string) bool {
	for _, detail := range envelope.Error.Details {
		if detail.Field == field {
			return true
		}
	}
	return false
}
