package kubernetes

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/k8spki"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Cluster is a cluster as the API returns it (section 8).
type Cluster struct {
	ID                string      `json:"id"`
	Name              string      `json:"name"`
	KubernetesVersion string      `json:"kubernetes_version"`
	State             State       `json:"state"`
	StateReason       *string     `json:"state_reason"`
	Endpoint          *string     `json:"endpoint"`
	VPCID             string      `json:"vpc_id"`
	SubnetID          string      `json:"subnet_id"`
	PodCIDR           string      `json:"pod_cidr"`
	ServiceCIDR       string      `json:"service_cidr"`
	ControlPlaneType  string      `json:"control_plane_type"`
	CACertificate     string      `json:"ca_certificate"`
	NodeGroups        []NodeGroup `json:"node_groups"`
	Nodes             []Node      `json:"nodes"`
	CreatedAt         time.Time   `json:"created_at"`
	UpdatedAt         time.Time   `json:"updated_at"`
	// TaskID is the task a create or delete queued.
	TaskID *string `json:"task_id,omitempty"`
}

// NodeGroup is a cluster's group of workers.
type NodeGroup struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	InstanceType string `json:"instance_type"`
	RootDiskGB   int    `json:"root_disk_gb"`
	DesiredSize  int    `json:"desired_size"`
}

// Node is one of a cluster's VMs. Ready is the Kubernetes node's condition as
// the last health pass saw it; null when nobody has asked yet.
type Node struct {
	Name        string  `json:"name"`
	InstanceID  string  `json:"instance_id"`
	Role        string  `json:"role"`
	NodeGroupID *string `json:"node_group_id"`
	State       string  `json:"state"`
	PrivateIP   string  `json:"private_ip"`
	Ready       *bool   `json:"ready"`
}

// Page is one page of clusters.
type Page struct {
	Items         []Cluster `json:"items"`
	NextPageToken *string   `json:"next_page_token"`
}

// Paging bounds, as every collection's.
const (
	DefaultPageSize = 50
	MinPageSize     = 1
	MaxPageSize     = 200
)

// ListInput filters GET /eks/clusters.
type ListInput struct {
	Query     string
	State     State
	PageSize  int
	PageToken string
}

func fromRow(row sqlc.K8sCluster) Cluster {
	c := Cluster{
		ID: row.ID, Name: row.Name, KubernetesVersion: row.KubernetesVersion, State: State(row.State),
		VPCID: row.VpcID, SubnetID: row.SubnetID, PodCIDR: row.PodCidr, ServiceCIDR: row.ServiceCidr,
		ControlPlaneType: row.ControlPlaneType, CACertificate: row.CaCertPem,
		NodeGroups: []NodeGroup{}, Nodes: []Node{},
		CreatedAt: parseTime(row.CreatedAt), UpdatedAt: parseTime(row.UpdatedAt),
	}
	if row.StateReason.Valid {
		c.StateReason = &row.StateReason.String
	}
	if row.Endpoint.Valid {
		c.Endpoint = &row.Endpoint.String
	}
	return c
}

func groupFromRow(row sqlc.K8sNodeGroup) NodeGroup {
	return NodeGroup{ID: row.ID, Name: row.Name, InstanceType: row.InstanceType,
		RootDiskGB: int(row.RootDiskGb), DesiredSize: int(row.DesiredSize)}
}

// Get returns one cluster with its node group and its nodes.
func (s *Service) Get(ctx context.Context, clusterID string) (Cluster, error) {
	if !id.HasResourceShape(id.PrefixK8sCluster, clusterID) {
		return Cluster{}, fmt.Errorf("%w: no cluster %q", service.ErrNotFound, clusterID)
	}
	row, err := s.store.GetK8sCluster(ctx, sqlc.GetK8sClusterParams{ID: clusterID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Cluster{}, fmt.Errorf("%w: no cluster %q", service.ErrNotFound, clusterID)
		}
		return Cluster{}, fmt.Errorf("get cluster %q: %w", clusterID, err)
	}
	return s.describe(ctx, row)
}

func (s *Service) describe(ctx context.Context, row sqlc.K8sCluster) (Cluster, error) {
	out := fromRow(row)
	groups, err := s.store.ListK8sNodeGroups(ctx, sqlc.ListK8sNodeGroupsParams{ClusterID: row.ID})
	if err != nil {
		return Cluster{}, fmt.Errorf("list the node groups of %q: %w", row.ID, err)
	}
	for _, g := range groups {
		out.NodeGroups = append(out.NodeGroups, groupFromRow(g))
	}
	nodes, err := s.store.ListK8sNodes(ctx, sqlc.ListK8sNodesParams{ClusterID: row.ID})
	if err != nil {
		return Cluster{}, fmt.Errorf("list the nodes of %q: %w", row.ID, err)
	}
	ready := s.health.Nodes(row.ID)
	for _, n := range nodes {
		node := Node{Name: n.Name, InstanceID: n.InstanceID, Role: n.Role, State: n.State,
			PrivateIP: n.PrivateIp.String}
		if n.NodeGroupID.Valid {
			node.NodeGroupID = &n.NodeGroupID.String
		}
		if r, ok := ready[n.Name]; ok {
			node.Ready = &r
		}
		out.Nodes = append(out.Nodes, node)
	}
	return out, nil
}

// List returns one page of clusters, newest first.
func (s *Service) List(ctx context.Context, in ListInput) (Page, error) {
	if in.State != "" && !knownState(in.State) {
		return Page{}, invalid(service.FieldError{Field: "state",
			Reason: "must be creating, active, updating, degraded, deleting or failed"})
	}
	if in.PageSize == 0 {
		in.PageSize = DefaultPageSize
	}
	if in.PageSize < MinPageSize || in.PageSize > MaxPageSize {
		return Page{}, invalid(service.FieldError{Field: "page_size",
			Reason: fmt.Sprintf("must be between %d and %d", MinPageSize, MaxPageSize)})
	}
	cursor, err := decodePageToken(in.PageToken)
	if err != nil {
		return Page{}, err
	}

	rows, err := s.store.ListK8sClusters(ctx, sqlc.ListK8sClustersParams{
		Query: in.Query, StateFilter: string(in.State),
		CursorCreated: cursor.CreatedAt, CursorID: cursor.ID, PageLimit: int64(in.PageSize + 1),
	})
	if err != nil {
		return Page{}, fmt.Errorf("list clusters: %w", err)
	}
	page := Page{Items: []Cluster{}}
	for i, row := range rows {
		if i == in.PageSize {
			token := encodePageToken(pageCursor{CreatedAt: rows[i-1].CreatedAt, ID: rows[i-1].ID})
			page.NextPageToken = &token
			break
		}
		c, err := s.describe(ctx, row)
		if err != nil {
			return Page{}, err
		}
		page.Items = append(page.Items, c)
	}
	return page, nil
}

type pageCursor struct {
	CreatedAt string `json:"c"`
	ID        string `json:"i"`
}

func encodePageToken(c pageCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodePageToken(token string) (pageCursor, error) {
	if token == "" {
		return pageCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	var c pageCursor
	if err == nil {
		err = json.Unmarshal(raw, &c)
	}
	if err != nil || c.CreatedAt == "" || c.ID == "" {
		return pageCursor{}, invalid(service.FieldError{Field: "page_token", Reason: "is not a token this API issued"})
	}
	return c, nil
}

// Kubeconfig is a minted kubeconfig and when its certificate expires.
type Kubeconfig struct {
	Kubeconfig k8spki.Secret `json:"-"`
	ExpiresAt  time.Time     `json:"expires_at"`
	Group      string        `json:"group"`
}

// MintKubeconfig signs a kubeconfig for the caller: kumo:admins for an admin,
// kumo:viewers for a viewer (K10). Nothing is stored (section 5).
func (s *Service) MintKubeconfig(ctx context.Context, clusterID, username string, role auth.Role) (Kubeconfig, error) {
	cluster, err := s.Get(ctx, clusterID)
	if err != nil {
		return Kubeconfig{}, err
	}
	if cluster.State != StateActive && cluster.State != StateDegraded && cluster.State != StateUpdating {
		return Kubeconfig{}, fmt.Errorf("%w: cluster %q is %s; a kubeconfig is minted once it is active",
			service.ErrConflict, clusterID, cluster.State)
	}
	group := k8spki.GroupViewers
	if role == auth.RoleAdmin {
		group = k8spki.GroupAdmins
	}
	pki, err := k8spki.LoadDir(s.pkiDir(clusterID))
	if err != nil {
		return Kubeconfig{}, fmt.Errorf("read the PKI of cluster %q: %w", clusterID, err)
	}
	now := s.now()
	raw, err := pki.MintKubeconfig(k8spki.KubeconfigRequest{
		ClusterName: cluster.Name, Server: *cluster.Endpoint, User: username, Group: group, Now: now,
	}, nil)
	if err != nil {
		return Kubeconfig{}, err
	}
	s.logger.InfoContext(ctx, "kubernetes: kubeconfig minted",
		"cluster_id", clusterID, "user", username, "group", group)
	return Kubeconfig{Kubeconfig: raw, ExpiresAt: now.Add(k8spki.KubeconfigValidity), Group: group}, nil
}
