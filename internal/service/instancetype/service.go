// Package instancetype owns the instance types: the sizes an instance can be
// launched at. Migration 0003 seeds the kumo.* presets once; from then on an
// administrator manages the set (docs/design/01-api.md §Instance types).
//
// An instance copies the vCPUs and memory of its type when it launches, so
// editing or deleting a type never changes an instance that already exists.
package instancetype

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Bounds of 01-api.md §Instance types. They keep a type launchable rather
// than measure it against this node: the capacity check of a launch is what
// compares a size with the host (00-architecture.md §6).
const (
	MinVCPUs          = 1
	MaxVCPUs          = 256
	MinMemoryMiB      = 256
	MaxMemoryMiB      = 1024 * 1024
	maxDescriptionLen = 255
)

// idPattern is the instance type id rule of 01-api.md: lowercase, starting
// with a letter or digit, like the kumo.small presets.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Sentinel errors of the constructor, so a caller can tell a wiring mistake
// from a runtime failure.
var (
	// ErrNoLogger means Options.Logger was nil.
	ErrNoLogger = errors.New("instancetype: a logger is required")
	// ErrNoStore means Options.Store was nil.
	ErrNoStore = errors.New("instancetype: a store is required")
)

// Type is an instance size.
type Type struct {
	ID          string `json:"id"`
	VCPUs       int    `json:"vcpus"`
	MemoryMiB   int    `json:"memory_mib"`
	Description string `json:"description"`
}

// Detail is a type plus the instances that reference it, which are what
// blocks a delete.
type Detail struct {
	Type
	UsedBy []string `json:"used_by"`
}

// CreateInput is a POST /instance-types body. An empty Description is
// generated from the size, the way the presets read.
type CreateInput struct {
	ID          string
	VCPUs       int
	MemoryMiB   int
	Description string
}

// UpdateInput is a PATCH /instance-types/{id} body. A nil field is left alone.
type UpdateInput struct {
	VCPUs       *int
	MemoryMiB   *int
	Description *string
}

// Options configures the service.
type Options struct {
	Logger *slog.Logger
	Store  *store.Store
}

// Service owns the instance types.
type Service struct {
	logger *slog.Logger
	store  *store.Store
}

// New builds the service.
func New(opts Options) (*Service, error) {
	if opts.Logger == nil {
		return nil, ErrNoLogger
	}
	if opts.Store == nil {
		return nil, ErrNoStore
	}
	return &Service{logger: opts.Logger, store: opts.Store}, nil
}

// List returns the types, smallest first.
func (s *Service) List(ctx context.Context) ([]Type, error) {
	rows, err := s.store.ListInstanceTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("instancetype: list the types: %w", err)
	}

	types := make([]Type, 0, len(rows))
	for _, row := range rows {
		types = append(types, fromRow(row))
	}
	return types, nil
}

// Get returns one type, or service.ErrNotFound when no such type exists.
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

// Detail returns one type with the instances that reference it.
func (s *Service) Detail(ctx context.Context, id string) (Detail, error) {
	t, err := s.Get(ctx, id)
	if err != nil {
		return Detail{}, err
	}

	usedBy, err := s.store.ListInstancesUsingInstanceType(ctx,
		sqlc.ListInstancesUsingInstanceTypeParams{InstanceType: t.ID})
	if err != nil {
		return Detail{}, fmt.Errorf("instancetype: list the instances using %q: %w", t.ID, err)
	}
	return Detail{Type: t, UsedBy: usedBy}, nil
}

// Create adds a type. A taken id maps to service.ErrAlreadyExists.
func (s *Service) Create(ctx context.Context, in CreateInput) (Type, error) {
	var fields []service.FieldError
	if !idPattern.MatchString(in.ID) {
		fields = append(fields, service.FieldError{
			Field:  "id",
			Reason: "must be 1-64 lowercase letters, digits, '.', '_' or '-', starting with a letter or digit",
		})
	}
	fields = append(fields, validateSize(in.VCPUs, in.MemoryMiB)...)
	if field := validateDescription(in.Description); field != nil {
		fields = append(fields, *field)
	}
	if len(fields) > 0 {
		return Type{}, &service.ValidationError{Message: "the instance type is not acceptable", Fields: fields}
	}

	description := in.Description
	if description == "" {
		description = describe(in.VCPUs, in.MemoryMiB)
	}

	row, err := s.store.CreateInstanceType(ctx, sqlc.CreateInstanceTypeParams{
		ID:          in.ID,
		Vcpus:       int64(in.VCPUs),
		MemoryMib:   int64(in.MemoryMiB),
		Description: description,
	})
	if err != nil {
		if isConstraint(err, "UNIQUE") || isConstraint(err, "PRIMARY KEY") {
			return Type{}, fmt.Errorf("%w: an instance type %q already exists", service.ErrAlreadyExists, in.ID)
		}
		return Type{}, fmt.Errorf("instancetype: create %q: %w", in.ID, err)
	}

	created := fromRow(row)
	s.logger.InfoContext(ctx, "instancetype: created",
		"instance_type", created.ID, "vcpus", created.VCPUs, "memory_mib", created.MemoryMiB)
	return created, nil
}

// Update changes the size or description of a type. Instances launched with
// it keep the size they were launched at; only later launches see the change.
//
// A description that was generated from the old size follows the new one,
// so "2 vCPU, 4 GiB" does not survive on a type that is now 4 vCPUs. A
// description an administrator wrote is theirs and is left alone.
func (s *Service) Update(ctx context.Context, id string, in UpdateInput) (Type, error) {
	var updated Type
	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		row, err := tx.GetInstanceType(ctx, sqlc.GetInstanceTypeParams{ID: id})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no instance type %q", service.ErrNotFound, id)
			}
			return fmt.Errorf("instancetype: read %q: %w", id, err)
		}

		current := fromRow(row)
		next := current
		if in.VCPUs != nil {
			next.VCPUs = *in.VCPUs
		}
		if in.MemoryMiB != nil {
			next.MemoryMiB = *in.MemoryMiB
		}
		switch {
		case in.Description != nil:
			next.Description = *in.Description
		case current.Description == describe(current.VCPUs, current.MemoryMiB):
			next.Description = describe(next.VCPUs, next.MemoryMiB)
		}

		fields := validateSize(next.VCPUs, next.MemoryMiB)
		if field := validateDescription(next.Description); field != nil {
			fields = append(fields, *field)
		}
		if len(fields) > 0 {
			return &service.ValidationError{Message: "the instance type is not acceptable", Fields: fields}
		}

		row, err = tx.UpdateInstanceType(ctx, sqlc.UpdateInstanceTypeParams{
			ID:          current.ID,
			Vcpus:       int64(next.VCPUs),
			MemoryMib:   int64(next.MemoryMiB),
			Description: next.Description,
		})
		if err != nil {
			return fmt.Errorf("instancetype: update %q: %w", current.ID, err)
		}

		updated = fromRow(row)
		return nil
	})
	if err != nil {
		return Type{}, err
	}

	s.logger.InfoContext(ctx, "instancetype: updated",
		"instance_type", updated.ID, "vcpus", updated.VCPUs, "memory_mib", updated.MemoryMiB)
	return updated, nil
}

// Delete removes a type. It is refused with service.ErrConflict while any
// instance row references it, terminated ones included: the foreign key from
// instances.instance_type has no ON DELETE clause (02-data-model.md).
func (s *Service) Delete(ctx context.Context, id string) error {
	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		users, err := tx.ListInstancesUsingInstanceType(ctx,
			sqlc.ListInstancesUsingInstanceTypeParams{InstanceType: id})
		if err != nil {
			return fmt.Errorf("instancetype: list the instances using %q: %w", id, err)
		}
		if len(users) > 0 {
			return fmt.Errorf("%w: instance type %q is in use by %s",
				service.ErrConflict, id, strings.Join(users, ", "))
		}

		deleted, err := tx.DeleteInstanceType(ctx, sqlc.DeleteInstanceTypeParams{ID: id})
		if err != nil {
			// A launch that committed between the check and the delete is
			// caught by the foreign key; it is the same conflict.
			if isConstraint(err, "FOREIGN KEY") {
				return fmt.Errorf("%w: instance type %q is in use", service.ErrConflict, id)
			}
			return fmt.Errorf("instancetype: delete %q: %w", id, err)
		}
		if deleted == 0 {
			return fmt.Errorf("%w: no instance type %q", service.ErrNotFound, id)
		}
		return nil
	})
	if err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "instancetype: deleted", "instance_type", id)
	return nil
}

// validateSize checks the vCPU and memory bounds.
func validateSize(vcpus, memoryMiB int) []service.FieldError {
	var fields []service.FieldError
	if vcpus < MinVCPUs || vcpus > MaxVCPUs {
		fields = append(fields, service.FieldError{
			Field:  "vcpus",
			Reason: fmt.Sprintf("must be between %d and %d", MinVCPUs, MaxVCPUs),
		})
	}
	if memoryMiB < MinMemoryMiB || memoryMiB > MaxMemoryMiB {
		fields = append(fields, service.FieldError{
			Field:  "memory_mib",
			Reason: fmt.Sprintf("must be between %d and %d", MinMemoryMiB, MaxMemoryMiB),
		})
	}
	return fields
}

// validateDescription bounds the free text.
func validateDescription(description string) *service.FieldError {
	if len(description) > maxDescriptionLen {
		return &service.FieldError{
			Field:  "description",
			Reason: fmt.Sprintf("must be at most %d bytes", maxDescriptionLen),
		}
	}
	return nil
}

// describe renders a size the way the presets read: "2 vCPU, 4 GiB", with
// MiB kept for a size that is not a whole number of GiB.
func describe(vcpus, memoryMiB int) string {
	if memoryMiB%1024 == 0 {
		return fmt.Sprintf("%d vCPU, %d GiB", vcpus, memoryMiB/1024)
	}
	return fmt.Sprintf("%d vCPU, %d MiB", vcpus, memoryMiB)
}

// isConstraint reports whether err is a SQLite constraint failure of the
// given kind ("UNIQUE", "PRIMARY KEY", "FOREIGN KEY").
func isConstraint(err error, kind string) bool {
	return err != nil && strings.Contains(err.Error(), kind+" constraint failed")
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
