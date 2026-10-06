package vpc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// Handler returns the worker handlers for the VPC workflows, keyed by task
// type. The worker registry is built from this, so a missing type fails at
// startup rather than at the first create or delete.
func (s *Service) Handler() map[string]worker.Handler {
	return map[string]worker.Handler{
		task.TypeVPCCreate: worker.HandlerFunc(s.CreateHandler),
		task.TypeVPCDelete: worker.HandlerFunc(s.DeleteHandler),
	}
}

// CreateHandler defines and starts the libvirt network of a created VPC. It
// runs in the worker, never in an HTTP handler (AGENTS.md architecture rule 4).
//
// The steps, from docs/design/04-libvirt-integration.md §6:
//
//  1. the network is read back first; a task that already ran adopts what it
//     left behind instead of defining a second network;
//  2. a network that does not exist yet is defined from the VPC row;
//  3. the definition is started; a start that says the network is already
//     active is the same as having won the race.
func (s *Service) CreateHandler(ctx context.Context, t worker.Task) error {
	var payload createPayload
	if err := t.Decode(&payload); err != nil {
		return err
	}
	if payload.VPCID == "" {
		return fmt.Errorf("%w: create task %s names no VPC", ErrBadTaskPayload, t.ID)
	}

	// Two tasks for one VPC have to run in some order anyway, so the worker
	// waits instead of failing the one that loses (docs/design/04 §6.7).
	release, err := s.locks.Acquire(ctx, LockKey(payload.VPCID), t.ID)
	if err != nil {
		return err
	}
	defer release()

	row, err := s.store.GetVPC(ctx, sqlc.GetVPCParams{ID: payload.VPCID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Deleted before the task ran: the network was never defined, so
			// there is nothing to clean up.
			s.logger.InfoContext(ctx, "vpc: create skipped, the VPC is gone",
				"vpc_id", payload.VPCID, "task_id", t.ID)
			return nil
		}
		return fmt.Errorf("get VPC %q: %w", payload.VPCID, err)
	}

	info, err := s.hv.GetNetwork(ctx, row.LibvirtNetwork)
	switch {
	case err == nil && info.Active:
		// A requeued create that already started its network adopts it. The
		// row stays the single source of truth; nothing here re-writes it.
		s.logger.InfoContext(ctx, "vpc: network already active",
			"vpc_id", row.ID, "task_id", t.ID)
		return nil
	case err == nil:
		// Defined but not active: an earlier attempt was interrupted between
		// the define and the start.
		if err := s.hv.StartNetwork(ctx, row.LibvirtNetwork); err != nil &&
			!errors.Is(err, hypervisor.ErrInvalidState) {
			return fmt.Errorf("start the network %q: %w", row.LibvirtNetwork, err)
		}
		return nil
	case errors.Is(err, hypervisor.ErrNotFound):
		if err := s.hv.DefineNetwork(ctx, networkSpec(row)); err != nil &&
			!errors.Is(err, hypervisor.ErrAlreadyExists) {
			return fmt.Errorf("define the network %q: %w", row.LibvirtNetwork, err)
		}
		if err := s.hv.StartNetwork(ctx, row.LibvirtNetwork); err != nil {
			if errors.Is(err, hypervisor.ErrInvalidState) {
				// Somebody else brought the definition up between the define
				// and our start; that is the state this task wanted.
				return nil
			}
			// Best effort: leave a clean slate for the retry.
			if undefErr := s.hv.UndefineNetwork(ctx, row.LibvirtNetwork); undefErr != nil {
				s.logger.WarnContext(ctx, "vpc: rolling back the network definition failed",
					"vpc_id", row.ID, "task_id", t.ID, "error", undefErr)
			}
			return fmt.Errorf("start the network %q: %w", row.LibvirtNetwork, err)
		}
		return nil
	default:
		return fmt.Errorf("get the network %q: %w", row.LibvirtNetwork, err)
	}
}

// DeleteHandler destroys, undefines and removes the row of a deleted VPC. It
// runs in the worker and mirrors the image delete pattern: the network goes
// first, so a failure never leaves a bridge with no row to explain it, and the
// counts are checked here again because a subnet (or an instance inside it)
// created after the delete was queued would otherwise lose its network.
func (s *Service) DeleteHandler(ctx context.Context, t worker.Task) error {
	var payload deletePayload
	if err := t.Decode(&payload); err != nil {
		return err
	}
	if payload.VPCID == "" {
		return fmt.Errorf("%w: delete task %s names no VPC", ErrBadTaskPayload, t.ID)
	}

	release, err := s.locks.Acquire(ctx, LockKey(payload.VPCID), t.ID)
	if err != nil {
		return err
	}
	defer release()

	row, err := s.store.GetVPC(ctx, sqlc.GetVPCParams{ID: payload.VPCID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Already gone: the network was destroyed with it.
			s.logger.InfoContext(ctx, "vpc: delete skipped, the VPC is gone",
				"vpc_id", payload.VPCID, "task_id", t.ID)
			return nil
		}
		return fmt.Errorf("get VPC %q: %w", payload.VPCID, err)
	}

	subnets, err := s.store.CountVPCSubnets(ctx, sqlc.CountVPCSubnetsParams{VpcID: row.ID})
	if err != nil {
		return fmt.Errorf("count the subnets of VPC %q: %w", row.ID, err)
	}
	if subnets > 0 {
		return fmt.Errorf("%w: VPC %s still has %d subnet(s) while its deletion was queued",
			service.ErrConflict, row.Name, subnets)
	}

	instances, err := s.store.CountVPCInstances(ctx, sqlc.CountVPCInstancesParams{VpcID: row.ID})
	if err != nil {
		return fmt.Errorf("count the instances of VPC %q: %w", row.ID, err)
	}
	if instances > 0 {
		return fmt.Errorf("%w: VPC %s still has %d instance(s) allocated from its subnets while its deletion was queued",
			service.ErrConflict, row.Name, instances)
	}

	// A network that was never created, or a task that already stopped one
	// that was active, are not failures: the desired end state holds.
	if err := s.hv.DestroyNetwork(ctx, row.LibvirtNetwork); err != nil &&
		!errors.Is(err, hypervisor.ErrNotFound) && !errors.Is(err, hypervisor.ErrInvalidState) {
		return fmt.Errorf("destroy the network %q: %w", row.LibvirtNetwork, err)
	}
	if err := s.hv.UndefineNetwork(ctx, row.LibvirtNetwork); err != nil &&
		!errors.Is(err, hypervisor.ErrNotFound) && !errors.Is(err, hypervisor.ErrInvalidState) {
		return fmt.Errorf("undefine the network %q: %w", row.LibvirtNetwork, err)
	}

	// The tags table has no foreign key off vpcs, so the tags are removed
	// explicitly in the same transaction as the row (AGENTS.md — never rely on
	// a cascade that does not exist).
	err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		if _, err := tx.DeleteVPC(ctx, sqlc.DeleteVPCParams{ID: row.ID}); err != nil {
			return fmt.Errorf("delete VPC %q: %w", row.ID, err)
		}
		if err := tx.DeleteResourceTags(ctx, sqlc.DeleteResourceTagsParams{ResourceID: row.ID}); err != nil {
			return fmt.Errorf("delete the tags of VPC %q: %w", row.ID, err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "vpc: deleted", "vpc_id", row.ID, "task_id", t.ID)
	return nil
}
