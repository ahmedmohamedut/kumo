// Package system answers questions about the control plane itself: which
// build is running, whether it still needs first-run setup, and whether the
// hypervisor is reachable.
package system

import (
	"context"
	"log/slog"
)

// UserCounter reports how many user accounts exist. Zero means the instance
// has never been set up.
type UserCounter interface {
	CountUsers(ctx context.Context) (int64, error)
}

// HypervisorChecker reports the hypervisor backend and whether it answers.
type HypervisorChecker interface {
	Ping(ctx context.Context) error
}

// Info is the payload of GET /api/v1/system/info.
type Info struct {
	Version    string         `json:"version"`
	NeedsSetup bool           `json:"needs_setup"`
	Hypervisor HypervisorInfo `json:"hypervisor"`
}

// HypervisorInfo describes the hypervisor backend.
type HypervisorInfo struct {
	Type string `json:"type"`
	OK   bool   `json:"ok"`
}

// Service implements Info.
type Service struct {
	logger        *slog.Logger
	version       string
	hypervisor    string
	users         UserCounter
	hypervisorPin HypervisorChecker
}

// New builds the service. users and hypervisorPin may be nil, which happens
// only in the M0 skeleton before the store and the hypervisor exist; the
// service then reports needs_setup=false and a hypervisor that is not ok.
func New(logger *slog.Logger, version, hypervisor string, users UserCounter, hypervisorPin HypervisorChecker) *Service {
	return &Service{
		logger:        logger,
		version:       version,
		hypervisor:    hypervisor,
		users:         users,
		hypervisorPin: hypervisorPin,
	}
}

// Info collects the current control plane status.
func (s *Service) Info(ctx context.Context) (Info, error) {
	info := Info{
		Version:    s.version,
		NeedsSetup: false,
		Hypervisor: HypervisorInfo{Type: s.hypervisor, OK: false},
	}

	if s.users != nil {
		count, err := s.users.CountUsers(ctx)
		if err != nil {
			return Info{}, err
		}
		info.NeedsSetup = count == 0
	}

	if s.hypervisorPin != nil {
		if err := s.hypervisorPin.Ping(ctx); err != nil {
			s.logger.WarnContext(ctx, "hypervisor is not reachable", "hypervisor", s.hypervisor, "error", err)
		} else {
			info.Hypervisor.OK = true
		}
	}

	return info, nil
}
