package spark

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/managedcluster"
	"github.com/ahmedmohamedut/kumo/internal/service/sparkcloudinit"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Cluster is a Spark cluster as the API returns it.
type Cluster struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	SparkVersion string  `json:"spark_version"`
	State        State   `json:"state"`
	StateReason  *string `json:"state_reason"`
	// Endpoint is the master URL, spark://<name>.<vpc>:7077, once active.
	Endpoint *string `json:"endpoint"`
	// MasterUIURL and HistoryURL are the web UIs, reachable from the VPC.
	// HistoryURL is null without an event-log bucket (S10).
	MasterUIURL    *string     `json:"master_ui_url"`
	HistoryURL     *string     `json:"history_url"`
	VPCID          string      `json:"vpc_id"`
	SubnetID       string      `json:"subnet_id"`
	MasterType     string      `json:"master_type"`
	ObjectStorage  bool        `json:"object_storage"`
	Buckets        []string    `json:"buckets"`
	EventLogBucket *string     `json:"event_log_bucket"`
	NodeGroups     []NodeGroup `json:"node_groups"`
	Nodes          []Node      `json:"nodes"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
	// TaskID is the task a create or a delete queued.
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

// Node is one of a cluster's VMs. Alive is what the master last said of it:
// the master's own state for the master, registered ALIVE for a worker; null
// when nobody has asked yet.
type Node struct {
	Name        string  `json:"name"`
	InstanceID  string  `json:"instance_id"`
	Role        string  `json:"role"`
	NodeGroupID *string `json:"node_group_id"`
	State       string  `json:"state"`
	PrivateIP   string  `json:"private_ip"`
	Alive       *bool   `json:"alive"`
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

// ListInput filters GET /spark/clusters.
type ListInput struct {
	Query     string
	State     State
	PageSize  int
	PageToken string
}

func fromRow(row sqlc.SparkCluster, buckets []string) Cluster {
	out := Cluster{
		ID: row.ID, Name: row.Name, SparkVersion: row.SparkVersion, State: State(row.State),
		VPCID: row.VpcID, SubnetID: row.SubnetID, MasterType: row.MasterType,
		ObjectStorage: row.ObjectStorage == 1, Buckets: buckets,
		CreatedAt: parseTime(row.CreatedAt), UpdatedAt: parseTime(row.UpdatedAt),
		NodeGroups: []NodeGroup{}, Nodes: []Node{},
	}
	if out.Buckets == nil {
		out.Buckets = []string{}
	}
	if row.StateReason.Valid {
		out.StateReason = &row.StateReason.String
	}
	if row.EventLogBucket.Valid {
		out.EventLogBucket = &row.EventLogBucket.String
	}
	if row.Endpoint.Valid {
		endpoint := row.Endpoint.String
		out.Endpoint = &endpoint
		// The UIs are on the endpoint's host: spark://<host>:7077.
		host := strings.TrimSuffix(strings.TrimPrefix(endpoint, "spark://"), ":"+strconv.Itoa(sparkcloudinit.MasterPort))
		ui := "http://" + net.JoinHostPort(host, strconv.Itoa(sparkcloudinit.MasterUIPort))
		out.MasterUIURL = &ui
		if row.EventLogBucket.Valid {
			history := "http://" + net.JoinHostPort(host, strconv.Itoa(sparkcloudinit.HistoryPort))
			out.HistoryURL = &history
		}
	}
	return out
}

func groupFromRow(row sqlc.SparkNodeGroup) NodeGroup {
	return NodeGroup{ID: row.ID, Name: row.Name, InstanceType: row.InstanceType,
		RootDiskGB: int(row.RootDiskGb), DesiredSize: int(row.DesiredSize)}
}

// Get returns one cluster with its node group and its nodes.
func (s *Service) Get(ctx context.Context, clusterID string) (Cluster, error) {
	row, err := s.row(ctx, clusterID)
	if err != nil {
		return Cluster{}, err
	}
	return s.describe(ctx, row)
}

func (s *Service) row(ctx context.Context, clusterID string) (sqlc.SparkCluster, error) {
	if !id.HasResourceShape(id.PrefixSparkCluster, clusterID) {
		return sqlc.SparkCluster{}, fmt.Errorf("%w: no Spark cluster %q", service.ErrNotFound, clusterID)
	}
	row, err := s.store.GetSparkCluster(ctx, sqlc.GetSparkClusterParams{ID: clusterID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.SparkCluster{}, fmt.Errorf("%w: no Spark cluster %q", service.ErrNotFound, clusterID)
		}
		return sqlc.SparkCluster{}, fmt.Errorf("get cluster %q: %w", clusterID, err)
	}
	return row, nil
}

func (s *Service) describe(ctx context.Context, row sqlc.SparkCluster) (Cluster, error) {
	buckets, err := s.store.ListSparkClusterBuckets(ctx, sqlc.ListSparkClusterBucketsParams{ClusterID: row.ID})
	if err != nil {
		return Cluster{}, fmt.Errorf("list the buckets of %q: %w", row.ID, err)
	}
	out := fromRow(row, buckets)
	groups, err := s.store.ListSparkNodeGroups(ctx, sqlc.ListSparkNodeGroupsParams{ClusterID: row.ID})
	if err != nil {
		return Cluster{}, fmt.Errorf("list the node groups of %q: %w", row.ID, err)
	}
	for _, g := range groups {
		out.NodeGroups = append(out.NodeGroups, groupFromRow(g))
	}
	nodes, err := s.store.ListSparkNodes(ctx, sqlc.ListSparkNodesParams{ClusterID: row.ID})
	if err != nil {
		return Cluster{}, fmt.Errorf("list the nodes of %q: %w", row.ID, err)
	}
	alive := s.health.Nodes(row.ID)
	for _, n := range nodes {
		node := Node{Name: n.Name, InstanceID: n.InstanceID, Role: n.Role, State: n.State, PrivateIP: n.PrivateIp.String}
		if n.NodeGroupID.Valid {
			node.NodeGroupID = &n.NodeGroupID.String
		}
		if a, ok := alive[n.Name]; ok {
			node.Alive = &a
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
	rows, err := s.store.ListSparkClusters(ctx, sqlc.ListSparkClustersParams{
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

// ClientConfig renders the client configuration of a cluster (section 5,
// option A): what spark-submit from a VPC instance needs, the Spark secret and
// the cluster's S3 key included. The handler gives it to admins only.
func (s *Service) ClientConfig(ctx context.Context, clusterID string) (sparkcloudinit.Secret, error) {
	row, err := s.row(ctx, clusterID)
	if err != nil {
		return "", err
	}
	switch State(row.State) {
	case StateActive, StateDegraded, StateUpdating:
	default:
		return "", fmt.Errorf("%w: cluster %q is %s; its client configuration is ready once it is active",
			service.ErrConflict, clusterID, row.State)
	}
	vpc, err := s.store.GetVPC(ctx, sqlc.GetVPCParams{ID: row.VpcID})
	if err != nil {
		return "", fmt.Errorf("read the VPC of cluster %q: %w", clusterID, err)
	}
	secret, s3Secret, err := s.secrets(clusterID)
	if err != nil {
		return "", err
	}
	conf, err := sparkcloudinit.ClientConfig(sparkcloudinit.ClientRequest{
		ClusterID: row.ID, Endpoint: endpointName(row.Name, vpc.Name), Secret: secret,
		S3: s.s3(row, s3Secret), EventLogBucket: row.EventLogBucket.String,
	})
	if err != nil {
		return "", fmt.Errorf("render the client configuration of cluster %q: %w", clusterID, err)
	}
	s.logger.InfoContext(ctx, "spark: client configuration issued", "cluster_id", clusterID)
	return conf, nil
}

// Make sure the shared node shape stays the one the layout builds.
var _ = managedcluster.Node{}
