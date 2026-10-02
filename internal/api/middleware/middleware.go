// Package middleware holds the gin middleware shared by every route: request
// IDs, access logging, panic recovery and development CORS.
package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/logging"
)

// RequestIDHeader is the header the request ID is read from and echoed to.
const RequestIDHeader = "X-Request-Id"

// ContextKeyRequestID is the gin context key holding the request ID.
const ContextKeyRequestID = "request_id"

// RequestIDFrom returns the request ID of the current request, or "" when the
// RequestID middleware is not installed.
func RequestIDFrom(c *gin.Context) string {
	requestID, ok := c.Get(ContextKeyRequestID)
	if !ok {
		return ""
	}
	value, ok := requestID.(string)
	if !ok {
		return ""
	}
	return value
}

// RequestID reuses an inbound request ID when the caller supplied a sane one,
// otherwise it mints a new ULID. It sets the response header so clients can
// quote the ID from an error envelope.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader(RequestIDHeader)
		if requestID == "" || len(requestID) > 64 {
			requestID = id.ULID()
		}

		c.Set(ContextKeyRequestID, requestID)
		c.Writer.Header().Set(RequestIDHeader, requestID)

		c.Next()
	}
}

// AccessLog logs one structured line per request.
func AccessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery

		c.Next()

		pairs := []any{
			"request_id", RequestIDFrom(c),
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"bytes", c.Writer.Size(),
			"latency_ms", time.Since(start).Milliseconds(),
			"client_ip", c.ClientIP(),
		}
		if query != "" {
			pairs = append(pairs, "query", query)
		}
		if private := c.Errors.ByType(gin.ErrorTypePrivate).String(); private != "" {
			pairs = append(pairs, "errors", private)
		}

		attrs := logging.Attrs(pairs...)
		switch status := c.Writer.Status(); {
		case status >= http.StatusInternalServerError:
			logger.LogAttrs(c.Request.Context(), slog.LevelError, "request", attrs...)
		case status >= http.StatusBadRequest:
			logger.LogAttrs(c.Request.Context(), slog.LevelWarn, "request", attrs...)
		default:
			logger.LogAttrs(c.Request.Context(), slog.LevelInfo, "request", attrs...)
		}
	}
}

// Recovery turns a panic in a handler into a 500 with the error envelope and
// keeps the process alive.
func Recovery(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if aborted, ok := recovered.(error); ok && errors.Is(aborted, http.ErrAbortHandler) {
				panic(recovered)
			}

			logger.LogAttrs(c.Request.Context(), slog.LevelError, "panic recovered",
				slog.String("request_id", RequestIDFrom(c)),
				slog.String("path", c.Request.URL.Path),
				slog.Any("panic", recovered),
			)

			if !c.Writer.Written() {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{
						"code":       "internal",
						"message":    "internal server error",
						"request_id": RequestIDFrom(c),
					},
				})
			}
			c.Abort()
		}()

		c.Next()
	}
}

// CORS allows the Vite dev server (and nothing else) to call the API. It is a
// no-op when no origins are configured, which is the production default.
func CORS(allowedOrigins []string) gin.HandlerFunc {
	if len(allowedOrigins) == 0 {
		return func(c *gin.Context) { c.Next() }
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" && slices.Contains(allowedOrigins, origin) {
			header := c.Writer.Header()
			header.Set("Access-Control-Allow-Origin", origin)
			header.Set("Access-Control-Allow-Credentials", "true")
			header.Add("Vary", "Origin")
		}

		if c.Request.Method == http.MethodOptions && c.GetHeader("Access-Control-Request-Method") != "" {
			header := c.Writer.Header()
			header.Set("Access-Control-Allow-Methods", "GET,POST,PATCH,PUT,DELETE,OPTIONS")
			header.Set("Access-Control-Allow-Headers", "Authorization,Content-Type,X-Request-Id")
			header.Set("Access-Control-Max-Age", "600")
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}
