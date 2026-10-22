package instance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Reconciliation is the state Kumo brings the database to against libvirt, the
// safety net of docs/design/00-architecture.md §4: libvirt owns the runtime
// state, SQLite owns the metadata, and a pass makes the first visible in the
// second.
//
// A pass never destroys anything. It writes states and addresses onto rows that
// already exist and reports a domain it cannot account for, because the one
// thing worse than a stale row is a reconciler that deletes a guest an operator
// is using (§4.3: orphans are reported, never removed).

// reconcilerName is the lock holder a pass identifies itself as, so a `409`
// raised against an operator says a reconcile is in progress rather than naming
// a task type that does not exist.
const reconcilerName = "reconciler"

// crashReason is the state_reason of an instance whose guest libvirt reports as
// crashed. Like domainMissing it is a prefix an operator can grep for.
const crashReason = "domain_crashed: "

// Orphan is a domain that carries an instance id libvirt runs but no row
// claims: a guest left behind by a crash between the define and the insert, or
// one an operator built by hand under a Kumo-shaped name.
//
// FirstSeen is kept across passes so an operator can tell a leftover that has
// been there for a week from one that appeared during the launch they are
// watching.
type Orphan struct {
	// DomainName is the libvirt domain name, which for a Kumo domain is the
	// instance id it was launched under.
	DomainName string `json:"domain_name"`
	// UUID is libvirt's own identifier for the domain.
	UUID string `json:"uuid"`
	// State is the domain's runtime state, mapped onto the instance vocabulary
	// so the console renders it with the indicator it uses everywhere else.
	State State `json:"state"`
	// FirstSeen is when a pass first reported this domain.
	FirstSeen time.Time `json:"first_seen"`
	// LastSeen is the most recent pass that still found it.
	LastSeen time.Time `json:"last_seen"`
}

// Reconciliation counts what one pass saw and changed. It is the log line of a
// pass, not a resource: nothing reads it back.
type Reconciliation struct {
	// Rows is how many instances the database held.
	Rows int
	// Domains is how many Kumo-shaped domains libvirt reported.
	Domains int
	// Updated is how many rows the pass wrote.
	Updated int
	// Skipped is how many rows the pass left to a task that owns them.
	Skipped int
	// Orphans is how many domains no row claimed.
	Orphans int
}

// orphanState holds the orphans of the most recent pass. It lives in memory on
// purpose: an orphan is an observation of the hypervisor, not metadata Kumo
// owns, so it has no row and cannot go stale across a restart.
type orphanState struct {
	mu   sync.RWMutex
	seen map[string]Orphan
}

// Orphans returns the domains the last pass could not account for, oldest
// sighting first. It answers GET /system/orphans.
func (s *Service) Orphans() []Orphan {
	s.orphans.mu.RLock()
	defer s.orphans.mu.RUnlock()

	out := make([]Orphan, 0, len(s.orphans.seen))
	for _, o := range s.orphans.seen {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].DomainName < out[j].DomainName
		}
		return out[i].FirstSeen.Before(out[j].FirstSeen)
	})
	return out
}

// Reconcile runs one full pass: it reads every instance and every domain, writes
// the observed state onto the rows a task does not own, and replaces the orphan
// report.
//
// One ListDomains call backs the whole pass, so a guest that changes state while
// the pass runs is simply picked up by the next one. That is what makes the pass
// cheap enough to run every ten seconds and why the event hints of §7 exist: the
// loop is the safety net, not the mechanism.
func (s *Service) Reconcile(ctx context.Context) (Reconciliation, error) {
	rows, err := s.store.ListInstancesForReconcile(ctx)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("reconcile: list the instances: %w", err)
	}

	domains, err := s.hv.ListDomains(ctx)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("reconcile: list the domains: %w", err)
	}

	byID := make(map[string]hypervisor.DomainInfo, len(domains))
	for _, d := range domains {
		// A domain is matched by its libvirt name, which is the instance id it
		// was launched under (docs/design/04-libvirt-integration.md §2). That is
		// DomainInfo.ID: DomainInfo.Name is the name an operator gave the
		// instance, which is not unique over time and is not what libvirt knows
		// the domain as. Anything that is not a Kumo id belongs to somebody else
		// and is not Kumo's to report on.
		if !id.HasResourceShape(id.PrefixInstance, d.ID) {
			continue
		}
		byID[d.ID] = d
	}

	report := Reconciliation{Rows: len(rows), Domains: len(byID)}
	claimed := make(map[string]struct{}, len(rows))

	for _, row := range rows {
		claimed[row.ID] = struct{}{}

		info, found := byID[row.ID]
		changed, skipped := s.reconcileRow(ctx, row, info, found)
		switch {
		case skipped:
			report.Skipped++
		case changed:
			report.Updated++
		}
	}

	report.Orphans = s.recordOrphans(ctx, byID, claimed)
	return report, nil
}

// ReconcileInstance brings one instance in line, which is what a lifecycle event
// asks for. An event names a domain, so there is no reason to read the whole
// table to act on it; the periodic pass still covers everything the stream drops
// (docs/design/04-libvirt-integration.md §7).
//
// An id with no row is not an error: an event for an instance that was just
// terminated is the normal end of a terminate.
func (s *Service) ReconcileInstance(ctx context.Context, instanceID string) error {
	row, err := s.store.GetInstance(ctx, sqlc.GetInstanceParams{ID: instanceID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("reconcile instance %q: %w", instanceID, err)
	}

	info, err := s.hv.GetDomain(ctx, instanceID)
	switch {
	case err == nil:
		s.reconcileRow(ctx, row, info, true)
	case errors.Is(err, hypervisor.ErrNotFound):
		s.reconcileRow(ctx, row, hypervisor.DomainInfo{}, false)
	default:
		return fmt.Errorf("reconcile instance %q: get the domain: %w", instanceID, err)
	}
	return nil
}

// reconcileRow applies the observation of one domain to one row.
//
// skipped tells the caller the row was left alone because somebody else owns it:
// a transitional state belongs to the task that wrote it (§4), and a held lock
// means that task is running right now. Neither is a problem to report — it is
// the pass staying out of the way.
func (s *Service) reconcileRow(
	ctx context.Context, row sqlc.Instance, info hypervisor.DomainInfo, found bool,
) (changed, skipped bool) {
	switch State(row.State) {
	case StatePending, StateTerminating:
		// Owned by the launch or the terminate that wrote the state. A launch
		// has not defined its domain yet, and a terminate is removing one, so
		// "no domain" is the expected reading rather than a fault to record.
		return false, true
	case StateTerminated:
		// A row an earlier release left as history. It describes nothing that
		// runs, and only a DELETE purges it (docs/design/02-data-model.md).
		return false, true
	}

	// A task that is running holds the lock for as long as it works, so failing
	// to take it is the signal that the row is being written by somebody with
	// more context than a pass has. The pass never waits: the next one is ten
	// seconds away.
	release, err := s.locks.TryAcquire(LockKey(row.ID), reconcilerName)
	if err != nil {
		return false, true
	}
	defer release()

	// The row may have been written between the list and the lock, so it is read
	// again under the lock before anything is decided from it.
	fresh, err := s.store.GetInstance(ctx, sqlc.GetInstanceParams{ID: row.ID})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.logger.ErrorContext(ctx, "reconcile: could not re-read the instance",
				"instance_id", row.ID, "error", err)
		}
		return false, true
	}
	switch State(fresh.State) {
	case StatePending, StateTerminating, StateTerminated:
		return false, true
	}
	row = fresh

	if !found {
		return s.reconcileMissingDomain(ctx, row), false
	}
	return s.reconcileObserved(ctx, row, info), false
}

// reconcileMissingDomain records an instance whose domain libvirt does not know.
//
// This is rule 2 of §4: a row that is not transitional and has no domain can
// never be acted on again, so it moves to error with the domain_missing reason
// the lifecycle workflows already write for the same discovery. Writing it twice
// would churn updated_at on every pass, so a row that already says so is left
// alone.
func (s *Service) reconcileMissingDomain(ctx context.Context, row sqlc.Instance) bool {
	reason := domainMissing + "the domain of instance " + row.ID + " is gone"
	if State(row.State) == StateError && row.StateReason.String == reason {
		return false
	}

	s.logger.WarnContext(ctx, "reconcile: the domain of an instance is gone",
		"instance_id", row.ID, "was_state", row.State)
	s.recordFailure(ctx, row.ID, StateError, reason)
	return true
}

// reconcileObserved writes the state libvirt reports onto a row that disagrees.
func (s *Service) reconcileObserved(
	ctx context.Context, row sqlc.Instance, info hypervisor.DomainInfo,
) bool {
	observed, ok := observedState(info.State)
	if !ok {
		// A paused domain is neither running nor shut off, and Kumo offers no
		// pause: inventing a state for it would put a value in the row that no
		// action could move. The sighting is logged and the row left as it is.
		s.logger.DebugContext(ctx, "reconcile: a domain is in a state Kumo does not model",
			"instance_id", row.ID, "domain_state", info.State)
		return false
	}

	// An instance in error keeps its reason unless the guest is demonstrably up.
	// A failed launch often leaves a domain defined but never started, and
	// reading that as a clean "stopped" would turn a broken instance into one
	// the console invites an operator to start.
	if State(row.State) == StateError && observed != StateRunning {
		return false
	}

	changed := false
	if State(row.State) != observed {
		s.logger.InfoContext(ctx, "reconcile: an instance changed state out of band",
			"instance_id", row.ID, "was_state", row.State, "state", observed,
			"domain_state", info.State)

		switch observed {
		case StateRunning:
			if err := s.markRunning(ctx, row.ID, info); err != nil {
				s.logger.ErrorContext(ctx, "reconcile: could not record the running state",
					"instance_id", row.ID, "error", err)
				return false
			}
		case StateError:
			s.recordFailure(ctx, row.ID, StateError,
				crashReason+"the guest of instance "+row.ID+" crashed")
		default:
			if err := s.markStopped(ctx, row.ID); err != nil {
				s.logger.ErrorContext(ctx, "reconcile: could not record the stopped state",
					"instance_id", row.ID, "error", err)
				return false
			}
		}
		changed = true
	}

	// Rule 4 of §4: the address is refreshed from the lease even when the state
	// already agreed, because a guest that renews into a different address is
	// still running. An empty reading never overwrites one the row has, for the
	// reason markRunning records.
	observedIP := s.observedAddress(ctx, row.ID, info)
	if observed == StateRunning && observedIP != "" && observedIP != row.PrivateIp.String {
		if _, err := s.store.SetInstanceAddress(ctx, sqlc.SetInstanceAddressParams{
			ID:        row.ID,
			PrivateIp: sql.NullString{String: observedIP, Valid: true},
			UpdatedAt: formatTime(s.now()),
		}); err != nil {
			s.logger.ErrorContext(ctx, "reconcile: could not record the address",
				"instance_id", row.ID, "error", err)
			return changed
		}
		s.logger.InfoContext(ctx, "reconcile: an instance address changed",
			"instance_id", row.ID, "was_ip", row.PrivateIp.String, "ip", observedIP)
		changed = true
	}

	return changed
}

// recordOrphans replaces the orphan report with the domains no row claimed and
// returns how many there are.
//
// Rule 3 of §4 is report, never remove: a domain under a Kumo-shaped name that
// the database does not know may be a guest somebody is relying on, and v1 has
// no way to tell that from a leftover. An operator decides.
func (s *Service) recordOrphans(
	ctx context.Context, domains map[string]hypervisor.DomainInfo, claimed map[string]struct{},
) int {
	now := s.now()

	s.orphans.mu.Lock()
	defer s.orphans.mu.Unlock()

	next := make(map[string]Orphan, len(domains))
	for name, info := range domains {
		if _, ok := claimed[name]; ok {
			continue
		}

		state, mapped := observedState(info.State)
		if !mapped {
			state = State(info.State)
		}

		orphan := Orphan{
			DomainName: name,
			UUID:       info.UUID,
			State:      state,
			FirstSeen:  now,
			LastSeen:   now,
		}
		if previous, seen := s.orphans.seen[name]; seen {
			orphan.FirstSeen = previous.FirstSeen
		} else {
			s.logger.WarnContext(ctx, "reconcile: a domain has no instance",
				"domain_name", name, "uuid", info.UUID, "domain_state", info.State)
		}
		next[name] = orphan
	}

	s.orphans.seen = next
	return len(next)
}

// observedAddress is the address the hypervisor has for a domain.
//
// A listing does not have to carry one: the libvirt backend resolves a lease per
// domain, so ListDomains reports state without an address and only GetDomain
// looks the address up. Asking for it here keeps rule 4 working on both backends
// and costs one call per running guest per pass, which is what the fake avoids
// by filling the address in on the listing.
func (s *Service) observedAddress(
	ctx context.Context, instanceID string, info hypervisor.DomainInfo,
) string {
	if info.IP != "" {
		return info.IP
	}
	if info.State != hypervisor.DomainStateRunning {
		return ""
	}

	full, err := s.hv.GetDomain(ctx, instanceID)
	if err != nil {
		// Not worth failing the pass: the state has already been written, and
		// the address is refreshed on the next tick.
		s.logger.DebugContext(ctx, "reconcile: could not read the address of a domain",
			"instance_id", instanceID, "error", err)
		return ""
	}
	return full.IP
}

// observedState maps a domain state onto the instance state it implies. ok is
// false for a state Kumo's state machine has no value for, which the caller
// reports rather than forces into one (docs/design/00-architecture.md §4).
func observedState(state hypervisor.DomainState) (State, bool) {
	switch state {
	case hypervisor.DomainStateRunning:
		return StateRunning, true
	case hypervisor.DomainStateShutoff:
		return StateStopped, true
	case hypervisor.DomainStateCrashed:
		return StateError, true
	case hypervisor.DomainStatePaused:
		return "", false
	default:
		return "", false
	}
}
