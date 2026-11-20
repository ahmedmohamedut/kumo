package spark

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/accesskey"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/managedcluster"
	"github.com/ahmedmohamedut/kumo/internal/service/sparkcloudinit"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Bounds of a create request (section 4).
const (
	MaxNameLength        = 40
	MinWorkerCount       = 1
	MaxWorkerCount       = 10
	DefaultWorkerCount   = 2
	DefaultMasterType    = "kumo.medium"
	DefaultWorkerType    = "kumo.large"
	MinMasterVCPUs       = 2
	MinMasterMemoryMiB   = 4096
	MinWorkerMemoryMiB   = 2048
	MaxBucketsPerCluster = 10
)

var (
	namePattern    = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)
	versionPattern = regexp.MustCompile(`^\d{1,2}\.\d{1,3}$`)
)

// CreateInput is a POST /spark/clusters body with the caller attached.
type CreateInput struct {
	Name string
	// SparkVersion may be empty: the newest available minor.
	SparkVersion string
	// SubnetID may be empty: the default subnet.
	SubnetID    string
	MasterType  string
	WorkerType  string
	WorkerCount int
	// ObjectStorage gives the cluster an access key of its own, read-write
	// on Buckets (S9). EventLogBucket, one of them, turns on event logs and
	// the history server (S10).
	ObjectStorage  bool
	Buckets        []string
	EventLogBucket string
	CreatedBy      string
}

// Create validates, makes the cluster's secrets and access key, reserves every
// VM and queues the build, answering 202 with the cluster creating (section
// 4). Everything that can refuse the request happens before anything is
// written, and a reservation that is refused takes the key and the secrets
// with it: a cluster is created whole or not at all.
func (s *Service) Create(ctx context.Context, in CreateInput) (Cluster, error) {
	if in.MasterType == "" {
		in.MasterType = DefaultMasterType
	}
	if in.WorkerType == "" {
		in.WorkerType = DefaultWorkerType
	}
	if in.WorkerCount == 0 {
		in.WorkerCount = DefaultWorkerCount
	}
	if fields := validateCreate(in); len(fields) > 0 {
		return Cluster{}, invalid(fields...)
	}
	if err := s.checkObjectStorage(ctx, in); err != nil {
		return Cluster{}, err
	}
	image, err := s.resolveImage(ctx, in.SparkVersion)
	if err != nil {
		return Cluster{}, err
	}
	in.SparkVersion = image.EngineVersion.String
	rootDiskGB := int(image.MinDiskGb)
	if err := s.checkTypes(ctx, in); err != nil {
		return Cluster{}, err
	}

	clusterID := id.SparkCluster()
	secret, err := sparkcloudinit.NewSecret(nil)
	if err != nil {
		return Cluster{}, err
	}
	var accessKey string
	var s3Secret sparkcloudinit.Secret
	if in.ObjectStorage {
		key, raw, err := accesskey.NewCredentials()
		if err != nil {
			return Cluster{}, err
		}
		accessKey, s3Secret = key, sparkcloudinit.Secret(raw)
		if err := s.objects.CreateAccessKey(ctx, objectstore.AccessKeySpec{
			AccessKey: accessKey, SecretKey: raw,
			Grant: objectstore.Grant{Buckets: in.Buckets, Level: objectstore.AccessReadWrite},
		}); err != nil {
			return Cluster{}, fmt.Errorf("create the cluster's access key: %w", err)
		}
	}
	undo := func() {
		ctx := context.WithoutCancel(ctx)
		if accessKey != "" {
			if err := s.objects.DeleteAccessKey(ctx, accessKey); err != nil {
				s.logger.ErrorContext(ctx, "spark: could not remove the access key of a refused create",
					"cluster_id", clusterID, "access_key", accessKey, "error", err)
			}
		}
		if err := s.removeSecrets(ctx, clusterID); err != nil {
			s.logger.ErrorContext(ctx, "spark: could not remove the secrets of a refused create",
				"cluster_id", clusterID, "error", err)
		}
	}
	if err := s.writeSecrets(clusterID, secret, s3Secret); err != nil {
		undo()
		return Cluster{}, err
	}

	reservations := []instance.ManagedReservation{{
		Name: masterName(in.Name), Owner: instance.ManagedBySpark, ImageID: image.ID,
		InstanceType: in.MasterType, SubnetID: in.SubnetID, RootDiskGB: rootDiskGB, CreatedBy: in.CreatedBy,
	}}
	for i := 1; i <= in.WorkerCount; i++ {
		reservations = append(reservations, instance.ManagedReservation{
			Name: managedcluster.WorkerName(in.Name, DefaultNodeGroup, i), Owner: instance.ManagedBySpark,
			ImageID: image.ID, InstanceType: in.WorkerType, SubnetID: in.SubnetID, RootDiskGB: rootDiskGB,
			CreatedBy: in.CreatedBy,
		})
	}

	var (
		created sqlc.SparkCluster
		group   sqlc.SparkNodeGroup
		taskID  string
		vms     []instance.ManagedReserved
	)
	err = s.instances.WithManagedReservations(ctx, reservations,
		func(ctx context.Context, tx *store.Tx, reserved []instance.ManagedReserved) error {
			vms = reserved
			now := formatTime(s.now())
			params := sqlc.CreateSparkClusterParams{
				ID: clusterID, Name: in.Name, SparkVersion: in.SparkVersion, ImageID: image.ID,
				VpcID: reserved[0].VPCID, SubnetID: reserved[0].SubnetID, MasterType: in.MasterType,
				State: string(StateCreating), EventLogBucket: nullable(in.EventLogBucket),
				CreatedBy: nullable(in.CreatedBy), CreatedAt: now,
			}
			if in.ObjectStorage {
				params.ObjectStorage, params.AccessKeyID = 1, nullable(accessKey)
			}
			row, err := tx.CreateSparkCluster(ctx, params)
			if err != nil {
				if isUniqueViolation(err) {
					return fmt.Errorf("%w: a Spark cluster named %q already exists", service.ErrAlreadyExists, in.Name)
				}
				return fmt.Errorf("create the cluster row: %w", err)
			}
			created = row
			for _, b := range in.Buckets {
				if err := tx.AddSparkClusterBucket(ctx, sqlc.AddSparkClusterBucketParams{ClusterID: clusterID, Bucket: b}); err != nil {
					return fmt.Errorf("record bucket %q: %w", b, err)
				}
			}
			group, err = tx.CreateSparkNodeGroup(ctx, sqlc.CreateSparkNodeGroupParams{
				ID: id.NodeGroup(), ClusterID: clusterID, Name: DefaultNodeGroup, InstanceType: in.WorkerType,
				RootDiskGb: int64(rootDiskGB), DesiredSize: int64(in.WorkerCount), CreatedAt: now,
			})
			if err != nil {
				return fmt.Errorf("create the node group: %w", err)
			}
			for i, vm := range reserved {
				p := sqlc.CreateSparkNodeParams{InstanceID: vm.InstanceID, ClusterID: clusterID, Role: RoleWorker,
					NodeGroupID: sql.NullString{String: group.ID, Valid: true}}
				if i == 0 {
					p.Role, p.NodeGroupID = RoleMaster, sql.NullString{}
				}
				if err := tx.CreateSparkNode(ctx, p); err != nil {
					return fmt.Errorf("record node %s: %w", vm.Name, err)
				}
			}
			queued, err := s.tasks.CreateInTx(ctx, tx, task.TypeSparkClusterCreate, clusterID,
				managedcluster.Payload{ClusterID: clusterID})
			if err != nil {
				return err
			}
			taskID = queued.ID
			return nil
		})
	if err != nil {
		undo()
		return Cluster{}, err
	}

	out := fromRow(created, in.Buckets)
	out.NodeGroups = []NodeGroup{groupFromRow(group)}
	for i, vm := range vms {
		role := RoleWorker
		if i == 0 {
			role = RoleMaster
		}
		out.Nodes = append(out.Nodes, Node{Name: vm.Name, InstanceID: vm.InstanceID, Role: role,
			PrivateIP: vm.PrivateIP, State: "pending"})
	}
	out.TaskID = &taskID
	s.logger.InfoContext(ctx, "spark: create queued", "cluster_id", clusterID, "name", in.Name,
		"spark_version", in.SparkVersion, "workers", in.WorkerCount, "object_storage", in.ObjectStorage,
		"task_id", taskID)
	return out, nil
}

func validateCreate(in CreateInput) []service.FieldError {
	var fields []service.FieldError
	if in.Name == "" || len(in.Name) > MaxNameLength || !namePattern.MatchString(in.Name) ||
		strings.Contains(in.Name, "--") {
		fields = append(fields, service.FieldError{Field: "name",
			Reason: fmt.Sprintf("must be a DNS label of 1-%d lowercase letters, digits and hyphens", MaxNameLength)})
	}
	if in.SparkVersion != "" && !versionPattern.MatchString(in.SparkVersion) {
		fields = append(fields, service.FieldError{Field: "spark_version", Reason: "must be a minor version, such as 4.3"})
	}
	if in.WorkerCount < MinWorkerCount || in.WorkerCount > MaxWorkerCount {
		fields = append(fields, service.FieldError{Field: "worker_count",
			Reason: fmt.Sprintf("must be between %d and %d", MinWorkerCount, MaxWorkerCount)})
	}
	if in.SubnetID != "" && !id.HasResourceShape(id.PrefixSubnet, in.SubnetID) {
		fields = append(fields, service.FieldError{Field: "subnet_id", Reason: "is not a subnet id"})
	}
	if !in.ObjectStorage {
		if len(in.Buckets) > 0 {
			fields = append(fields, service.FieldError{Field: "buckets", Reason: "needs object_storage"})
		}
		if in.EventLogBucket != "" {
			fields = append(fields, service.FieldError{Field: "event_log_bucket",
				Reason: "needs object_storage: event logs are written by each job's driver, never on the master"})
		}
		return fields
	}
	if len(in.Buckets) == 0 || len(in.Buckets) > MaxBucketsPerCluster {
		fields = append(fields, service.FieldError{Field: "buckets",
			Reason: fmt.Sprintf("must name 1-%d buckets with object_storage", MaxBucketsPerCluster)})
	}
	seen := map[string]bool{}
	for _, b := range in.Buckets {
		if seen[b] {
			fields = append(fields, service.FieldError{Field: "buckets", Reason: fmt.Sprintf("names %q twice", b)})
		}
		seen[b] = true
	}
	if in.EventLogBucket != "" && !slices.Contains(in.Buckets, in.EventLogBucket) {
		fields = append(fields, service.FieldError{Field: "event_log_bucket", Reason: "must be one of buckets"})
	}
	return fields
}

// checkObjectStorage refuses a cluster that asks for object storage this node
// cannot give it: the module off, a guest endpoint no VPC can reach, an
// isolated VPC, or a bucket that does not exist.
func (s *Service) checkObjectStorage(ctx context.Context, in CreateInput) error {
	if !in.ObjectStorage {
		return nil
	}
	if s.objects == nil {
		return invalid(service.FieldError{Field: "object_storage",
			Reason: "object storage is off on this node (KUMO_OBJECTSTORE)"})
	}
	if reason := unroutable(s.s3Endpoint); reason != "" {
		return fmt.Errorf("%w: the guests cannot reach object storage: %s; set KUMO_SPARK_S3_ENDPOINT "+
			"(docs/design/09-spark.md section 2)", service.ErrConflict, reason)
	}
	subnet, err := s.subnetOf(ctx, in.SubnetID)
	if err != nil {
		return err
	}
	vpc, err := s.store.GetVPC(ctx, sqlc.GetVPCParams{ID: subnet.VpcID})
	if err != nil {
		return fmt.Errorf("read the VPC of subnet %q: %w", subnet.ID, err)
	}
	if vpc.Mode == "isolated" {
		return invalid(service.FieldError{Field: "object_storage",
			Reason: fmt.Sprintf("VPC %q is isolated, so its guests cannot reach object storage", vpc.Name)})
	}
	for _, b := range in.Buckets {
		if _, err := s.objects.HeadBucket(ctx, b); err != nil {
			if errors.Is(err, objectstore.ErrNotFound) {
				return invalid(service.FieldError{Field: "buckets", Reason: fmt.Sprintf("no bucket %q exists", b)})
			}
			return fmt.Errorf("read bucket %q: %w", b, err)
		}
	}
	return nil
}

// unroutable says why a guest could not reach the endpoint, or "".
func unroutable(endpoint string) string {
	if endpoint == "" {
		return "no S3 endpoint is configured for the guests"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return fmt.Sprintf("%q is not a URL", endpoint)
	}
	if u.Hostname() == "localhost" {
		return endpoint + " is the host's loopback"
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
		return endpoint + " is the host's loopback"
	}
	return ""
}

func (s *Service) subnetOf(ctx context.Context, subnetID string) (sqlc.Subnet, error) {
	if subnetID == "" {
		row, err := s.store.GetDefaultSubnet(ctx)
		if err != nil {
			return sqlc.Subnet{}, fmt.Errorf("%w: no default subnet", service.ErrConflict)
		}
		return row, nil
	}
	row, err := s.store.GetSubnet(ctx, sqlc.GetSubnetParams{ID: subnetID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.Subnet{}, invalid(service.FieldError{Field: "subnet_id", Reason: "no such subnet"})
		}
		return sqlc.Subnet{}, fmt.Errorf("read subnet %q: %w", subnetID, err)
	}
	return row, nil
}

// checkTypes holds the instance types to what a node needs: a master runs
// the master and the history server, a worker its executors beside the OS.
func (s *Service) checkTypes(ctx context.Context, in CreateInput) error {
	for _, c := range []struct {
		field, name string
		vcpus, mib  int
	}{
		{"master_type", in.MasterType, MinMasterVCPUs, MinMasterMemoryMiB},
		{"worker_type", in.WorkerType, 1, MinWorkerMemoryMiB},
	} {
		preset, err := s.store.GetInstanceType(ctx, sqlc.GetInstanceTypeParams{ID: c.name})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return invalid(service.FieldError{Field: c.field, Reason: fmt.Sprintf("no instance type %q exists", c.name)})
			}
			return fmt.Errorf("read the instance type %q: %w", c.name, err)
		}
		if int(preset.Vcpus) < c.vcpus || int(preset.MemoryMib) < c.mib {
			return invalid(service.FieldError{Field: c.field, Reason: fmt.Sprintf(
				"%s has %d vCPU and %d MiB; needs at least %d vCPU and %d MiB",
				c.name, preset.Vcpus, preset.MemoryMib, c.vcpus, c.mib)})
		}
	}
	return nil
}

// resolveImage picks the image a cluster boots from: the newest available
// Spark engine image, or the one of the version asked for.
func (s *Service) resolveImage(ctx context.Context, version string) (sqlc.Image, error) {
	if version == "" {
		available, err := s.store.ListAvailableEngineImages(ctx)
		if err != nil {
			return sqlc.Image{}, fmt.Errorf("list the engine images: %w", err)
		}
		for _, row := range available {
			if row.Engine.Valid && row.Engine.String == "spark" && row.EngineVersion.Valid {
				version = row.EngineVersion.String
				break
			}
		}
		if version == "" {
			return sqlc.Image{}, fmt.Errorf("%w; import one first (packaging/images/spark-4.3)", ErrNoEngineImage)
		}
	}
	row, err := s.store.GetAvailableEngineImage(ctx, sqlc.GetAvailableEngineImageParams{
		Engine: nullable("spark"), EngineVersion: nullable(version),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.Image{}, fmt.Errorf("%w (Spark %s)", ErrNoEngineImage, version)
		}
		return sqlc.Image{}, fmt.Errorf("read the Spark %s image: %w", version, err)
	}
	return row, nil
}

// masterName names the master's VM, its hostname too.
func masterName(cluster string) string { return cluster + "-master" }

func invalid(fields ...service.FieldError) error {
	return &service.ValidationError{Message: "the cluster request is not acceptable", Fields: fields}
}

func nullable(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
