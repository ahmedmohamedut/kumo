package auth

import (
	"strings"
	"sync"
	"time"
)

// Login is rate limited to 5 attempts per minute per IP and username
// (docs/design/01-api.md §Auth), so a stolen username cannot be ground down by
// guessing passwords from outside.
const (
	loginAttempts = 5
	loginWindow   = time.Minute
)

// loginLimiter counts failed logins per "remote address + username" pair. It is
// in-memory on purpose: a control plane is a single process, and losing the
// counters on restart only costs an attacker five more attempts.
type loginLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	limit   int
	entries map[string]*loginAttemptsEntry
	now     func() time.Time
}

type loginAttemptsEntry struct {
	failures int
	// startedAt is when the current window opened, not when the first failure
	// happened: the window is fixed, not sliding.
	startedAt time.Time
}

// newLoginLimiter returns a limiter with the given budget per window.
func newLoginLimiter(limit int, window time.Duration, now func() time.Time) *loginLimiter {
	return &loginLimiter{
		window:  window,
		limit:   limit,
		entries: make(map[string]*loginAttemptsEntry),
		now:     now,
	}
}

// blocked reports whether the caller has spent its budget for the current
// window. A successful login clears the counter (see succeeded).
func (l *loginLimiter) blocked(remoteAddr, username string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.entries[loginKey(remoteAddr, username)]
	if !ok || l.expired(entry) {
		return false
	}

	return entry.failures >= l.limit
}

// failed records one failed login.
func (l *loginLimiter) failed(remoteAddr, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := loginKey(remoteAddr, username)
	entry, ok := l.entries[key]
	if !ok || l.expired(entry) {
		l.entries[key] = &loginAttemptsEntry{failures: 1, startedAt: l.now()}
		l.sweep()
		return
	}

	entry.failures++
}

// succeeded clears the counter of a caller that proved it knows the password.
func (l *loginLimiter) succeeded(remoteAddr, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.entries, loginKey(remoteAddr, username))
}

// sweep drops windows that can no longer block anybody, so the table cannot
// grow without bound. The caller holds the lock.
func (l *loginLimiter) sweep() {
	for key, entry := range l.entries {
		if l.expired(entry) {
			delete(l.entries, key)
		}
	}
}

func (l *loginLimiter) expired(entry *loginAttemptsEntry) bool {
	return l.now().Sub(entry.startedAt) >= l.window
}

// loginKey is the limiter key. The username is lowercased because users are
// stored case-insensitively, so "Ada" and "ada" must share one budget.
func loginKey(remoteAddr, username string) string {
	return strings.ToLower(strings.TrimSpace(remoteAddr)) + "\x00" + strings.ToLower(username)
}
