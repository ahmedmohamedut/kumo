package spark

import (
	"context"
	"fmt"
	"strings"

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
// active or degraded cluster, at most once per probe interval, read the
// master's status. A cluster is degraded after failing unhealthyPasses passes
// in a row and active again after one healthy pass. A stopped or missing VM is
// the instance reconciler's to report.
func (s *Service) Reconcile(ctx context.Context) (HealthReport, error) {
	var report HealthReport
	for _, state := range []State{StateActive, StateDegraded} {
		rows, err := s.store.ListSparkClustersInState(ctx, sqlc.ListSparkClustersInStateParams{State: string(state)})
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
				s.logger.WarnContext(ctx, "spark: health check could not run", "cluster_id", row.ID, "error", err)
				continue
			}
			if updated {
				report.Updated++
			}
		}
	}
	return report, nil
}

// check reads one cluster's master and moves its state if it should.
func (s *Service) check(ctx context.Context, row sqlc.SparkCluster) (bool, error) {
	l, err := s.layout(ctx, row)
	if err != nil {
		return false, err
	}
	api, err := s.api(s.connection(l))
	if err != nil {
		return false, err
	}
	defer api.Close()

	reason := ""
	if st, err := api.Status(ctx); err != nil {
		reason = "the master does not answer: " + err.Error()
	} else if !st.Alive() {
		s.health.RecordNodes(row.ID, aliveByName(l, st))
		reason = "the master is " + st.Status
	} else if missing := s.unregistered(ctx, l, api); len(missing) > 0 {
		reason = "workers not registered: " + strings.Join(missing, ", ")
	}

	failures := s.health.Record(row.ID, reason == "")
	switch managedcluster.Judge(State(row.State) == StateDegraded, reason, row.StateReason.String, failures, unhealthyPasses) {
	case managedcluster.Recover:
		s.setState(ctx, row.ID, StateActive, "")
		return true, nil
	case managedcluster.Degrade, managedcluster.Refresh:
		s.setState(ctx, row.ID, StateDegraded, reason)
		return true, nil
	case managedcluster.Keep:
	}
	return false, nil
}
