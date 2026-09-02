package auth

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// LoginRateLimiter is a fixed-window rate limiter for the only
// unauthenticated endpoint, POST /api/login, keyed by username + client IP.
// Without it, bcrypt's ~50ms cost is the only brake on online password
// guessing (~72k attempts/hr per connection). The window is fixed (not a
// token bucket) because that's trivial to reason about and unit-test; the
// clock is injectable so tests don't sleep.
type LoginRateLimiter struct {
	mu       sync.Mutex
	attempts map[string]*loginWindow
	limit    int
	window   time.Duration
	now      func() time.Time
}

type loginWindow struct {
	count   int
	started time.Time
}

// NewLoginRateLimiter allows limit attempts per key per window.
func NewLoginRateLimiter(limit int, window time.Duration) *LoginRateLimiter {
	return &LoginRateLimiter{
		attempts: make(map[string]*loginWindow),
		limit:    limit,
		window:   window,
		now:      time.Now,
	}
}

// Allow records one attempt for key and reports whether it's within the
// limit. A key's first attempt (or first after the window has elapsed)
// starts a fresh window; once limit attempts land inside one window, all
// further attempts are denied until the window expires.
func (l *LoginRateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	w, ok := l.attempts[key]
	if !ok || now.Sub(w.started) >= l.window {
		l.attempts[key] = &loginWindow{count: 1, started: now}
		return true
	}
	w.count++
	return w.count <= l.limit
}

// Reset clears a key's attempt count - called on successful login so a
// fat-fingered operator isn't locked out by their own typos.
func (l *LoginRateLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

// LoginKey builds the rate-limit key for a login request: client IP (host
// part of RemoteAddr) plus the submitted username, so one IP can't brute
// many usernames at full speed and one username can't be brute-forced from
// many IPs at full speed without tripping the IP half of the key.
func LoginKey(remoteAddr, username string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return host + "|" + username
}

// LimitedLoginHandler wraps the login handler with the rate limiter. The
// body is read here (username is part of the key) and re-attached so the
// wrapped LoginHandler can decode it as usual; a successful login (204)
// resets the key's window. Denied requests get 429 with a JSON error body.
// This wrapper is applied to /api/login ONLY - no other route is throttled.
func LimitedLoginHandler(store *Store, limiter *LoginRateLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := readCappedBody(w, r)
		if err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		var peek struct {
			Username string `json:"username"`
		}
		json.Unmarshal(raw, &peek) // tolerate garbage; LoginHandler will 400

		key := LoginKey(r.RemoteAddr, peek.Username)
		if !limiter.Allow(key) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]string{"error": "too many attempts"})
			return
		}

		r.Body = readCloser(raw)
		rec := &statusRecorder{ResponseWriter: w}
		store.LoginHandler(rec, r)
		if rec.code == http.StatusNoContent {
			limiter.Reset(key)
		}
	}
}

// --- small plumbing helpers -------------------------------------------------

// statusRecorder captures the status code written to the client so the
// wrapper can tell a successful login (204) from a failure.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// readCappedBody reads the request body under the same cap LoginHandler
// enforces, leaving nothing unconsumed.
func readCappedBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
}

func readCloser(b []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(b))
}
