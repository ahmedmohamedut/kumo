// Package service holds the business logic of Kumo. Handlers in internal/api
// call into it; it never imports gin, and it owns the sentinel errors that the
// api layer maps to HTTP status codes (docs/design/01-api.md §Conventions).
package service

import "errors"

// Sentinel errors. Wrap them with additional context:
//
//	return nil, fmt.Errorf("create key pair %q: %w", name, service.ErrConflict)
var (
	// ErrInvalid is a malformed or unusable request argument.
	ErrInvalid = errors.New("invalid argument")
	// ErrUnauthorized means the caller is not authenticated.
	ErrUnauthorized = errors.New("unauthenticated")
	// ErrForbidden means the caller is authenticated but not allowed.
	ErrForbidden = errors.New("permission denied")
	// ErrNotFound means the resource does not exist.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists means the resource name/id is taken.
	ErrAlreadyExists = errors.New("already exists")
	// ErrConflict means another operation holds the resource.
	ErrConflict = errors.New("conflict")
	// ErrQuota means the request exceeds a capacity or quota limit.
	ErrQuota = errors.New("quota exceeded")
	// ErrUnavailable means a dependency (hypervisor, worker) cannot serve it.
	ErrUnavailable = errors.New("unavailable")
)
