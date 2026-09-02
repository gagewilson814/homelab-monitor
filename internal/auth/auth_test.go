package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestLogoutRevokesOnlyThatSession(t *testing.T) {
	s := newTestStore(t)

	first := sessionCookie(t, login(t, s, "admin", "secret"))
	second := sessionCookie(t, login(t, s, "admin", "secret"))

	req := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	req.AddCookie(first)
	rr := httptest.NewRecorder()
	s.LogoutHandler(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("logout POST: code=%d, want 204", rr.Code)
	}

	if _, found, _ := s.FindSession(first.Value); found {
		t.Error("logged-out session still resolves")
	}
	if _, found, _ := s.FindSession(second.Value); !found {
		t.Error("logout revoked a different session of the same user")
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
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE token = ?`, past, token); err != nil {
		t.Fatalf("force expiry: %v", err)
	}
	if _, found, _ := s.FindSession(token); found {
		t.Fatal("expired session should not resolve")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token = ?`, token).Scan(&n); err != nil {
		t.Fatalf("count expired row: %v", err)
	}
	if n != 0 {
		t.Fatal("expired session row was not deleted")
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
