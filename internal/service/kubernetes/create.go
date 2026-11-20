package kubernetes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/k8spki"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Bounds of a create request (section 3, and kubeadm's preflight minimums).
const (
	MaxNameLength       = 40
	MinNodeCount        = 1
	MaxNodeCount        = 10
	DefaultNodeCount    = 2
	DefaultInstanceType = "kumo.medium"
	// MinControlPlaneVCPUs and MinNodeMemoryMiB are kubeadm's: init refuses a
	// control plane with one CPU, and a node under about 1.7 GiB.
	MinControlPlaneVCPUs = 2
	MinNodeMemoryMiB     = 2048
	MaxRootDiskGB        = 500
)

var (
	// namePattern is a DNS label: the name is in the endpoint and in every
	// node's name.
	namePattern    = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)
	versionPattern = regexp.MustCompile(`^1\.\d{1,3}$`)
)

// CreateInput is a POST /eks/clusters body with the caller attached.
type CreateInput struct {
	Name string
	// KubernetesVersion may be empty: the newest available minor.
	KubernetesVersion string
	// SubnetID may be empty: the default subnet, as a launch does.
	SubnetID         string
	ControlPlaneType string
	NodeType         string
	NodeCount        int
	// NodeRootDiskGB may be 0: the image's min_disk_gb.
	NodeRootDiskGB int
	CreatedBy      string
}

// Create validates, generates the cluster's PKI, reserves every VM and queues
// the build, answering 202 with the cluster creating (section 4). Everything
// that can refuse the request happens before anything is written; then one
// transaction holds every VM, the cluster, its node group, its nodes and the
// task, so a cluster is reserved whole or not at all.
func (s *Service) Create(ctx context.Context, in CreateInput) (Cluster, error) {
	if in.ControlPlaneType == "" {
		in.ControlPlaneType = DefaultInstanceType
	}
	if in.NodeType == "" {
		in.NodeType = DefaultInstanceType
	}
	if in.NodeCount == 0 {
		in.NodeCount = DefaultNodeCount
	}
	if fields := validateCreate(in); len(fields) > 0 {
		return Cluster{}, invalid(fields...)
	}

	image, err := s.resolveImage(ctx, in.KubernetesVersion)
	if err != nil {
		return Cluster{}, err
	}
	in.KubernetesVersion = image.EngineVersion.String
	rootDiskGB := int(image.MinDiskGb)
	if in.NodeRootDiskGB == 0 {
		in.NodeRootDiskGB = rootDiskGB
	}
	if in.NodeRootDiskGB < rootDiskGB || in.NodeRootDiskGB > MaxRootDiskGB {
		return Cluster{}, invalid(service.FieldError{Field: "node_root_disk_gb",
			Reason: fmt.Sprintf("must be between %d (the image's minimum) and %d GiB", rootDiskGB, MaxRootDiskGB)})
	}
	if err := s.checkTypes(ctx, in); err != nil {
		return Cluster{}, err
	}

	// The PKI exists before any row does: the create task needs it to render
	// the control plane's seed, and the directory is written once
	// (k8spki.WriteDir). A create the reservation refuses removes it again.
	clusterID := id.K8sCluster()
	pki, err := k8spki.Generate(in.Name, s.now(), nil)
	if err != nil {
		return Cluster{}, fmt.Errorf("generate the PKI of cluster %q: %w", in.Name, err)
	}
	dir := s.pkiDir(clusterID)
	if err := k8spki.WriteDir(dir, pki); err != nil {
		return Cluster{}, fmt.Errorf("write the PKI of cluster %q: %w", in.Name, err)
	}

	reservations := []instance.ManagedReservation{{
		Name: controlPlaneName(in.Name, 1), Owner: instance.ManagedByKubernetes, ImageID: image.ID,
		InstanceType: in.ControlPlaneType, SubnetID: in.SubnetID, RootDiskGB: rootDiskGB,
		CreatedBy: in.CreatedBy,
	}}
	for i := 1; i <= in.NodeCount; i++ {
		reservations = append(reservations, instance.ManagedReservation{
			Name: workerName(in.Name, DefaultNodeGroup, i), Owner: instance.ManagedByKubernetes, ImageID: image.ID,
			InstanceType: in.NodeType, SubnetID: in.SubnetID, RootDiskGB: in.NodeRootDiskGB,
			CreatedBy: in.CreatedBy,
		})
	}

	var (
		created sqlc.K8sCluster
		group   sqlc.K8sNodeGroup
		taskID  string
		vms     []instance.ManagedReserved
	)
	err = s.instances.WithManagedReservations(ctx, reservations,
		func(ctx context.Context, tx *store.Tx, reserved []instance.ManagedReserved) error {
			vms = reserved
			now := formatTime(s.now())
			row, err := tx.CreateK8sCluster(ctx, sqlc.CreateK8sClusterParams{
				ID: clusterID, Name: in.Name, KubernetesVersion: in.KubernetesVersion, ImageID: image.ID,
				VpcID: reserved[0].VPCID, SubnetID: reserved[0].SubnetID,
				PodCidr: PodCIDR, ServiceCidr: ServiceCIDR,
				ControlPlaneCount: 1, ControlPlaneType: in.ControlPlaneType,
				State: string(StateCreating), CaCertPem: string(pki.CA.CertPEM),
				CreatedBy: nullable(in.CreatedBy), CreatedAt: now,
			})
			if err != nil {
				if isUniqueViolation(err) {
					return fmt.Errorf("%w: a cluster named %q already exists", service.ErrAlreadyExists, in.Name)
				}
				return fmt.Errorf("create the cluster row: %w", err)
			}
			created = row

			group, err = tx.CreateK8sNodeGroup(ctx, sqlc.CreateK8sNodeGroupParams{
				ID: id.NodeGroup(), ClusterID: clusterID, Name: DefaultNodeGroup,
				InstanceType: in.NodeType, RootDiskGb: int64(in.NodeRootDiskGB),
				DesiredSize: int64(in.NodeCount), CreatedAt: now,
			})
			if err != nil {
				return fmt.Errorf("create the node group: %w", err)
			}
			for i, vm := range reserved {
				params := sqlc.CreateK8sNodeParams{InstanceID: vm.InstanceID, ClusterID: clusterID, Role: RoleWorker,
					NodeGroupID: sql.NullString{String: group.ID, Valid: true}}
				if i == 0 {
					params.Role, params.NodeGroupID = RoleControlPlane, sql.NullString{}
				}
				if err := tx.CreateK8sNode(ctx, params); err != nil {
					return fmt.Errorf("record node %s: %w", vm.Name, err)
				}
			}

			queued, err := s.tasks.CreateInTx(ctx, tx, task.TypeK8sClusterCreate, clusterID,
				clusterPayload{ClusterID: clusterID})
			if err != nil {
				return err
			}
			taskID = queued.ID
			return nil
		})
	if err != nil {
		// Nothing committed: the PKI has no cluster to belong to.
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			s.logger.ErrorContext(ctx, "kubernetes: could not remove the PKI of a refused create",
				"cluster_id", clusterID, "error", rmErr)
		}
		return Cluster{}, err
	}

	out := fromRow(created)
	out.NodeGroups = []NodeGroup{groupFromRow(group)}
	for i, vm := range vms {
		role := RoleWorker
		if i == 0 {
			role = RoleControlPlane
		}
		out.Nodes = append(out.Nodes, Node{Name: vm.Name, InstanceID: vm.InstanceID, Role: role,
			PrivateIP: vm.PrivateIP, State: "pending"})
	}
	out.TaskID = &taskID

	s.logger.InfoContext(ctx, "kubernetes: create queued",
		"cluster_id", clusterID, "name", in.Name, "kubernetes_version", in.KubernetesVersion,
		"nodes", in.NodeCount, "task_id", taskID)
	return out, nil
}

func validateCreate(in CreateInput) []service.FieldError {
	var fields []service.FieldError
	if in.Name == "" || len(in.Name) > MaxNameLength || !namePattern.MatchString(in.Name) ||
		strings.Contains(in.Name, "--") {
		fields = append(fields, service.FieldError{Field: "name",
			Reason: fmt.Sprintf("must be a DNS label of 1-%d lowercase letters, digits and hyphens", MaxNameLength)})
	}
	if in.KubernetesVersion != "" && !versionPattern.MatchString(in.KubernetesVersion) {
		fields = append(fields, service.FieldError{Field: "kubernetes_version",
			Reason: "must be a minor version, such as 1.37"})
	}
	if in.NodeCount < MinNodeCount || in.NodeCount > MaxNodeCount {
		fields = append(fields, service.FieldError{Field: "node_count",
			Reason: fmt.Sprintf("must be between %d and %d", MinNodeCount, MaxNodeCount)})
	}
	if in.SubnetID != "" && !id.HasResourceShape(id.PrefixSubnet, in.SubnetID) {
		fields = append(fields, service.FieldError{Field: "subnet_id", Reason: "is not a subnet id"})
	}
	return fields
}

// checkTypes holds the instance types to kubeadm's minimums, which a node
// below would fail at its first boot rather than here.
func (s *Service) checkTypes(ctx context.Context, in CreateInput) error {
	for _, c := range []struct {
		field, name string
		minVCPUs    int
	}{
		{"control_plane_type", in.ControlPlaneType, MinControlPlaneVCPUs},
		{"node_type", in.NodeType, 1},
	} {
		preset, err := s.store.GetInstanceType(ctx, sqlc.GetInstanceTypeParams{ID: c.name})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return invalid(service.FieldError{Field: c.field, Reason: fmt.Sprintf("no instance type %q exists", c.name)})
			}
			return fmt.Errorf("read the instance type %q: %w", c.name, err)
		}
		if int(preset.Vcpus) < c.minVCPUs || preset.MemoryMib < MinNodeMemoryMiB {
			return invalid(service.FieldError{Field: c.field, Reason: fmt.Sprintf(
				"%s has %d vCPU and %d MiB; a Kubernetes node needs at least %d vCPU and %d MiB",
				c.name, preset.Vcpus, preset.MemoryMib, c.minVCPUs, MinNodeMemoryMiB)})
		}
	}
	return nil
}

// resolveImage picks the image a cluster boots from: the newest available
// kubernetes engine image, or the one of the version asked for.
func (s *Service) resolveImage(ctx context.Context, version string) (sqlc.Image, error) {
	if version == "" {
		available, err := s.store.ListAvailableEngineImages(ctx)
		if err != nil {
			return sqlc.Image{}, fmt.Errorf("list the engine images: %w", err)
		}
		for _, row := range available {
			if row.Engine.Valid && row.Engine.String == "kubernetes" && row.EngineVersion.Valid {
				version = row.EngineVersion.String
				break
			}
		}
		if version == "" {
			return sqlc.Image{}, fmt.Errorf("%w; import one first (packaging/images/kubernetes-1.37)", ErrNoEngineImage)
		}
	}
	row, err := s.store.GetAvailableEngineImage(ctx, sqlc.GetAvailableEngineImageParams{
		Engine: nullable("kubernetes"), EngineVersion: nullable(version),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.Image{}, fmt.Errorf("%w (Kubernetes %s)", ErrNoEngineImage, version)
		}
		return sqlc.Image{}, fmt.Errorf("read the Kubernetes %s image: %w", version, err)
	}
	return row, nil
}

// controlPlaneName and workerName name a cluster's VMs, which are their
// Kubernetes node names and their hostnames too.
func controlPlaneName(cluster string, n int) string { return fmt.Sprintf("%s-cp-%d", cluster, n) }
func workerName(cluster, group string, n int) string {
	return fmt.Sprintf("%s-%s-%d", cluster, group, n)
}

func invalid(fields ...service.FieldError) error {
	return &service.ValidationError{Message: "the cluster request is not acceptable", Fields: fields}
}

func nullable(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
