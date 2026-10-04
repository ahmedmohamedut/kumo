// Package instancetype owns the instance type presets: the sizes an instance
// can be launched at. They are read-only in v1 (docs/design/01-api.md
// §Instance types), so this package only seeds and reads them.
package instancetype

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// ErrNoStore means Options.Store was nil.
var ErrNoStore = errors.New("instancetype: a store is required")

// Type is a preset size.
type Type struct {
	ID          string `json:"id"`
	VCPUs       int    `json:"vcpus"`
	MemoryMiB   int    `json:"memory_mib"`
	Description string `json:"description"`
}

// Service reads and seeds the presets.
type Service struct {
	store *store.Store
}

// Options configures the service.
type Options struct {
	Store *store.Store
}

// New builds the service.
func New(opts Options) (*Service, error) {
	if opts.Store == nil {
		return nil, ErrNoStore
	}
	return &Service{store: opts.Store}, nil
}

// Seed makes sure every preset of docs/design/01-api.md exists. It is
// INSERT OR IGNORE, so a row an operator edited by hand survives a restart and
// a fresh database gets the documented set.
func (s *Service) Seed(ctx context.Context) error {
	if err := s.store.SeedInstanceTypes(ctx); err != nil {
		return fmt.Errorf("instancetype: seed the presets: %w", err)
	}
	return nil
}

// List returns the presets, smallest first.
func (s *Service) List(ctx context.Context) ([]Type, error) {
	rows, err := s.store.ListInstanceTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("instancetype: list the presets: %w", err)
	}

	types := make([]Type, 0, len(rows))
	for _, row := range rows {
		types = append(types, fromRow(row))
	}
	return types, nil
}

// Get returns one preset, or service.ErrNotFound when no such preset exists.
// The launch request validates against it, so an unknown type is a 404 in the
// service and an invalid_argument on the wire.
func (s *Service) Get(ctx context.Context, id string) (Type, error) {
	row, err := s.store.GetInstanceType(ctx, sqlc.GetInstanceTypeParams{ID: id})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Type{}, fmt.Errorf("%w: no instance type %q", service.ErrNotFound, id)
		}
		return Type{}, fmt.Errorf("instancetype: read %q: %w", id, err)
	}
	return fromRow(row), nil
}

// fromRow converts the generated row.
func fromRow(row sqlc.InstanceType) Type {
	return Type{
		ID:          row.ID,
		VCPUs:       int(row.Vcpus),
		MemoryMiB:   int(row.MemoryMib),
		Description: row.Description,
	}
}
