package auth

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// newTestStore opens a fresh SQLite database in a temp directory and seeds
// it with one user: admin / secret. Each test gets its own file, so tests
// never see each other's rows.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.Seed("admin", "secret"); err != nil {
		t.Fatalf("seed test store: %v", err)
	}
	return s
}

func login(t *testing.T, s *Store, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.LoginHandler(rr, req)
	return rr
}

func sessionCookie(t *testing.T, rr *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rr.Result().Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	t.Fatal("expected a session cookie to be set")
	return nil
}

func TestLoginAcceptsCorrectCredentials(t *testing.T) {
	s := newTestStore(t)
	rr := login(t, s, "admin", "secret")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("login correct: code=%d, want 204", rr.Code)
	}
	cookie := sessionCookie(t, rr)
	if cookie.Value == "" {
		t.Fatal("session cookie has no value")
	}
	if cookie.HttpOnly != true || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie flags: HttpOnly=%v SameSite=%v, want HttpOnly=true SameSite=Lax", cookie.HttpOnly, cookie.SameSite)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	s := newTestStore(t)
	rr := login(t, s, "admin", "nope")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("login wrong password: code=%d, want 401", rr.Code)
	}
	if len(rr.Result().Cookies()) != 0 {
		t.Fatal("no cookie should be set on failed login")
	}
}

func TestLoginRejectsUnknownUsername(t *testing.T) {
	s := newTestStore(t)
	// Same 401 as a wrong password, so usernames can't be enumerated.
	rr := login(t, s, "nobody", "whatever")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("login unknown user: code=%d, want 401", rr.Code)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != "invalid credentials" {
		t.Errorf("unknown-user body = %q, want the same as a wrong password", got)
	}
}

func TestLoginRejectsEmptyUsername(t *testing.T) {
	s := newTestStore(t)
	rr := login(t, s, "", "secret")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("login empty username: code=%d, want 401", rr.Code)
	}
}

func TestLoginRejectsNonJSON(t *testing.T) {
	s := newTestStore(t)
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader("not json"))
	rr := httptest.NewRecorder()
	s.LoginHandler(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("login bad body: code=%d, want 400", rr.Code)
	}
}

func TestLoginRejectsWrongMethod(t *testing.T) {
	s := newTestStore(t)
	req := httptest.NewRequest(http.MethodGet, "/api/login", nil)
	rr := httptest.NewRecorder()
	s.LoginHandler(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("login GET: code=%d, want 405", rr.Code)
	}
}

func TestLoginRejectsOversizedBody(t *testing.T) {
	s := newTestStore(t)

	// Without a cap, an unauthenticated caller could force an allocation as
	// large as they cared to send.
	huge := `{"username":"admin","password":"` + strings.Repeat("a", maxBodyBytes+1024) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(huge))
	rr := httptest.NewRecorder()
	s.LoginHandler(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("oversized login body: code=%d, want 400", rr.Code)
	}
}

func TestLogoutRejectsNonPost(t *testing.T) {
	s := newTestStore(t)

	// A GET logout would be reachable by cross-site top-level navigation,
	// which SameSite=Lax still attaches the session cookie to.
	req := httptest.NewRequest(http.MethodGet, "/api/logout", nil)
	rr := httptest.NewRecorder()
	s.LogoutHandler(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("logout GET: code=%d, want 405", rr.Code)
	}
}

func TestLogoutRevokesUserSessionsNotOtherUsers(t *testing.T) {
	s := newTestStore(t)

	// A second user so we can prove logout doesn't touch other accounts.
	if _, err := s.CreateUserFromPassword("ops", "hunter2"); err != nil {
		t.Fatalf("create second user: %v", err)
	}

	// Two sessions for admin, one for the other user.
	first := sessionCookie(t, login(t, s, "admin", "secret"))
	second := sessionCookie(t, login(t, s, "admin", "secret"))
	other := sessionCookie(t, login(t, s, "ops", "hunter2"))

	req := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	req.AddCookie(first)
	rr := httptest.NewRecorder()
	s.LogoutHandler(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("logout POST: code=%d, want 204", rr.Code)
	}

	// Tokens are only stored hashed now, so logout revokes by user: all of
	// admin's sessions die (their other devices included)...
	if _, found, _ := s.FindSession(first.Value); found {
		t.Error("logged-out session still resolves")
	}
	if _, found, _ := s.FindSession(second.Value); found {
		t.Error("admin's other session should have been revoked by user-wide logout")
	}
	// ...but no other user's sessions are touched.
	if _, found, _ := s.FindSession(other.Value); !found {
		t.Error("logout revoked a different user's session")
	}
}

func TestSessionRoundTripsThroughLogin(t *testing.T) {
	s := newTestStore(t)
	cookie := sessionCookie(t, login(t, s, "admin", "secret"))

	user, found, err := s.SessionUser(cookie.Value)
	if err != nil || !found {
		t.Fatalf("SessionUser: found=%v err=%v", found, err)
	}
	if user.Username != "admin" {
		t.Errorf("SessionUser username = %q, want admin", user.Username)
	}
	if len(user.Password) == 0 || string(user.Password) == "secret" {
		t.Error("SessionUser must expose the stored hash, never the plaintext")
	}
}

func TestSessionExpires(t *testing.T) {
	s := newTestStore(t)
	token, err := s.CreateSession(1)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Force the row into the past; FindSession treats expired as not-found
	// and deletes the row so the table can't grow without bound.
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE user_id = ?`, past, 1); err != nil {
		t.Fatalf("force expiry: %v", err)
	}
	if _, found, _ := s.FindSession(token); found {
		t.Fatal("expired session should not resolve")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, 1).Scan(&n); err != nil {
		t.Fatalf("count expired row: %v", err)
	}
	if n != 0 {
		t.Fatal("expired session row was not deleted")
	}
}

func TestFindSessionRejectsWrongToken(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateSession(1); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, found, _ := s.FindSession("totally-different-token"); found {
		t.Fatal("an unrelated token must not resolve to someone's session")
	}
}

func TestSessionTokensNotStoredInPlaintext(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	token, err := s.CreateSession(1)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// The literal cookie value must not appear anywhere in the DB - that's
	// the whole point of hashing tokens at rest (a leaked DB file would
	// otherwise be an instant session hijack).
	for _, col := range []string{"token_hash", "token_lookup"} {
		var stored string
		if err := s.db.QueryRow(`SELECT `+col+` FROM sessions WHERE user_id = ?`, 1).Scan(&stored); err != nil {
			t.Fatalf("read %s: %v", col, err)
		}
		if stored == token {
			t.Errorf("plaintext session token stored in %s", col)
		}
	}
	// And the round trip still works: the raw token resolves.
	if _, found, _ := s.FindSession(token); !found {
		t.Fatal("valid session token should resolve after hashing")
	}
}

func TestOpenMigratesLegacyTokenSchema(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "legacy.db")

	// Build a pre-hashing database: old sessions shape with a RAW token.
	legacyDB, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	_, err = legacyDB.Exec(`
CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, password_hash TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE sessions (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, token TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL, expires_at TEXT NOT NULL);
INSERT INTO users (username, password_hash, created_at) VALUES ('admin', ?, '2020-01-01T00:00:00Z');
INSERT INTO sessions (user_id, token, created_at, expires_at) VALUES (1, 'legacy-token-abc', '2020-01-01T00:00:00Z', '2099-01-01T00:00:00Z');
`, hash)
	legacyDB.Close()
	if err != nil {
		t.Fatalf("seed legacy db: %v", err)
	}

	// Open must migrate the table and preserve the legacy session.
	s, err := Open(dbFile)
	if err != nil {
		t.Fatalf("Open with legacy schema: %v", err)
	}
	defer s.Close()

	sess, found, err := s.FindSession("legacy-token-abc")
	if err != nil || !found {
		t.Fatalf("legacy session after migration: found=%v err=%v", found, err)
	}
	if sess.UserID != 1 || sess.Token != "legacy-token-abc" {
		t.Errorf("migrated session = %+v", sess)
	}
	// And the old column is gone (querying it must fail).
	if err := s.db.QueryRow(`SELECT token FROM sessions`).Scan(new(any)); err == nil {
		t.Error("legacy token column still queryable - migration did not replace the schema")
	}
}

func TestDBFileIsOwnerOnly(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "auth.db")
	s, err := Open(dbFile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	info, err := os.Stat(dbFile)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("db perms = %o, want 600 (session tokens must not be world-readable)", perm)
	}
}

func TestSecureCookieFlag(t *testing.T) {
	s := newTestStore(t)

	orig := cookieSecure
	cookieSecure = true
	defer func() { cookieSecure = orig }()

	cookie := sessionCookie(t, login(t, s, "admin", "secret"))
	if !cookie.Secure {
		t.Error("cookie should carry Secure when HOMELAB_COOKIE_SECURE is enabled")
	}
	// And without the env var, Secure stays off (plain-HTTP deployments).
	cookieSecure = false
	cookie = sessionCookie(t, login(t, s, "admin", "secret"))
	if cookie.Secure {
		t.Error("cookie should not carry Secure by default (backend serves plain HTTP)")
	}
}

func TestUnknownUserCannotLoginWithRealUsersPassword(t *testing.T) {
	s := newTestStore(t)
	// The dummy-hash path must never accidentally accept anything.
	rr := login(t, s, "ghost", "secret")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user with the real user's password: code=%d, want 401", rr.Code)
	}
}

func TestSessionUserMissingUser(t *testing.T) {
	s := newTestStore(t)

	// A session whose user row vanished (user deleted out from under it)
	// must not authenticate as anyone.
	token, err := s.CreateSession(9999)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, found, _ := s.SessionUser(token); found {
		t.Fatal("session for a nonexistent user should not resolve")
	}
}

func TestRevokeInvalidatesToken(t *testing.T) {
	s := newTestStore(t)
	cookie := sessionCookie(t, login(t, s, "admin", "secret"))
	user, _, _ := s.SessionUser(cookie.Value)

	if err := s.RevokeSession(user.ID, cookie.Value); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, found, _ := s.FindSession(cookie.Value); found {
		t.Fatal("revoked token should not resolve")
	}
}

func TestCreateUserFromPasswordStoresHashNotPlaintext(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.CreateUserFromPassword("ops", "hunter2"); err != nil {
		t.Fatalf("CreateUserFromPassword: %v", err)
	}

	u, found, err := s.FindUserByUsername("ops")
	if err != nil || !found {
		t.Fatalf("FindUserByUsername: found=%v err=%v", found, err)
	}
	if string(u.Password) == "hunter2" {
		t.Fatal("plaintext password stored in the database")
	}
	if err := bcrypt.CompareHashAndPassword(u.Password, []byte("hunter2")); err != nil {
		t.Errorf("stored hash does not verify against the plaintext: %v", err)
	}
}

func TestSeedOnlyWorksOnce(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.Seed("admin", "first"); err != nil {
		t.Fatalf("first Seed: %v", err)
	}
	if _, err := s.Seed("intruder", "second"); err == nil || (err != ErrAlreadySeeded) {
		t.Fatalf("second Seed err = %v, want ErrAlreadySeeded", err)
	}
	if n, _ := s.UserCount(); n != 1 {
		t.Fatalf("UserCount = %d, want 1 (second seed must not add a user)", n)
	}
}

func TestOpenCreatesMissingTables(t *testing.T) {
	s := newTestStore(t)
	if n, err := s.UserCount(); err != nil || n != 1 {
		t.Fatalf("UserCount = %d, %v; want 1 from the seeded user", n, err)
	}
}

func TestRequireAPIExposesUser(t *testing.T) {
	s := newTestStore(t)
	cookie := sessionCookie(t, login(t, s, "admin", "secret"))

	var got *User
	handler := s.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		got, _ = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("authed request: code=%d, want 200", rr.Code)
	}
	if got == nil || got.Username != "admin" {
		t.Fatalf("context user = %+v, want the admin user", got)
	}
}

func TestRequirePageRedirectsUnauthenticated(t *testing.T) {
	s := newTestStore(t)
	rr := httptest.NewRecorder()
	s.RequirePage(http.NotFoundHandler()).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusFound {
		t.Fatalf("unauth page: code=%d, want 302", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/login.html" {
		t.Errorf("redirect target = %q, want /login.html", loc)
	}
}

func TestSessionsAreConcurrentSafe(t *testing.T) {
	s := newTestStore(t)

	// SQLite runs through one connection (see Open), but the Store must
	// still behave under concurrent callers - the backend serves requests
	// from multiple goroutines.
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := s.CreateSession(1)
			if err != nil {
				t.Errorf("CreateSession: %v", err)
				return
			}
			s.FindSession(token)
			if user, found, err := s.SessionUser(token); err != nil || !found || user.Username != "admin" {
				t.Errorf("SessionUser: found=%v err=%v", found, err)
			}
		}()
	}
	wg.Wait()
}

// --- login rate limiter ------------------------------------------------------

func TestLoginRateLimiterAllowsWithinLimit(t *testing.T) {
	rl := NewLoginRateLimiter(3, 15*time.Second)
	for i := 0; i < 3; i++ {
		if !rl.Allow("ip|admin") {
			t.Fatalf("attempt %d denied, want allowed", i+1)
		}
	}
	if rl.Allow("ip|admin") {
		t.Error("attempt 4 allowed, want denied")
	}
}

func TestLoginRateLimiterWindowResets(t *testing.T) {
	rl := NewLoginRateLimiter(2, 15*time.Second)
	clock := time.Unix(0, 0)
	rl.now = func() time.Time { return clock }

	for i := 0; i < 2; i++ {
		if !rl.Allow("ip|admin") {
			t.Fatalf("attempt %d denied, want allowed", i+1)
		}
	}
	if rl.Allow("ip|admin") {
		t.Error("attempt 3 allowed, want denied")
	}
	clock = clock.Add(16 * time.Second) // window elapsed
	if !rl.Allow("ip|admin") {
		t.Error("denied after window elapsed, want a fresh window")
	}
}

func TestLoginRateLimiterResetOnSuccess(t *testing.T) {
	rl := NewLoginRateLimiter(2, 15*time.Second)
	rl.Allow("ip|admin")
	rl.Allow("ip|admin")
	if rl.Allow("ip|admin") {
		t.Fatal("precondition: should be locked out")
	}
	rl.Reset("ip|admin")
	if !rl.Allow("ip|admin") {
		t.Error("denied after Reset, want allowed")
	}
}

func TestLoginRateLimiterKeysAreIndependent(t *testing.T) {
	rl := NewLoginRateLimiter(1, 15*time.Second)
	if !rl.Allow("ip1|admin") || !rl.Allow("ip2|admin") || !rl.Allow("ip1|ops") {
		t.Error("different keys must not share their window")
	}
	if rl.Allow("ip1|admin") {
		t.Error("same key allowed past its own limit")
	}
}

func TestLoginKeyIncludesIPAndUsername(t *testing.T) {
	if got := LoginKey("192.168.1.5:55555", "admin"); got != "192.168.1.5|admin" {
		t.Errorf("LoginKey = %q", got)
	}
	if got := LoginKey("bogus", "admin"); got != "bogus|admin" {
		t.Errorf("LoginKey without port = %q", got)
	}
}

func TestLimitedLoginHandler429sAndResets(t *testing.T) {
	s := newTestStore(t)
	rl := NewLoginRateLimiter(3, 15*time.Second)
	handler := LimitedLoginHandler(s, rl)

	do := func(password string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"username": "admin", "password": password})
		rr := httptest.NewRecorder()
		handler(rr, httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body)))
		return rr
	}

	// 3 failures exhaust the window...
	for i := 0; i < 3; i++ {
		if rr := do("wrong"); rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: code=%d, want 401", i+1, rr.Code)
		}
	}
	// ...the 4th attempt (even with the RIGHT password) is throttled.
	rr := do("secret")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled attempt: code=%d, want 429", rr.Code)
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if got.Error != "too many attempts" {
		t.Errorf("429 body = %+v", got)
	}

	// A success resets the key - but we're locked out, so prove Reset via a
	// fresh limiter behaving identically to production wiring.
	rl.Reset("192.0.2.1|admin") // httptest.NewRequest RemoteAddr host
	if rr := do("secret"); rr.Code != http.StatusNoContent {
		t.Fatalf("after reset: code=%d, want 204", rr.Code)
	}

	// And a successful login resets the counter: only 2 more failures fit
	// before the window refills, and the 3rd attempt succeeds again.
	do("wrong")
	do("wrong")
	if rr := do("secret"); rr.Code != http.StatusNoContent {
		t.Fatalf("successful login did not reset the rate-limit window: code=%d", rr.Code)
	}
}
