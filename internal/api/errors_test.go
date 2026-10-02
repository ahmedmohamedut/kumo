package api_test

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
)

func systemService(logger *slog.Logger) *system.Service {
	return system.New(logger, "test", "fake", nil, nil)
}

// errDialTimeout stands in for an unexpected dependency failure.
var errDialTimeout = errors.New("i/o timeout")

func TestWriteErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{
			name:       "validation error",
			err:        api.Invalid("name is required", api.FieldError{Field: "name", Reason: "required"}),
			wantStatus: http.StatusBadRequest,
			wantCode:   api.CodeInvalidArgument,
			wantMsg:    "name is required",
		},
		{
			name:       "wrapped invalid argument",
			err:        fmt.Errorf("parse root_disk_gb: %w", service.ErrInvalid),
			wantStatus: http.StatusBadRequest,
			wantCode:   api.CodeInvalidArgument,
			wantMsg:    "parse root_disk_gb: invalid argument",
		},
		{
			name:       "unauthenticated",
			err:        fmt.Errorf("verify jwt: %w", service.ErrUnauthorized),
			wantStatus: http.StatusUnauthorized,
			wantCode:   api.CodeUnauthenticated,
		},
		{
			name:       "permission denied",
			err:        service.ErrForbidden,
			wantStatus: http.StatusForbidden,
			wantCode:   api.CodePermissionDenied,
		},
		{
			name:       "not found",
			err:        fmt.Errorf("get instance %q: %w", "i-1", service.ErrNotFound),
			wantStatus: http.StatusNotFound,
			wantCode:   api.CodeNotFound,
		},
		{
			name:       "already exists",
			err:        service.ErrAlreadyExists,
			wantStatus: http.StatusConflict,
			wantCode:   api.CodeAlreadyExists,
		},
		{
			name:       "conflict",
			err:        service.ErrConflict,
			wantStatus: http.StatusConflict,
			wantCode:   api.CodeConflict,
		},
		{
			name:       "quota exceeded",
			err:        service.ErrQuota,
			wantStatus: http.StatusTooManyRequests,
			wantCode:   api.CodeQuotaExceeded,
		},
		{
			name:       "dependency unavailable",
			err:        service.ErrUnavailable,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   api.CodeUnavailable,
		},
		{
			name:       "unknown error is hidden behind a 500",
			err:        fmt.Errorf("%w: dial tcp 10.0.0.5:5432", errDialTimeout),
			wantStatus: http.StatusInternalServerError,
			wantCode:   api.CodeInternal,
			wantMsg:    "internal server error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			logger, logs := testLogger()
			handler := failingHandler(t, logger, tt.err)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/fail", http.NoBody))

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.wantStatus, recorder.Body.String())
			}

			body := decodeError(t, recorder.Body.Bytes())
			if body.Error.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.wantCode)
			}
			if tt.wantMsg != "" && body.Error.Message != tt.wantMsg {
				t.Errorf("message = %q, want %q", body.Error.Message, tt.wantMsg)
			}
			if body.Error.RequestID == "" {
				t.Error("request_id is empty")
			}

			if tt.wantStatus == http.StatusInternalServerError {
				if !strings.Contains(logs.String(), "i/o timeout") {
					t.Errorf("the real error was not logged: %s", logs.String())
				}
				if strings.Contains(recorder.Body.String(), "10.0.0.5") {
					t.Errorf("the 500 body leaked the error: %s", recorder.Body.String())
				}
			}
		})
	}
}

func TestWriteErrorCarriesFieldDetails(t *testing.T) {
	t.Parallel()

	handler := failingHandler(t, logging.Discard(), api.Invalid("invalid request",
		api.FieldError{Field: "name", Reason: "required"},
		api.FieldError{Field: "root_disk_gb", Reason: "must be >= 10"},
	))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/fail", http.NoBody))

	body := decodeError(t, recorder.Body.Bytes())
	if len(body.Error.Details) != 2 {
		t.Fatalf("details = %+v, want 2 entries", body.Error.Details)
	}
	if body.Error.Details[0] != (api.FieldError{Field: "name", Reason: "required"}) {
		t.Errorf("details[0] = %+v", body.Error.Details[0])
	}
}

func TestWriteErrorHidesDetailsOnUnknownErrors(t *testing.T) {
	t.Parallel()

	handler := failingHandler(t, logging.Discard(), fmt.Errorf("wrapped: %w", service.ErrNotFound))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/fail", http.NoBody))

	if strings.Contains(recorder.Body.String(), `"details"`) {
		t.Errorf("sentinel errors should not carry details: %s", recorder.Body.String())
	}
}

// failingHandler mounts a route that always fails with the given error.
func failingHandler(t *testing.T, logger *slog.Logger, err error) *gin.Engine {
	t.Helper()

	if logger == nil {
		logger = logging.Discard()
	}

	handler := api.NewHandler(api.Options{
		Logger:  logger,
		GinMode: gin.TestMode,
		System:  systemService(logger),
	})
	router := handler.Router()
	router.GET("/fail", func(c *gin.Context) {
		api.WriteError(c, logger, err)
	})

	return router
}
