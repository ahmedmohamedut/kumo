package instance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// UpdateInput is a PATCH /instances/{id} body.
//
// Name is a pointer and Tags a map that is nil when absent, because a PATCH that
// says nothing about a field must leave that field alone: an empty string is a
// name, and an empty map is a request to drop every tag. Those are different
// things from an absent field, and the shape is what tells them apart.
type UpdateInput struct {
	Name *string
	Tags map[string]string
}

// Update renames an instance or replaces its tags.
//
// The launch columns are deliberately out of reach. A vCPU count, a disk size and
// the image a guest boots from are inputs of a launch, and changing them on a
// running instance would say something the host is not doing: the domain keeps the
// CPU count and the disk it was defined with until it is launched again. So the
// only editable columns are the ones that exist to be read by a human.
func (s *Service) Update(ctx context.Context, instanceID string, in UpdateInput, includeUserData bool) (Instance, error) {
	if fields := validateUpdate(in); len(fields) > 0 {
		return Instance{}, &service.ValidationError{Message: "the instance is not acceptable", Fields: fields}
	}

	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		existing, err := tx.GetInstance(ctx, sqlc.GetInstanceParams{ID: instanceID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no instance %q", service.ErrNotFound, instanceID)
			}
			return fmt.Errorf("get instance: %w", err)
		}
		// A managed VM's name is its DNS label, and the label is the database's
		// endpoint: renaming it would silently break every client. Tags would
		// be harmless, but the row is not the caller's to edit either way.
		if ManagedBy(existing.ManagedBy) != ManagedByNone {
			// tx, not the store: see errManaged on the single writer connection.
			return s.errManaged(ctx, tx, existing)
		}

		if in.Name != nil {
			if _, err := tx.RenameInstance(ctx, sqlc.RenameInstanceParams{
				ID:        instanceID,
				Name:      *in.Name,
				UpdatedAt: formatTime(s.now()),
			}); err != nil {
				if isUniqueViolation(err) {
					return fmt.Errorf("%w: an instance named %q already exists",
						service.ErrAlreadyExists, *in.Name)
				}
				return fmt.Errorf("rename instance %q: %w", instanceID, err)
			}
		}

		// An empty map is not the same as no map: it is the only way to drop the
		// tags of an instance, and an operator who cannot clear a tag has to go
		// to the database.
		if in.Tags != nil {
			if err := replaceTags(ctx, tx, instanceID, in.Tags); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Instance{}, err
	}

	// Read the row back rather than projecting it here: the same read the detail
	// endpoint answers with, so a rename and a GET cannot disagree about what
	// user_data is for this caller.
	return s.Get(ctx, instanceID, includeUserData)
}

// validateUpdate checks the fields the caller did send. An absent field is not an
// error, but a body that carries nothing at all is: it would answer 200 for a
// request that changed nothing, which is worse than saying so.
func validateUpdate(in UpdateInput) []service.FieldError {
	var fields []service.FieldError

	if in.Name != nil {
		if reason := validateName(*in.Name); reason != "" {
			fields = append(fields, service.FieldError{Field: "name", Reason: reason})
		}
	}
	fields = append(fields, validateTags(in.Tags)...)

	if in.Name == nil && in.Tags == nil {
		fields = append(fields, service.FieldError{
			Field:  "body",
			Reason: "must carry a name or a tag set",
		})
	}
	return fields
}
