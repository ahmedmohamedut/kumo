package vpc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/network"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/subnet"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// errDefaultTaken marks the unique-constraint race in Bootstrap: another
// process wrote the default row between the read and the write. It is handled
// by re-reading the winner's row.
var errDefaultTaken = errors.New("vpc: the default VPC was created by another process")

// ErrDefaultSkipped means Bootstrap did not create a default VPC because its
// range is taken on this host. It is not a failure: the node runs without a
// default VPC, and the first VPC an operator creates becomes the default
// (Create). The caller logs it and carries on.
var ErrDefaultSkipped = errors.New("vpc: no default VPC was created")

// defaultVPCName is the row name of a fresh default VPC and of its default
// subnet: the label the console shows and, for the VPC, the DNS domain.
const defaultVPCName = "default"

// Bootstrap makes sure the default VPC exists and returns it. It runs once per
// boot, before the worker starts, because every other workflow needs a VPC to
// attach a guest to (docs/design/00-architecture.md §7, step 3).
//
// The order matters:
//
//  1. a default VPC row that already exists wins — its row is the source of
//     truth, whatever libvirt currently holds;
//  2. otherwise the libvirt network named in the config is adopted if it
//     exists (an operator's `kumo-default`, whose bridge and subnet must
//     survive Kumo's first run);
//  3. otherwise a fresh default VPC is created on the documented guest range
//     and its network is defined and started, with the host-collision check
//     of a normal create;
//  4. the default subnet is created last, so a failure in any earlier step
//     leaves no half-default behind.
func (s *Service) Bootstrap(ctx context.Context, networkName string) (VPC, error) {
	row, err := s.store.GetDefaultVPC(ctx)
	switch {
	case err == nil:
		tags, err := s.tagsByResource(ctx, []string{row.ID})
		if err != nil {
			return VPC{}, err
		}
		return vpcFromRow(row, tags[row.ID]), nil
	case !errors.Is(err, sql.ErrNoRows):
		return VPC{}, fmt.Errorf("get the default VPC: %w", err)
	}

	info, err := s.hv.GetNetwork(ctx, networkName)
	switch {
	case err == nil:
		return s.adoptDefault(ctx, networkName, info)
	case errors.Is(err, hypervisor.ErrNotFound):
		return s.createDefault(ctx)
	default:
		return VPC{}, fmt.Errorf("look for the default network %q: %w", networkName, err)
	}
}

// adoptDefault records an existing libvirt network as the default VPC. The
// network, the bridge and the subnet already live on the host; the row keeps
// those names and the gateway dnsmasq is already serving, so nothing moves.
// The subnet is not re-created: the adopted network may already allocate from
// its own ranges, which Kumo must not guess at.
func (s *Service) adoptDefault(ctx context.Context, name string, info hypervisor.NetworkInfo) (VPC, error) {
	prefix, err := netip.ParsePrefix(info.CIDR)
	if err != nil {
		return VPC{}, fmt.Errorf("the adopted network %q has an unparsable CIDR %q: %w",
			name, info.CIDR, err)
	}

	now := formatTime(s.now())
	var row sqlc.Vpc
	err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		row, err = tx.CreateVPC(ctx, sqlc.CreateVPCParams{
			ID:             id.VPC(),
			Name:           adoptedName(info.Domain),
			Cidr:           prefix.String(),
			Mode:           string(info.Mode),
			GatewayIp:      info.Gateway,
			Netmask:        network.Netmask(prefix),
			BridgeName:     info.Bridge,
			LibvirtNetwork: info.Name,
			IsDefault:      1,
			CreatedAt:      now,
			UpdatedAt:      now,
		})
		if err != nil {
			if isUniqueViolation(err) {
				// Two processes adopted the same network; the row is UNIQUE on
				// libvirt_network, so the first write wins.
				return errDefaultTaken
			}
			return fmt.Errorf("adopt the network %q as the default VPC: %w", name, err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errDefaultTaken) {
			return s.reloadDefault(ctx)
		}
		return VPC{}, err
	}

	s.logger.InfoContext(ctx, "vpc: adopted the default network",
		"vpc_id", row.ID, "network", row.LibvirtNetwork, "bridge", row.BridgeName)

	// An adopted network that is not active leaves the bridge down for every
	// launch, so it is started here. It is never redefined: the row describes
	// the network, never the other way around, and a define would drop the
	// dhcp hosts the network already serves.
	if !info.Active {
		if err := s.hv.StartNetwork(ctx, info.Name); err != nil && !errors.Is(err, hypervisor.ErrInvalidState) {
			rollbackDefault(ctx, s, row)
			return VPC{}, fmt.Errorf("start the adopted network %q: %w", info.Name, err)
		}
	}
	return vpcFromRow(row, map[string]string{}), nil
}

// createDefault creates a fresh default VPC and brings its network up. The
// CIDR, the mode and the bridge follow docs/design/04 §6: the documented guest
// range, NAT, and kmb plus the first 8 hex characters of the VPC id.
func (s *Service) createDefault(ctx context.Context) (VPC, error) {
	prefix, err := netip.ParsePrefix(s.defaultVPCCIDR)
	if err != nil {
		return VPC{}, fmt.Errorf("the default VPC CIDR %q does not parse: %w", s.defaultVPCCIDR, err)
	}

	collision, err := hostCollision(prefix)
	if err != nil {
		return VPC{}, fmt.Errorf("scan the host routes for overlapping networks: %w", err)
	}
	if collision != "" {
		// The operator's LAN owns this block. Nothing is half-created: the node
		// starts without a default VPC and the operator makes one in the console
		// on a range that suits their network (or sets KUMO_DEFAULT_VPC_CIDR).
		return VPC{}, fmt.Errorf("%w: the default VPC CIDR %s overlaps %s; create a VPC in the console, or set KUMO_DEFAULT_VPC_CIDR to a free RFC 1918 range (e.g. 172.31.0.0/16)",
			ErrDefaultSkipped, prefix, collision)
	}

	now := formatTime(s.now())
	var row sqlc.Vpc
	err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		existing, err := tx.ListVPCCIDRs(ctx)
		if err != nil {
			return fmt.Errorf("list the existing VPC CIDRs: %w", err)
		}
		for _, other := range existing {
			otherPrefix, parseErr := netip.ParsePrefix(other.Cidr)
			if parseErr != nil {
				continue // rows this package wrote parse
			}
			if prefix.Overlaps(otherPrefix) {
				return fmt.Errorf("%w: the default VPC CIDR %s overlaps the VPC %s (%s)",
					service.ErrConflict, prefix, other.ID, other.Cidr)
			}
		}

		vpcID := id.VPC()
		row, err = tx.CreateVPC(ctx, sqlc.CreateVPCParams{
			ID:             vpcID,
			Name:           defaultVPCName,
			Cidr:           prefix.String(),
			Mode:           string(ModeNAT),
			GatewayIp:      network.Gateway(prefix).String(),
			Netmask:        network.Netmask(prefix),
			BridgeName:     hypervisor.BridgeName(vpcID),
			LibvirtNetwork: vpcID,
			IsDefault:      1,
			CreatedAt:      now,
			UpdatedAt:      now,
		})
		if err != nil {
			if isUniqueViolation(err) {
				// Two processes booted the same database at once; the first
				// default row wins and owns the network and the subnet.
				return errDefaultTaken
			}
			return fmt.Errorf("create the default VPC row: %w", err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errDefaultTaken) {
			return s.reloadDefault(ctx)
		}
		return VPC{}, err
	}
	s.logger.InfoContext(ctx, "vpc: creating the default VPC",
		"vpc_id", row.ID, "cidr", row.Cidr, "bridge", row.BridgeName)

	// Bring the network up now: the worker is not started yet, and instances
	// need the bridge before the first launch. The retry-tolerant sequence is
	// the same one the create task runs.
	if err := defineAndStart(ctx, s.hv, row); err != nil {
		rollbackDefault(ctx, s, row)
		return VPC{}, err
	}

	if err := s.createDefaultSubnet(ctx, row); err != nil {
		rollbackDefault(ctx, s, row)
		return VPC{}, err
	}
	return vpcFromRow(row, map[string]string{}), nil
}

// createDefaultSubnet partitions the first half of the default VPC for the
// instances every launch lands in. A VPC that is already at the narrowest
// allowed prefix has no room for a subnet, which is logged rather than fatal:
// the VPC still works, the console just cannot launch into it.
func (s *Service) createDefaultSubnet(ctx context.Context, row sqlc.Vpc) error {
	if s.subnets == nil {
		s.logger.WarnContext(ctx, "vpc: no subnet service wired; the default subnet was not created",
			"vpc_id", row.ID)
		return nil
	}

	prefix, err := netip.ParsePrefix(row.Cidr)
	if err != nil {
		return fmt.Errorf("parse the CIDR of the default VPC %q: %w", row.ID, err)
	}
	if prefix.Bits() >= network.MaxPrefixBits {
		s.logger.WarnContext(ctx, "vpc: the default VPC has no room for a default subnet",
			"vpc_id", row.ID, "cidr", row.Cidr)
		return nil
	}

	half := netip.PrefixFrom(prefix.Masked().Addr(), prefix.Bits()+1)
	if _, err := s.subnets.Create(ctx, subnet.CreateInput{
		Name:      defaultVPCName,
		VPCID:     row.ID,
		CIDR:      half.String(),
		IsDefault: true,
	}); err != nil {
		if errors.Is(err, service.ErrAlreadyExists) {
			// The process that won the bootstrap race already carved the
			// default subnet out of this VPC; the retry loop of a lost race
			// must not fail the boot.
			s.logger.InfoContext(ctx, "vpc: the default subnet already exists", "vpc_id", row.ID)
			return nil
		}
		return fmt.Errorf("create the default subnet: %w", err)
	}
	return nil
}

// reloadDefault re-reads the default VPC after losing the write race. The
// winner owns the rest of the bootstrap, so nothing else needs doing.
func (s *Service) reloadDefault(ctx context.Context) (VPC, error) {
	row, err := s.store.GetDefaultVPC(ctx)
	if err != nil {
		return VPC{}, fmt.Errorf("reload the default VPC after a bootstrap race: %w", err)
	}
	s.logger.WarnContext(ctx, "vpc: another process created the default VPC first",
		"vpc_id", row.ID)
	return vpcFromRow(row, map[string]string{}), nil
}

// adoptedName picks the row name of an adopted network. The network's own DNS
// domain is kept when it matches the VPC name rule, because it is a label an
// operator chose; a network without one gets the conventional name.
func adoptedName(domain string) string {
	if domain != "" && namePattern.MatchString(domain) {
		return domain
	}
	return defaultVPCName
}

// defineAndStart defines a VPC's network and brings it up, tolerating the
// states an earlier attempt (or the winner of a bootstrap race) left behind:
// an existing definition is adopted and an active network is already done.
func defineAndStart(ctx context.Context, hv hypervisor.Hypervisor, row sqlc.Vpc) error {
	spec := networkSpec(row)
	if err := hv.DefineNetwork(ctx, spec); err != nil && !errors.Is(err, hypervisor.ErrAlreadyExists) {
		return fmt.Errorf("define the network %q: %w", spec.Name, err)
	}
	if err := hv.StartNetwork(ctx, spec.Name); err != nil && !errors.Is(err, hypervisor.ErrInvalidState) {
		// Best effort: leave a clean slate for the retry.
		_ = hv.UndefineNetwork(ctx, spec.Name)
		return fmt.Errorf("start the network %q: %w", spec.Name, err)
	}
	return nil
}

// rollbackDefault is the best-effort tidy-up when a fresh default VPC fails
// after its row was committed and its network was defined: the row (and its
// tags) goes away so the next boot starts clean, and the network definition
// follows it if it still exists.
func rollbackDefault(ctx context.Context, s *Service, row sqlc.Vpc) {
	s.logger.WarnContext(ctx, "vpc: rolling back the default VPC after a bootstrap failure",
		"vpc_id", row.ID)

	_ = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		if _, err := tx.DeleteVPC(ctx, sqlc.DeleteVPCParams{ID: row.ID}); err != nil {
			return err
		}
		return tx.DeleteResourceTags(ctx, sqlc.DeleteResourceTagsParams{ResourceID: row.ID})
	})

	_ = s.hv.UndefineNetwork(ctx, row.LibvirtNetwork)
}
