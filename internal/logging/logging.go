// Package logging builds the structured logger used across Kumo.
package logging

import (
	"io"
	"log/slog"
)

// New returns a JSON slog logger writing to w. Keys are snake_case so log
// fields line up with the field names used by the API and the database
// (see docs/design/00-architecture.md §8).
func New(level slog.Level, w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// Discard returns a logger that drops everything, for tests.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// Attrs turns alternating key/value pairs into log attributes, so callers can
// build attribute lists conditionally without resorting to raw keys. Keys must
// be snake_case; the odd element of an odd-length list is dropped.
func Attrs(pairs ...any) []slog.Attr {
	attrs := make([]slog.Attr, 0, (len(pairs)+1)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			continue
		}
		attrs = append(attrs, slog.Any(key, pairs[i+1]))
	}
	return attrs
}
