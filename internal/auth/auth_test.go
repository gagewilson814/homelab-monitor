package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// newTestStore returns a Store whose password verifies against "secret".
func newTestStore(t *testing.T) *Store {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt.GenerateFromPassword: %v", err)
	}
	return NewStore(string(hash))
}

func login(t *testing.T, s *Store, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.LoginHandler(rr, req)
	return rr
}

func TestLoginAcceptsCorrectPassword(t *testing.T) {
	s := newTestStore(t)
	rr := login(t, s, "secret")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("login correct: code=%d, want 204", rr.Code)
	}
	cookies := rr.Result().Cookies()
	var cookie *http.Cookie
	for _, c := range cookies {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatal("expected a session cookie to be set")
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	s := newTestStore(t)
	rr := login(t, s, "nope")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("login wrong: code=%d, want 401", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == cookieName {
			t.Fatal("no cookie should be set on failed login")
		}
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

func TestValidTokenRoundTrips(t *testing.T) {
	s := newTestStore(t)
	rr := login(t, s, "secret")
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("expected a session cookie")
	}
	if !s.valid(cookie.Value) {
		t.Fatal("token created by login should validate")
	}
}

func TestRequireAPIBlocksUnauthenticated(t *testing.T) {
	s := newTestStore(t)
	req := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	rr := httptest.NewRecorder()
	s.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth fleet: code=%d, want 401", rr.Code)
	}
}

func TestRequireAPIAllowsAuthenticated(t *testing.T) {
	s := newTestStore(t)
	rr := login(t, s, "secret")
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}

	handled := false
	handler := func(w http.ResponseWriter, r *http.Request) {
		handled = true
		w.WriteHeader(http.StatusOK)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	req.AddCookie(cookie)
	// Fresh recorder: login already consumed the old one's header.
	rr2 := httptest.NewRecorder()
	s.RequireAPI(handler)(rr2, req)
	if rr2.Code != http.StatusOK || !handled {
		t.Fatalf("authed fleet: code=%d handled=%v, want 200 and inner handler to run", rr2.Code, handled)
	}
}

func TestSessionExpires(t *testing.T) {
	// A token whose expiry is in the past must not validate.
	s := newTestStore(t)
	s.mu.Lock()
	s.sessions["expired"] = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	if s.valid("expired") {
		t.Fatal("expired token should not validate")
	}
}

func TestRevokeInvalidatesToken(t *testing.T) {
	s := newTestStore(t)
	rr := login(t, s, "secret")
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	s.revoke(cookie.Value)
	if s.valid(cookie.Value) {
		t.Fatal("revoked token should not validate")
	}
}

func TestSessionsAreConcurrentSafe(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token, _ := s.create()
			s.valid(token)
			s.revoke(token)
		}(i)
	}
	wg.Wait()
}
