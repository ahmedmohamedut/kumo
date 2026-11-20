// Package managedcluster is the half of "run a group of VMs as one cluster"
// that does not depend on what runs inside them (docs/design/09-spark.md
// section 10). Kubernetes clusters (08-kubernetes.md) and Spark clusters
// (09-spark.md) share it: a node and its naming, the task payload, the bounded
// wait of a workflow, the endpoint's DNS record, the teardown that tolerates
// what is already gone, and the health pass's memory.
//
// What stays in each module: its tables and queries, its seeds, its API client,
// its readiness predicate and how it drains a node. The capacity, addressing
// and rollback of the VMs themselves are internal/service/instance/managed.go's.
package managedcluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service/cloudinit"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// Node is one of a cluster's VMs, as a module's node row and the instance it
// is describe it.
type Node struct {
	InstanceID string
	Name       string
	Role       string
	PrivateIP  string
}

// Seed wraps a node's user data into the cloud-init a managed launch takes.
func Seed(n Node, userData string) hypervisor.CloudInit {
	return hypervisor.CloudInit{
		MetaData:      cloudinit.MetaData(n.InstanceID, n.Name),
		UserData:      userData,
		NetworkConfig: cloudinit.NetworkConfig(),
	}
}

// WorkerName is a worker's VM and node name: <cluster>-<group>-<n>.
func WorkerName(cluster, group string, n int) string {
	return fmt.Sprintf("%s-%s-%d", cluster, group, n)
}

// WorkerIndex is a worker's number, the suffix of its name; 0 when it has none.
func WorkerIndex(name string) int {
	n, err := strconv.Atoi(name[strings.LastIndex(name, "-")+1:])
	if err != nil {
		return 0
	}
	return n
}

// HighestIndex is the largest worker number in use, so a scale-up names its
// new workers after it and never reuses a name a removed worker had.
func HighestIndex(workers []Node) int {
	highest := 0
	for _, w := range workers {
		highest = max(highest, WorkerIndex(w.Name))
	}
	return highest
}

// SortedByIndex is the workers oldest first.
func SortedByIndex(workers []Node) []Node {
	out := append([]Node(nil), workers...)
	sort.Slice(out, func(i, j int) bool { return WorkerIndex(out[i].Name) < WorkerIndex(out[j].Name) })
	return out
}

// Surplus is the workers a scale-down to desired removes, newest first: the
// order a scale-down drains them in.
func Surplus(workers []Node, desired int) []Node {
	sorted := SortedByIndex(workers)
	if desired < 0 {
		desired = 0
	}
	if len(sorted) <= desired {
		return nil
	}
	extra := sorted[desired:]
	out := make([]Node, len(extra))
	for i, n := range extra {
		out[len(extra)-1-i] = n
	}
	return out
}

// Payload is the input of every cluster task: only the cluster's id.
// Everything else lives on the rows, and nothing secret goes into a payload.
type Payload struct {
	ClusterID string `json:"cluster_id"`
}

// DecodePayload reads a task's cluster id; bad is the module's error for a
// payload that does not carry one.
func DecodePayload(t worker.Task, bad error) (string, error) {
	var p Payload
	if err := json.Unmarshal(t.Payload, &p); err != nil || p.ClusterID == "" {
		return "", fmt.Errorf("%w: task %s", bad, t.ID)
	}
	return p.ClusterID, nil
}

// ErrTimedOut is a Wait that ran out of time.
var ErrTimedOut = errors.New("managedcluster: timed out")

// Wait is the bounded poll of a cluster workflow: a condition is retried every
// Interval until it holds, the cluster is being deleted, or Timeout runs out.
type Wait struct {
	Timeout  time.Duration
	Interval time.Duration
	// Deleting reports whether a delete took the cluster over, which ends the
	// wait with Canceled: a create or a scale must not keep building what a
	// delete is removing.
	Deleting func(ctx context.Context) bool
	Canceled error
}

// For polls cond. It returns nil when cond holds, Canceled (wrapped) when the
// cluster is being deleted, ErrTimedOut (wrapped) past the timeout, or the
// context's error.
func (w Wait) For(ctx context.Context, clusterID, what string, cond func(context.Context) bool) error {
	deadline := time.Now().Add(w.Timeout)
	for {
		if cond(ctx) {
			return nil
		}
		if w.Deleting != nil && w.Deleting(ctx) {
			return fmt.Errorf("%w (cluster %s)", w.Canceled, clusterID)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w waiting for %s after %s", ErrTimedOut, what, w.Timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(w.Interval):
		}
	}
}

// PublishEndpoint makes name resolve to ip on the VPC's network. A record
// already there is an earlier attempt's, and is adopted.
func PublishEndpoint(ctx context.Context, hv hypervisor.Hypervisor, network, ip, name string) error {
	err := hv.AddDNSHost(ctx, network, hypervisor.DNSHost{IP: ip, Hostnames: []string{name}})
	if err != nil && !errors.Is(err, hypervisor.ErrAlreadyExists) {
		return fmt.Errorf("publish the endpoint %s: %w", name, err)
	}
	return nil
}

// RemoveEndpoint removes the record; one already gone is fine.
func RemoveEndpoint(ctx context.Context, hv hypervisor.Hypervisor, network, ip, name string) error {
	if ip == "" {
		return nil
	}
	err := hv.DeleteDNSHost(ctx, network, hypervisor.DNSHost{IP: ip, Hostnames: []string{name}})
	if err != nil && !errors.Is(err, hypervisor.ErrNotFound) {
		return fmt.Errorf("remove the endpoint %s: %w", name, err)
	}
	return nil
}

// Tearer removes one managed VM, tolerating what is already gone; it is
// instance.Service.TeardownManaged with the owner bound.
type Tearer func(ctx context.Context, instanceID string) error

// Teardown removes every node's VM, in order, skipping nodes with no instance.
func Teardown(ctx context.Context, tear Tearer, nodes []Node) error {
	for _, n := range nodes {
		if n.InstanceID == "" {
			continue
		}
		if err := tear(ctx, n.InstanceID); err != nil {
			return fmt.Errorf("tear down node %s: %w", n.Name, err)
		}
	}
	return nil
}

// Verdict is what a health pass does to a cluster's state.
type Verdict int

// The verdicts.
const (
	// Keep leaves the state as it is.
	Keep Verdict = iota
	// Recover moves a degraded cluster back to active.
	Recover
	// Degrade moves an active cluster to degraded.
	Degrade
	// Refresh keeps a cluster degraded with a reason that changed, so the
	// console says what is wrong now rather than what was wrong at first.
	Refresh
)

// Judge decides a pass: reason is empty for a healthy pass; failures counts
// the unhealthy passes in a row, this one included; threshold is how many it
// takes to degrade an active cluster. One healthy pass recovers a degraded one.
func Judge(degraded bool, reason, lastReason string, failures, threshold int) Verdict {
	switch {
	case reason == "" && degraded:
		return Recover
	case reason != "" && !degraded && failures >= threshold:
		return Degrade
	case reason != "" && degraded && reason != lastReason:
		return Refresh
	}
	return Keep
}

// Health is the memory of a module's health pass, shared by its workflows:
// when each cluster was last probed, how many passes in a row it failed, and
// each node's health as last seen.
type Health struct {
	interval time.Duration
	now      func() time.Time

	mu       sync.Mutex
	probedAt map[string]time.Time
	failures map[string]int
	nodes    map[string]map[string]bool
}

// NewHealth is an empty memory probing each cluster at most once per interval.
func NewHealth(interval time.Duration, now func() time.Time) *Health {
	if now == nil {
		now = time.Now
	}
	return &Health{
		interval: interval, now: now,
		probedAt: map[string]time.Time{}, failures: map[string]int{}, nodes: map[string]map[string]bool{},
	}
}

// Due reports whether a cluster is due a probe, and if so records that it is
// being probed now.
func (h *Health) Due(clusterID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if last, ok := h.probedAt[clusterID]; ok && now.Sub(last) < h.interval {
		return false
	}
	h.probedAt[clusterID] = now
	return true
}

// Record counts a pass and returns the unhealthy passes in a row.
func (h *Health) Record(clusterID string, healthy bool) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if healthy {
		h.failures[clusterID] = 0
	} else {
		h.failures[clusterID]++
	}
	return h.failures[clusterID]
}

// RecordNodes keeps each node's health as seen now.
func (h *Health) RecordNodes(clusterID string, healthy map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nodes[clusterID] = healthy
}

// Nodes is each node's health as last seen; empty when nobody has looked.
func (h *Health) Nodes(clusterID string) map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]bool{}
	for name, healthy := range h.nodes[clusterID] {
		out[name] = healthy
	}
	return out
}

// Forget drops a deleted cluster.
func (h *Health) Forget(clusterID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.probedAt, clusterID)
	delete(h.failures, clusterID)
	delete(h.nodes, clusterID)
}
