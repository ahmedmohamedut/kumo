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
	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	objfake "github.com/ahmedmohamedut/kumo/internal/objectstore/fake"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/spark"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

type sparkClusterBody struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	SparkVersion   string   `json:"spark_version"`
	State          string   `json:"state"`
	Endpoint       *string  `json:"endpoint"`
	MasterUIURL    *string  `json:"master_ui_url"`
	HistoryURL     *string  `json:"history_url"`
	ObjectStorage  bool     `json:"object_storage"`
	Buckets        []string `json:"buckets"`
	EventLogBucket *string  `json:"event_log_bucket"`
	NodeGroups     []struct {
		DesiredSize int `json:"desired_size"`
	} `json:"node_groups"`
	Nodes []struct {
		Name  string `json:"name"`
		Role  string `json:"role"`
		State string `json:"state"`
		Alive *bool  `json:"alive"`
	} `json:"nodes"`
	TaskID *string `json:"task_id"`
}

type sparkRouter struct {
	router *gin.Engine
	signer *auth.Signer
	tokens session
}

func newSparkRouter(t *testing.T, on bool) sparkRouter {
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

	objects := objfake.New()
	for _, b := range []string{"data", "logs"} {
		if err := objects.CreateBucket(t.Context(), b); err != nil {
			t.Fatal(err)
		}
	}
	var clusters *spark.Service
	registries := []worker.Registry{images.Handler(), instances.Handler()}
	if on {
		clusters, err = spark.New(spark.Options{
			Logger: logger, Store: db, Hypervisor: hv, Instances: instances, Tasks: tasks,
			ObjectStore: objects, S3Endpoint: "http://192.168.1.10:9000",
			DataDir: t.TempDir(), API: spark.FakeAPI,
			ReadyTimeout: 2 * time.Second, PollInterval: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		registries = append(registries, clusters.Handler())
	}
	handler := api.NewHandler(api.Options{
		Logger: logger, GinMode: gin.TestMode, System: system.New(logger, "test", "fake", nil, nil, nil),
		Auth: authSvc, Images: images, Instances: instances, SparkClusters: clusters, Tasks: tasks,
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
	return sparkRouter{router: router, signer: signer, tokens: setupUserAndLogin(t, router)}
}

func importSparkImage(t *testing.T, sr sparkRouter) {
	t.Helper()
	url, sum := tinyImageServer(t)
	recorder := doWithToken(t, sr.router, http.MethodPost, "/api/v1/images/import", fmt.Sprintf(
		`{"name":"spark-4.3","source_url":%q,"checksum":{"algorithm":"sha256","value":%q},`+
			`"os_family":"linux","architecture":"x86_64","default_user":"ubuntu","min_disk_gb":1,`+
			`"kind":"engine","engine":"spark","engine_version":"4.3"}`, url, sum), sr.tokens.access)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("POST /images/import = %d (%s)", recorder.Code, recorder.Body)
	}
	var imported imageBody
	decode(t, recorder, &imported)
	waitForTaskState(t, sr.router, sr.tokens.access, imported.TaskID, "succeeded")
}

const createSparkBody = `{"name":"etl","worker_type":"kumo.medium","worker_count":2,` +
	`"object_storage":true,"buckets":["data","logs"],"event_log_bucket":"logs"}`

func createActiveSparkCluster(t *testing.T, sr sparkRouter) sparkClusterBody {
	t.Helper()
	importSparkImage(t, sr)
	recorder := doWithToken(t, sr.router, http.MethodPost, "/api/v1/spark/clusters", createSparkBody, sr.tokens.access)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("POST /spark/clusters = %d (%s)", recorder.Code, recorder.Body)
	}
	var created sparkClusterBody
	decode(t, recorder, &created)
	if created.State != string(spark.StateCreating) || created.TaskID == nil ||
		recorder.Header().Get("Location") != "/api/v1/tasks/"+*created.TaskID {
		t.Fatalf("created = %+v, Location %q", created, recorder.Header().Get("Location"))
	}
	waitForTaskState(t, sr.router, sr.tokens.access, *created.TaskID, "succeeded")
	got := doWithToken(t, sr.router, http.MethodGet, "/api/v1/spark/clusters/"+created.ID, "", sr.tokens.access)
	var body sparkClusterBody
	decode(t, got, &body)
	return body
}

func TestCreateSparkClusterEndsActive(t *testing.T) {
	t.Parallel()
	sr := newSparkRouter(t, true)
	c := createActiveSparkCluster(t, sr)

	if c.State != string(spark.StateActive) || c.Endpoint == nil || *c.Endpoint != "spark://etl.default:7077" || c.SparkVersion != "4.3" {
		t.Fatalf("cluster = %+v", c)
	}
	if c.MasterUIURL == nil || c.HistoryURL == nil || !c.ObjectStorage || len(c.Buckets) != 2 {
		t.Errorf("cluster = %+v", c)
	}
	if len(c.Nodes) != 3 || c.Nodes[0].Role != "master" {
		t.Fatalf("nodes = %+v", c.Nodes)
	}
	for _, n := range c.Nodes {
		if n.State != string(instance.StateRunning) || n.Alive == nil || !*n.Alive {
			t.Errorf("node %+v, want running and alive", n)
		}
	}
	list := doWithToken(t, sr.router, http.MethodGet, "/api/v1/instances", "", sr.tokens.access)
	if strings.Contains(list.Body.String(), "etl-master") {
		t.Error("a Spark node appears in GET /instances")
	}
	page := doWithToken(t, sr.router, http.MethodGet, "/api/v1/spark/clusters?page_size=1", "", sr.tokens.access)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `"name":"etl"`) {
		t.Errorf("GET /spark/clusters = %d %s", page.Code, page.Body)
	}
}

func TestCreateSparkClusterRefusals(t *testing.T) {
	t.Parallel()
	sr := newSparkRouter(t, true)
	if r := doWithToken(t, sr.router, http.MethodPost, "/api/v1/spark/clusters", createSparkBody, sr.tokens.access); r.Code != http.StatusConflict ||
		!strings.Contains(r.Body.String(), `"code":"no_engine_image"`) {
		t.Errorf("POST with no image = %d %s, want 409 no_engine_image", r.Code, r.Body)
	}
	importSparkImage(t, sr)
	for _, body := range []string{
		`not json`, `{"name":"ETL"}`, `{"name":"etl","worker_count":11}`, `{"name":"etl","master_type":"kumo.small"}`,
		`{"name":"etl","buckets":["data"]}`, `{"name":"etl","object_storage":true,"buckets":["nope"]}`,
		`{"name":"etl","object_storage":true,"buckets":["data"],"event_log_bucket":"logs"}`,
	} {
		r := doWithToken(t, sr.router, http.MethodPost, "/api/v1/spark/clusters", body, sr.tokens.access)
		if r.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400 (%s)", body, r.Code, r.Body)
		}
	}
	if r := doWithToken(t, sr.router, http.MethodGet, "/api/v1/spark/clusters?page_size=x", "", sr.tokens.access); r.Code != http.StatusBadRequest {
		t.Errorf("GET with a bad page_size = %d", r.Code)
	}
	if r := doWithToken(t, sr.router, http.MethodGet, "/api/v1/spark/clusters/spk-0123456789abcdef0", "", sr.tokens.access); r.Code != http.StatusNotFound {
		t.Errorf("GET of no cluster = %d", r.Code)
	}
}

// The client configuration is an admin's: it carries the cluster's secret and
// S3 key (09-spark.md section 5, option A), and is never cached.
func TestSparkClientConfig(t *testing.T) {
	t.Parallel()
	sr := newSparkRouter(t, true)
	c := createActiveSparkCluster(t, sr)
	path := "/api/v1/spark/clusters/" + c.ID + "/client-config"

	r := doWithToken(t, sr.router, http.MethodPost, path, "", sr.tokens.access)
	if r.Code != http.StatusOK || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("POST client-config = %d, Cache-Control %q (%s)", r.Code, r.Header().Get("Cache-Control"), r.Body)
	}
	var body struct {
		ClientConfig string `json:"client_config"`
		Filename     string `json:"filename"`
	}
	decode(t, r, &body)
	for _, want := range []string{"spark.master", "spark://etl.default:7077", "spark.authenticate.secret",
		"spark.hadoop.fs.s3a.secret.key", "spark.eventLog.dir"} {
		if !strings.Contains(body.ClientConfig, want) {
			t.Errorf("the client configuration lacks %q", want)
		}
	}
	if body.Filename != "kumo-"+c.ID+".conf" {
		t.Errorf("filename = %q", body.Filename)
	}
	// Nowhere else: the cluster's GET carries no secret.
	got := doWithToken(t, sr.router, http.MethodGet, "/api/v1/spark/clusters/"+c.ID, "", sr.tokens.access)
	for _, line := range strings.Split(body.ClientConfig, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.HasSuffix(f[0], "secret") && strings.Contains(got.Body.String(), f[1]) {
			t.Errorf("GET /spark/clusters/{id} carries %s", f[0])
		}
	}
}

func TestSparkWritesAreAdminOnly(t *testing.T) {
	t.Parallel()
	sr := newSparkRouter(t, true)
	c := createActiveSparkCluster(t, sr)
	viewer, _ := sr.signer.IssueAccessToken("01J00000000000000000000000", auth.RoleViewer)
	for _, w := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/spark/clusters", createSparkBody},
		{http.MethodDelete, "/api/v1/spark/clusters/" + c.ID, ""},
		{http.MethodPost, "/api/v1/spark/clusters/" + c.ID + "/client-config", ""},
	} {
		if r := doWithToken(t, sr.router, w.method, w.path, w.body, viewer.Token); r.Code != http.StatusForbidden {
			t.Errorf("%s %s as a viewer = %d, want 403", w.method, w.path, r.Code)
		}
	}
	for _, path := range []string{"/api/v1/spark/clusters", "/api/v1/spark/clusters/" + c.ID} {
		if r := doWithToken(t, sr.router, http.MethodGet, path, "", viewer.Token); r.Code != http.StatusOK {
			t.Errorf("GET %s as a viewer = %d, want 200", path, r.Code)
		}
	}
	if r := do(t, sr.router, http.MethodGet, "/api/v1/spark/clusters", ""); r.Code != http.StatusUnauthorized {
		t.Errorf("GET without a token = %d, want 401", r.Code)
	}
}

func TestDeleteSparkCluster(t *testing.T) {
	t.Parallel()
	sr := newSparkRouter(t, true)
	c := createActiveSparkCluster(t, sr)
	r := doWithToken(t, sr.router, http.MethodDelete, "/api/v1/spark/clusters/"+c.ID, "", sr.tokens.access)
	if r.Code != http.StatusAccepted {
		t.Fatalf("DELETE = %d (%s)", r.Code, r.Body)
	}
	var queued sparkClusterBody
	decode(t, r, &queued)
	if queued.State != string(spark.StateDeleting) || queued.TaskID == nil {
		t.Fatalf("queued = %+v", queued)
	}
	if again := doWithToken(t, sr.router, http.MethodDelete, "/api/v1/spark/clusters/"+c.ID, "", sr.tokens.access); again.Code != http.StatusConflict {
		t.Errorf("a second DELETE = %d, want 409", again.Code)
	}
	waitForTaskState(t, sr.router, sr.tokens.access, *queued.TaskID, "succeeded")
	if got := doWithToken(t, sr.router, http.MethodGet, "/api/v1/spark/clusters/"+c.ID, "", sr.tokens.access); got.Code != http.StatusNotFound {
		t.Errorf("GET after the delete = %d, want 404", got.Code)
	}
}

func TestSparkOff(t *testing.T) {
	t.Parallel()
	sr := newSparkRouter(t, false)
	for _, w := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/spark/clusters"},
		{http.MethodPost, "/api/v1/spark/clusters"},
		{http.MethodPost, "/api/v1/spark/clusters/spk-0123456789abcdef0/client-config"},
	} {
		r := doWithToken(t, sr.router, w.method, w.path, "", sr.tokens.access)
		if r.Code != http.StatusServiceUnavailable || !strings.Contains(r.Body.String(), "KUMO_SPARK") {
			t.Errorf("%s %s with the module off = %d %s, want 503 naming the switch", w.method, w.path, r.Code, r.Body)
		}
	}
}
