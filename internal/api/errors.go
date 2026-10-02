package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/service"
)

// Error codes of the API error envelope (docs/design/01-api.md §Conventions).
const (
	CodeInvalidArgument  = "invalid_argument"
	CodeUnauthenticated  = "unauthenticated"
	CodePermissionDenied = "permission_denied"
	CodeNotFound         = "not_found"
	CodeAlreadyExists    = "already_exists"
	CodeConflict         = "conflict"
	CodeQuotaExceeded    = "quota_exceeded"
	CodeInternal         = "internal"
	CodeUnavailable      = "unavailable"
	CodeMethodNotAllowed = "method_not_allowed"
)

// FieldError describes why a single request field was rejected.
type FieldError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// ValidationError is returned by handlers when the request fails validation.
// It carries per-field details so the console can render inline errors.
type ValidationError struct {
	Message string
	Fields  []FieldError
}

// Error implements error.
func (e *ValidationError) Error() string { return e.Message }

// Invalid builds a ValidationError.
func Invalid(message string, fields ...FieldError) *ValidationError {
	return &ValidationError{Message: message, Fields: fields}
}

// statusError carries an explicit HTTP status for the cases the domain
// sentinels do not cover, such as unknown routes (404) and methods (405).
type statusError struct {
	status int
	code   string
	err    error
}

// Error implements error.
func (e *statusError) Error() string { return e.err.Error() }

// Unwrap exposes the wrapped sentinel to errors.Is.
func (e *statusError) Unwrap() error { return e.err }

type errorBody struct {
	Code      string       `json:"code"`
	Message   string       `json:"message"`
	Details   []FieldError `json:"details,omitempty"`
	RequestID string       `json:"request_id,omitempty"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

// WriteError renders err with the envelope of docs/design/01-api.md and aborts
// the handler chain. Unrecognized errors become a generic 500: the real message
// is only written to the log.
func WriteError(c *gin.Context, logger *slog.Logger, err error) {
	status, body := describeError(err)
	body.RequestID = middleware.RequestIDFrom(c)

	if status == http.StatusInternalServerError {
		logger.ErrorContext(c.Request.Context(), "request failed",
			"request_id", body.RequestID,
			"path", c.Request.URL.Path,
			"error", err,
		)
		body.Code = CodeInternal
		body.Message = "internal server error"
		body.Details = nil
	}

	c.AbortWithStatusJSON(status, errorResponse{Error: body})
}

// describeError maps an error to an HTTP status and the envelope body.
func describeError(err error) (int, errorBody) {
	var (
		validation *ValidationError
		explicit   *statusError
	)

	switch {
	case errors.As(err, &explicit):
		return explicit.status, errorBody{Code: explicit.code, Message: explicit.err.Error()}
	case errors.As(err, &validation):
		return http.StatusBadRequest, errorBody{
			Code:    CodeInvalidArgument,
			Message: validation.Message,
			Details: validation.Fields,
		}
	case errors.Is(err, service.ErrInvalid):
		return http.StatusBadRequest, errorBody{Code: CodeInvalidArgument, Message: err.Error()}
	case errors.Is(err, service.ErrUnauthorized):
		return http.StatusUnauthorized, errorBody{Code: CodeUnauthenticated, Message: err.Error()}
	case errors.Is(err, service.ErrForbidden):
		return http.StatusForbidden, errorBody{Code: CodePermissionDenied, Message: err.Error()}
	case errors.Is(err, service.ErrNotFound):
		return http.StatusNotFound, errorBody{Code: CodeNotFound, Message: err.Error()}
	case errors.Is(err, service.ErrAlreadyExists):
		return http.StatusConflict, errorBody{Code: CodeAlreadyExists, Message: err.Error()}
	case errors.Is(err, service.ErrConflict):
		return http.StatusConflict, errorBody{Code: CodeConflict, Message: err.Error()}
	case errors.Is(err, service.ErrQuota):
		return http.StatusTooManyRequests, errorBody{Code: CodeQuotaExceeded, Message: err.Error()}
	case errors.Is(err, service.ErrUnavailable):
		return http.StatusServiceUnavailable, errorBody{Code: CodeUnavailable, Message: err.Error()}
	default:
		return http.StatusInternalServerError, errorBody{
			Code:    CodeInternal,
			Message: err.Error(),
		}
	}
}

// notFound is the error behind unknown routes.
func notFound(c *gin.Context) error {
	return &statusError{
		status: http.StatusNotFound,
		code:   CodeNotFound,
		err: fmt.Errorf("%s %s: %w",
			c.Request.Method, c.Request.URL.Path, service.ErrNotFound),
	}
}

// methodNotAllowed is the error behind known paths called with a wrong method.
func methodNotAllowed(c *gin.Context) error {
	return &statusError{
		status: http.StatusMethodNotAllowed,
		code:   CodeMethodNotAllowed,
		err: fmt.Errorf("method %s is not allowed on %s: %w",
			c.Request.Method, c.Request.URL.Path, service.ErrInvalid),
	}
}
