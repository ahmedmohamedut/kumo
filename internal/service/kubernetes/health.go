package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/k8sapi"
	"github.com/ahmedmohamedut/kumo/internal/service/k8spki"
	"github.com/ahmedmohamedut/kumo/internal/service/managedcluster"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// HealthReport is what one health pass did.
type HealthReport struct {
	Clusters int
	Probed   int
	Updated  int
}

// Reconcile is the health pass of section 6, run by the reconciler: for every
// active or degraded cluster, at most once per probe interval, ask /readyz and
// the nodes. A cluster is degraded after failing unhealthyPasses passes in a
// row, and active again after one healthy pass. A stopped or missing VM is the
// instance reconciler's to report.
func (s *Service) Reconcile(ctx context.Context) (HealthReport, error) {
	var report HealthReport
	for _, state := range []State{StateActive, StateDegraded} {
		rows, err := s.store.ListK8sClustersInState(ctx, sqlc.ListK8sClustersInStateParams{State: string(state)})
		if err != nil {
			return report, fmt.Errorf("list %s clusters: %w", state, err)
		}
		for _, row := range rows {
			report.Clusters++
			if !s.health.Due(row.ID) {
				continue
			}
			report.Probed++
			updated, err := s.check(ctx, row)
			if err != nil {
				s.logger.WarnContext(ctx, "kubernetes: health check could not run", "cluster_id", row.ID, "error", err)
				continue
			}
			if updated {
				report.Updated++
			}
		}
	}
	return report, nil
}

// check probes one cluster and moves its state if it should.
func (s *Service) check(ctx context.Context, row sqlc.K8sCluster) (bool, error) {
	l, err := s.layout(ctx, row)
	if err != nil {
		return false, err
	}
	pki, err := k8spki.LoadDir(s.pkiDir(row.ID))
	if err != nil {
		return false, err
	}
	api, err := s.api(s.connection(l, pki))
	if err != nil {
		return false, err
	}
	defer api.Close()

	reason := ""
	if err := api.Ready(ctx); err != nil {
		reason = "the API server is not ready: " + err.Error()
	} else if missing := s.unreadyNodes(ctx, row.ID, api, l.nodeNames()); len(missing) > 0 {
		reason = "nodes not Ready: " + strings.Join(missing, ", ")
	}

	failures := s.health.Record(row.ID, reason == "")
	switch managedcluster.Judge(State(row.State) == StateDegraded, reason, row.StateReason.String, failures, unhealthyPasses) {
	case managedcluster.Recover:
		s.setState(ctx, row.ID, StateActive, "")
		return true, nil
	case managedcluster.Degrade, managedcluster.Refresh:
		// Refresh: still degraded, for a reason that changed, kept current so
		// the console says what is wrong now rather than at the create.
		s.setState(ctx, row.ID, StateDegraded, reason)
		return true, nil
	case managedcluster.Keep:
	}
	return false, nil
}

// FakeAPI is the ClusterAPI of a node on the fake hypervisor, where no guest
// runs and so no API server ever answers: every expected node is Ready, and
// every call succeeds. It is what makes the module usable on
// KUMO_HYPERVISOR=fake, the way the databases module gets a probe that answers
// there (internal/cli/serve.go).
func FakeAPI(conn Connection) (ClusterAPI, error) {
	return &fakeAPI{nodes: conn.Nodes}, nil
}

type fakeAPI struct{ nodes []string }

func (f *fakeAPI) Ready(context.Context) error { return nil }
func (f *fakeAPI) Nodes(context.Context) ([]k8sapi.Node, error) {
	out := make([]k8sapi.Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		out = append(out, k8sapi.Node{Name: n, Ready: true})
	}
	return out, nil
}
func (f *fakeAPI) CreateBootstrapToken(context.Context, k8spki.BootstrapToken, time.Time) error {
	return nil
}
func (f *fakeAPI) Cordon(context.Context, string) error                     { return nil }
func (f *fakeAPI) PodsOnNode(context.Context, string) ([]k8sapi.Pod, error) { return nil, nil }
func (f *fakeAPI) Evict(context.Context, string, string) error              { return nil }
func (f *fakeAPI) DeleteNode(context.Context, string) error                 { return nil }
func (f *fakeAPI) Close()                                                   {}
