package api_test

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/kubernetes"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// clusterBody is the response shape api/openapi.yaml promises for a cluster,
// written out so a field that disappears from the JSON fails here.
type clusterBody struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	KubernetesVersion string  `json:"kubernetes_version"`
	State             string  `json:"state"`
	StateReason       *string `json:"state_reason"`
	Endpoint          *string `json:"endpoint"`
	VPCID             string  `json:"vpc_id"`
	SubnetID          string  `json:"subnet_id"`
	PodCIDR           string  `json:"pod_cidr"`
	ServiceCIDR       string  `json:"service_cidr"`
	ControlPlaneType  string  `json:"control_plane_type"`
	CACertificate     string  `json:"ca_certificate"`
	NodeGroups        []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DesiredSize int    `json:"desired_size"`
	} `json:"node_groups"`
	Nodes []struct {
		Name  string `json:"name"`
		Role  string `json:"role"`
		State string `json:"state"`
		Ready *bool  `json:"ready"`
	} `json:"nodes"`
	TaskID *string `json:"task_id"`
}

type clusterRouter struct {
	router *gin.Engine
	signer *auth.Signer
	tokens session
	hv     *fake.Fake
}

// newClusterRouter builds a router with the Kubernetes module on, the fake
// hypervisor and the fake cluster API, as kumod runs on KUMO_HYPERVISOR=fake.
func newClusterRouter(t *testing.T, on bool) clusterRouter {
	t.Helper()
	logger := logging.Discard()
	db := storetest.New(t)
	signer, err := auth.NewSigner([]byte(strings.Repeat("k", 32)), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	authSvc, err := authservice.New(authservice.Options{Logger: logger, Store: db, Signer: signer, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	tasks, _ := task.New(task.Options{Store: db})
	// 32 GiB, so a cluster of kumo.medium nodes has room to scale.
	hv := fake.New(fake.Config{Capacity: hypervisor.Capacity{VCPUs: 16, MemoryBytes: 32 << 30, DiskBytes: 500 << 30}})
	if err := hv.EnsureInfra(t.Context(), hypervisor.InfraConfig{
		ImagesPool: imagesPool, InstancesPool: instancesPool, Network: networkName, DataDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	seedDefaultNetwork(t, db, hv)
	images, err := image.New(image.Options{
		Logger: logger, Store: db, Hypervisor: hv, Tasks: tasks,
		Downloader: imagefetch.New(imagefetch.Options{AllowPrivateNetworks: true, MaxBytes: 4 << 20}),
		MaxBytes:   4 << 20, ImagesPool: imagesPool, TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	types, _ := instancetype.New(instancetype.Options{Logger: logger, Store: db})
	pairs, _ := keypair.New(keypair.Options{Logger: logger, Store: db})
	sys := system.New(logger, "test", "fake", db, hv, db)
	limits := capacity.Limits{MemoryReserveMiB: 1024, VCPUOvercommit: 4}
	sys.SetLimits(limits)
	instances, err := instance.New(instance.Options{
		Logger: logger, Store: db, Hypervisor: hv, Volumes: hv, Tasks: tasks, Images: images, Types: types,
		KeyPairs: pairs, Capacity: sys, InstancesPool: instancesPool, ImagesPool: imagesPool,
		ConsoleDir: t.TempDir(), Network: networkName, Limits: limits,
		PollInterval: time.Millisecond, PollTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	var clusters *kubernetes.Service
	registries := []worker.Registry{images.Handler(), instances.Handler()}
	if on {
		clusters, err = kubernetes.New(kubernetes.Options{
			Logger: logger, Store: db, Hypervisor: hv, Instances: instances, Tasks: tasks,
			DataDir: t.TempDir(), API: kubernetes.FakeAPI,
			ReadyTimeout: 2 * time.Second, PollInterval: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		registries = append(registries, clusters.Handler())
	}
	handler := api.NewHandler(api.Options{
		Logger: logger, GinMode: gin.TestMode, System: system.New(logger, "test", "fake", nil, nil, nil),
		Auth: authSvc, Images: images, Instances: instances, Clusters: clusters, Tasks: tasks,
		Signer: signer, RefreshTTL: time.Hour,
	})
	router := handler.Router()
	pool := worker.New(worker.Options{Logger: logger, Store: db, Registry: mergeRegistries(registries...),
		Workers: 1, PollInterval: 5 * time.Millisecond})
	if err := pool.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = pool.Stop(ctx)
	})
	return clusterRouter{router: router, signer: signer, tokens: setupUserAndLogin(t, router), hv: hv}
}

func importKubernetesImage(t *testing.T, cr clusterRouter) {
	t.Helper()
	url, sum := tinyImageServer(t)
	recorder := doWithToken(t, cr.router, http.MethodPost, "/api/v1/images/import", fmt.Sprintf(
		`{"name":"kubernetes-1.37","source_url":%q,"checksum":{"algorithm":"sha256","value":%q},`+
			`"os_family":"linux","architecture":"x86_64","default_user":"ubuntu","min_disk_gb":1,`+
			`"kind":"engine","engine":"kubernetes","engine_version":"1.37"}`, url, sum), cr.tokens.access)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("POST /images/import = %d (%s)", recorder.Code, recorder.Body)
	}
	var imported imageBody
	decode(t, recorder, &imported)
	waitForTaskState(t, cr.router, cr.tokens.access, imported.TaskID, "succeeded")
}

const createClusterBody = `{"name":"orders","node_count":2}`

func createActiveCluster(t *testing.T, cr clusterRouter) clusterBody {
	t.Helper()
	importKubernetesImage(t, cr)
	recorder := doWithToken(t, cr.router, http.MethodPost, "/api/v1/eks/clusters", createClusterBody, cr.tokens.access)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("POST /eks/clusters = %d (%s)", recorder.Code, recorder.Body)
	}
	var created clusterBody
	decode(t, recorder, &created)
	if created.State != "creating" || created.TaskID == nil ||
		recorder.Header().Get("Location") != "/api/v1/tasks/"+*created.TaskID {
		t.Fatalf("created = %+v, Location %q", created, recorder.Header().Get("Location"))
	}
	waitForTaskState(t, cr.router, cr.tokens.access, *created.TaskID, "succeeded")

	got := doWithToken(t, cr.router, http.MethodGet, "/api/v1/eks/clusters/"+created.ID, "", cr.tokens.access)
	var body clusterBody
	decode(t, got, &body)
	return body
}

// A cluster is created asynchronously and ends active, with its endpoint, its
// node group and every node Ready.
func TestCreateClusterEndsActive(t *testing.T) {
	t.Parallel()
	cr := newClusterRouter(t, true)
	c := createActiveCluster(t, cr)

	if c.State != "active" || c.Endpoint == nil || *c.Endpoint != "https://api.orders.default:6443" {
		t.Fatalf("cluster = %+v", c)
	}
	if c.KubernetesVersion != "1.37" || c.PodCIDR != "100.64.0.0/16" || !strings.Contains(c.CACertificate, "BEGIN CERTIFICATE") {
		t.Errorf("cluster = %+v", c)
	}
	if len(c.NodeGroups) != 1 || c.NodeGroups[0].DesiredSize != 2 || len(c.Nodes) != 3 {
		t.Errorf("groups %+v nodes %+v", c.NodeGroups, c.Nodes)
	}
	for _, n := range c.Nodes {
		if n.State != "running" || n.Ready == nil || !*n.Ready {
			t.Errorf("node %+v, want running and Ready", n)
		}
	}
	// The nodes are not on the Instances pages.
	list := doWithToken(t, cr.router, http.MethodGet, "/api/v1/instances", "", cr.tokens.access)
	if strings.Contains(list.Body.String(), "orders-cp-1") {
		t.Error("a cluster node appears in GET /instances")
	}
	groups := doWithToken(t, cr.router, http.MethodGet, "/api/v1/eks/clusters/"+c.ID+"/node-groups", "", cr.tokens.access)
	if groups.Code != http.StatusOK || !strings.Contains(groups.Body.String(), `"name":"default"`) {
		t.Errorf("GET node-groups = %d %s", groups.Code, groups.Body)
	}
}

func TestCreateClusterWithoutAnImage(t *testing.T) {
	t.Parallel()
	cr := newClusterRouter(t, true)
	recorder := doWithToken(t, cr.router, http.MethodPost, "/api/v1/eks/clusters", createClusterBody, cr.tokens.access)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"code":"no_engine_image"`) {
		t.Errorf("POST /eks/clusters with no image = %d %s, want 409 no_engine_image", recorder.Code, recorder.Body)
	}
}

func TestCreateClusterValidation(t *testing.T) {
	t.Parallel()
	cr := newClusterRouter(t, true)
	importKubernetesImage(t, cr)
	for _, body := range []string{`not json`, `{"name":"Orders"}`, `{"name":"orders","node_count":11}`,
		`{"name":"orders","control_plane_type":"kumo.small"}`} {
		recorder := doWithToken(t, cr.router, http.MethodPost, "/api/v1/eks/clusters", body, cr.tokens.access)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400 (%s)", body, recorder.Code, recorder.Body)
		}
	}
}

// The kubeconfig is minted for the caller, in the group of their role, with
// the username in the certificate, and is not cacheable.
func TestMintKubeconfigByRole(t *testing.T) {
	t.Parallel()
	cr := newClusterRouter(t, true)
	c := createActiveCluster(t, cr)
	path := "/api/v1/eks/clusters/" + c.ID + "/kubeconfig"

	viewerToken, err := cr.signer.IssueAccessToken("01J00000000000000000000000", auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token, group, cn string
	}{
		// The username, looked up from the token's user: setupUserAndLogin's.
		{cr.tokens.access, "kumo:admins", "ada"},
		{viewerToken.Token, "kumo:viewers", "01J00000000000000000000000"},
	} {
		recorder := doWithToken(t, cr.router, http.MethodPost, path, "", tc.token)
		if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("POST kubeconfig = %d, cache %q (%s)", recorder.Code, recorder.Header().Get("Cache-Control"), recorder.Body)
		}
		var resp struct {
			Kubeconfig string `json:"kubeconfig"`
			Group      string `json:"group"`
			ExpiresAt  string `json:"expires_at"`
		}
		decode(t, recorder, &resp)
		var doc struct {
			Users []struct {
				User struct {
					Cert string `yaml:"client-certificate-data"`
				} `yaml:"user"`
			} `yaml:"users"`
		}
		if err := yaml.Unmarshal([]byte(resp.Kubeconfig), &doc); err != nil || len(doc.Users) != 1 {
			t.Fatalf("kubeconfig: %v", err)
		}
		certPEM, _ := base64.StdEncoding.DecodeString(doc.Users[0].User.Cert)
		block, _ := pem.Decode(certPEM)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Group != tc.group || cert.Subject.Organization[0] != tc.group || resp.ExpiresAt == "" {
			t.Errorf("group = %s / %v, want %s", resp.Group, cert.Subject.Organization, tc.group)
		}
		if !strings.HasPrefix(cert.Subject.CommonName, tc.cn) {
			t.Errorf("CN = %q, want the caller (%s)", cert.Subject.CommonName, tc.cn)
		}
	}
}

// Writes are admin only; reads and the kubeconfig are open to a viewer.
func TestClusterWritesAreAdminOnly(t *testing.T) {
	t.Parallel()
	cr := newClusterRouter(t, true)
	c := createActiveCluster(t, cr)
	viewer, _ := cr.signer.IssueAccessToken("01J00000000000000000000000", auth.RoleViewer)

	for _, w := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/eks/clusters", createClusterBody},
		{http.MethodDelete, "/api/v1/eks/clusters/" + c.ID, ""},
	} {
		if r := doWithToken(t, cr.router, w.method, w.path, w.body, viewer.Token); r.Code != http.StatusForbidden {
			t.Errorf("%s %s as a viewer = %d, want 403", w.method, w.path, r.Code)
		}
	}
	for _, path := range []string{"/api/v1/eks/clusters", "/api/v1/eks/clusters/" + c.ID} {
		if r := doWithToken(t, cr.router, http.MethodGet, path, "", viewer.Token); r.Code != http.StatusOK {
			t.Errorf("GET %s as a viewer = %d, want 200", path, r.Code)
		}
	}
	if r := do(t, cr.router, http.MethodGet, "/api/v1/eks/clusters", ""); r.Code != http.StatusUnauthorized {
		t.Errorf("GET without a token = %d, want 401", r.Code)
	}
}

// Delete is 202 and a task, and the cluster is gone when it finishes.
func TestDeleteCluster(t *testing.T) {
	t.Parallel()
	cr := newClusterRouter(t, true)
	c := createActiveCluster(t, cr)

	recorder := doWithToken(t, cr.router, http.MethodDelete, "/api/v1/eks/clusters/"+c.ID, "", cr.tokens.access)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("DELETE = %d (%s)", recorder.Code, recorder.Body)
	}
	var queued clusterBody
	decode(t, recorder, &queued)
	if queued.State != string(kubernetes.StateDeleting) || queued.TaskID == nil {
		t.Fatalf("queued = %+v", queued)
	}
	if again := doWithToken(t, cr.router, http.MethodDelete, "/api/v1/eks/clusters/"+c.ID, "", cr.tokens.access); again.Code != http.StatusConflict {
		t.Errorf("a second DELETE = %d, want 409", again.Code)
	}
	waitForTaskState(t, cr.router, cr.tokens.access, *queued.TaskID, "succeeded")
	if got := doWithToken(t, cr.router, http.MethodGet, "/api/v1/eks/clusters/"+c.ID, "", cr.tokens.access); got.Code != http.StatusNotFound {
		t.Errorf("GET after the delete = %d, want 404", got.Code)
	}
	if info, _ := cr.hv.GetNetwork(t.Context(), networkName); len(info.DNSHosts) != 0 {
		t.Errorf("the endpoint record survived the delete: %+v", info.DNSHosts)
	}
}

func TestKubernetesOff(t *testing.T) {
	t.Parallel()
	cr := newClusterRouter(t, false)
	for _, w := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/eks/clusters"},
		{http.MethodPost, "/api/v1/eks/clusters"},
		{http.MethodGet, "/api/v1/eks/clusters/eks-0123456789abcdef0"},
	} {
		r := doWithToken(t, cr.router, w.method, w.path, "", cr.tokens.access)
		if r.Code != http.StatusServiceUnavailable || !strings.Contains(r.Body.String(), "KUMO_KUBERNETES") {
			t.Errorf("%s %s with the module off = %d %s, want 503 naming the switch", w.method, w.path, r.Code, r.Body)
		}
	}
}

// A node group is resized through PATCH: 202 and a task that ends active with
// the new workers, 200 unchanged for the same size, 400 out of range, 403 for
// a viewer.
func TestScaleNodeGroup(t *testing.T) {
	t.Parallel()
	cr := newClusterRouter(t, true)
	c := createActiveCluster(t, cr)
	path := "/api/v1/eks/clusters/" + c.ID + "/node-groups/" + c.NodeGroups[0].ID

	recorder := doWithToken(t, cr.router, http.MethodPatch, path, `{"desired_size":3}`, cr.tokens.access)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("PATCH = %d (%s)", recorder.Code, recorder.Body)
	}
	var scaled clusterBody
	decode(t, recorder, &scaled)
	if scaled.State != "updating" || scaled.TaskID == nil {
		t.Fatalf("scaled = %+v", scaled)
	}
	waitForTaskState(t, cr.router, cr.tokens.access, *scaled.TaskID, "succeeded")
	var after clusterBody
	decode(t, doWithToken(t, cr.router, http.MethodGet, "/api/v1/eks/clusters/"+c.ID, "", cr.tokens.access), &after)
	if after.State != "active" || after.NodeGroups[0].DesiredSize != 3 || len(after.Nodes) != 4 {
		t.Errorf("after the scale: %s, desired %d, %d nodes", after.State, after.NodeGroups[0].DesiredSize, len(after.Nodes))
	}

	if r := doWithToken(t, cr.router, http.MethodPatch, path, `{"desired_size":3}`, cr.tokens.access); r.Code != http.StatusOK {
		t.Errorf("PATCH to the same size = %d, want 200", r.Code)
	}
	for _, body := range []string{`{}`, `{"desired_size":0}`, `{"desired_size":11}`} {
		if r := doWithToken(t, cr.router, http.MethodPatch, path, body, cr.tokens.access); r.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d, want 400", body, r.Code)
		}
	}
	viewer, _ := cr.signer.IssueAccessToken("01J00000000000000000000000", auth.RoleViewer)
	if r := doWithToken(t, cr.router, http.MethodPatch, path, `{"desired_size":1}`, viewer.Token); r.Code != http.StatusForbidden {
		t.Errorf("PATCH as a viewer = %d, want 403", r.Code)
	}
}
