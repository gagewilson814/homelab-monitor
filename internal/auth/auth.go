// Package auth provides the dashboard's multi-user authentication: SQLite-
// backed user accounts and session cookies, plus the HTTP handlers and
// middleware that guard the backend's API and pages. Passwords are stored
// as bcrypt hashes and never logged or returned; sessions are opaque
// 32-byte hex tokens persisted in the database, so they survive a backend
// restart (unlike the original in-memory session map this package started
// with).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite" // pure-Go SQLite driver; registered as "sqlite"
)

const (
	// cookieName is the session cookie's name. Renamed from "session" when
	// auth moved to SQL so an old in-memory cookie can't be mistaken for a
	// real one; the frontend never references the name literally.
	cookieName = "homelab_session"
	sessionTTL = 24 * time.Hour

	// maxBodyBytes caps how much of a request body the JSON decoder will
	// read. /api/login is reachable without a session, so without a cap
	// anyone who can reach the port could force a multi-gigabyte allocation
	// by posting one enormous "password" string.
	maxBodyBytes = 64 << 10
)

// dummyPasswordHash is compared against on the unknown-username login path
// so that a login for a user that doesn't exist costs the same bcrypt work
// as one for a user that does - without it, the ~130x timing gap between
// the two paths is a reliable remote username-enumeration oracle. It is the
// hash of a fixed throwaway string; nothing can ever authenticate with it.
const dummyPasswordHash = "$2a$10$gplLPYGmSz9YEJyHS.qaE.6kmrq1hJ9X65MEHoU5OdpWlIWzFiKYm"

// cookieSecure mirrors HOMELAB_COOKIE_SECURE: when set, the session cookie
// gets the Secure flag so browsers never send it over plain HTTP. Off by
// default because the backend serves plain HTTP by design (LAN/VPN use) -
// forcing Secure unconditionally would break those deployments.
var cookieSecure = os.Getenv("HOMELAB_COOKIE_SECURE") != ""

// ErrAlreadySeeded is returned by Seed when the database already has at
// least one user - seeding is a one-time bootstrap, not a user-adding tool.
var ErrAlreadySeeded = errors.New("database already has a user")

// User is one dashboard account. Password is the bcrypt hash (never the
// plaintext) and is never serialized anywhere.
type User struct {
	ID        int
	Username  string
	Password  []byte
	CreatedAt time.Time
}

// Session is one active login. The Token is the cookie's value - an opaque
// 32-byte hex string, never stored in the database (only its bcrypt hash
// and a SHA-256 lookup key are, see the sessions schema), so a leaked DB
// file can't be replayed as a cookie.
type Session struct {
	ID        int
	UserID    int
	Token     string
	ExpiresAt time.Time
}

// Store wraps the SQLite database holding users and sessions.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the SQLite database at dbFile and
// ensures the users and sessions tables exist. The single-connection limit
// sidesteps SQLITE_BUSY contention: this database serves one small operator
// dashboard, so serializing access costs nothing.
func Open(dbFile string) (*Store, error) {
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return nil, fmt.Errorf("open auth database %s: %w", dbFile, err)
	}
	db.SetMaxOpenConns(1)

	schema := `
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY,
	username TEXT UNIQUE NOT NULL,
	password_hash TEXT NOT NULL,
	created_at TEXT NOT NULL
);
` + sessionsSchema
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create auth schema: %w", err)
	}
	if err := migrateSessionSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate auth schema: %w", err)
	}
	// Session tokens are bearer credentials, and the DB also holds password
	// hashes: restrict the file to the owner so a local read of it can't
	// become a session hijack. Tightening an existing file's perms is
	// intentional (older databases were created 0644).
	if err := os.Chmod(dbFile, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
		db.Close()
		return nil, fmt.Errorf("chmod auth database %s: %w", dbFile, err)
	}
	return &Store{db: db}, nil
}

// sessionsSchema is the current sessions table shape: token_hash holds
// bcrypt(token) and token_lookup holds hex(sha256(token)). Both are one-way
// - the raw token is never at rest - but only sha256 is fast enough to use
// as an indexed lookup key, so FindSession fetches the row by token_lookup
// and then verifies with bcrypt.
const sessionsSchema = `CREATE TABLE IF NOT EXISTS sessions (
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	token_lookup TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
)`

// migrateSessionSchema converts a pre-hashing sessions table (which stored
// the raw token in a "token" column) to the token_hash/token_lookup shape,
// re-hashing every live session so a backend upgrade doesn't log anyone
// out. A fresh database needs no migration.
func migrateSessionSchema(db *sql.DB) error {
	old := false
	cols, err := db.Query(`PRAGMA table_info(sessions)`)
	if err == nil {
		for cols.Next() {
			var cid int
			var name, ctype string
			var notNull, pk int
			var dflt sql.NullString
			if err := cols.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err == nil {
				if name == "token" {
					old = true
				}
			}
		}
		cols.Close()
	}
	if !old {
		return nil
	}

	rows, err := db.Query(`SELECT user_id, token, created_at, expires_at FROM sessions`)
	if err != nil {
		return fmt.Errorf("read legacy sessions: %w", err)
	}
	type legacy struct {
		userID                  int
		token, created, expires string
	}
	var legacyRows []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.userID, &l.token, &l.created, &l.expires); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy sessions: %w", err)
		}
		legacyRows = append(legacyRows, l)
	}
	rows.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`ALTER TABLE sessions RENAME TO sessions_old`); err != nil {
		return err
	}
	if _, err := tx.Exec(sessionsSchema); err != nil {
		return err
	}
	for _, l := range legacyRows {
		lookup, hash, err := hashToken(l.token)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO sessions (user_id, token_hash, token_lookup, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
			l.userID, hash, lookup, l.created, l.expires,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DROP TABLE sessions_old`); err != nil {
		return err
	}
	return tx.Commit()
}

// hashToken returns (hex(sha256(token)), bcrypt(token)) for a raw session
// token - the two one-way forms the database stores. bcrypt is the
// at-rest-protection hash; sha256 is the O(1) indexed lookup key.
func hashToken(token string) (string, []byte, error) {
	digest := sha256.Sum256([]byte(token))
	hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.DefaultCost)
	if err != nil {
		return "", nil, fmt.Errorf("hash session token: %w", err)
	}
	return hex.EncodeToString(digest[:]), hash, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// UserCount reports how many accounts exist. The backend refuses to start
// while this is zero - there'd be nothing to log in with.
func (s *Store) UserCount() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// FindUserByUsername looks up a user by name; found is false when no such
// user exists (indistinguishable from a wrong password at the login path,
// so usernames aren't enumerable).
func (s *Store) FindUserByUsername(username string) (*User, bool, error) {
	var u User
	var createdAt string
	err := s.db.QueryRow(
		`SELECT id, username, password_hash, created_at FROM users WHERE username = ?`,
		strings.TrimSpace(username),
	).Scan(&u.ID, &u.Username, &u.Password, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("find user: %w", err)
	}
	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, false, err
	}
	return &u, true, nil
}

// CreateUser inserts a user with an already-hashed password. Only seed and
// test code should call this directly; the login path goes through
// CreateUserFromPassword so no plaintext ever reaches the database.
func (s *Store) CreateUser(username string, hash []byte) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, errors.New("username is required")
	}
	if len(hash) == 0 {
		return nil, errors.New("password hash is required")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(
		`INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)`,
		username, string(hash), now,
	)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return &User{ID: int(id), Username: username, Password: hash, CreatedAt: mustTime(now)}, nil
}

// CreateUserFromPassword bcrypt-hashes the plaintext password and inserts
// the user. The plaintext lives only in this function's argument.
func (s *Store) CreateUserFromPassword(username, plaintextPassword string) (*User, error) {
	if strings.TrimSpace(plaintextPassword) == "" {
		return nil, errors.New("password is required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintextPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	return s.CreateUser(username, hash)
}

// CreateSession mints a new session for userID: 32 random bytes as a hex
// token, expiring in sessionTTL. The token is what goes in the cookie; the
// database stores only its bcrypt hash and sha256 lookup key.
func (s *Store) CreateSession(userID int) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	token := hex.EncodeToString(raw)

	lookup, hash, err := hashToken(token)
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	if _, err := s.db.Exec(
		`INSERT INTO sessions (user_id, token_hash, token_lookup, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		userID, hash, lookup, now.Format(time.RFC3339), now.Add(sessionTTL).Format(time.RFC3339),
	); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return token, nil
}

// FindSession returns the session for token; found is false when the token
// is unknown OR expired - expired rows are deleted on sight so the table
// can't grow without bound. Lookup goes through the sha256 index, then the
// row's bcrypt hash is verified against the presented token.
func (s *Store) FindSession(token string) (*Session, bool, error) {
	digest := sha256.Sum256([]byte(token))
	var sess Session
	var tokenHash, createdAt, expiresAt string
	err := s.db.QueryRow(
		`SELECT id, user_id, token_hash, created_at, expires_at FROM sessions WHERE token_lookup = ?`,
		hex.EncodeToString(digest[:]),
	).Scan(&sess.ID, &sess.UserID, &tokenHash, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("find session: %w", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(tokenHash), []byte(token)) != nil {
		return nil, false, nil
	}
	if sess.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return nil, false, err
	}
	if time.Now().After(sess.ExpiresAt) {
		if _, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, sess.ID); err != nil {
			return nil, false, fmt.Errorf("delete expired session: %w", err)
		}
		return nil, false, nil
	}
	sess.Token = token
	return &sess, true, nil
}

// RevokeSession deletes one of userID's sessions (the caller's own logout
// path), leaving any other active sessions alone.
func (s *Store) RevokeSession(userID int, token string) error {
	sess, found, err := s.FindSession(token)
	if err != nil {
		return err
	}
	if !found || sess.UserID != userID {
		return nil // nothing of this user's to revoke
	}
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, sess.ID); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// RevokeUserSessions deletes every active session belonging to userID -
// used by logout, which can no longer look sessions up by raw token. It
// signs the user's other devices out too, and never touches other users.
func (s *Store) RevokeUserSessions(userID int) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("revoke user sessions: %w", err)
	}
	return nil
}

// SessionUser resolves a token to its logged-in user. found is false for an
// unknown/expired token; a token whose user row has vanished counts as not
// found too (nil user), so deleting a user signs out its sessions.
func (s *Store) SessionUser(token string) (*User, bool, error) {
	sess, found, err := s.FindSession(token)
	if err != nil || !found {
		return nil, false, err
	}

	var u User
	var createdAt string
	err = s.db.QueryRow(
		`SELECT id, username, password_hash, created_at FROM users WHERE id = ?`,
		sess.UserID,
	).Scan(&u.ID, &u.Username, &u.Password, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("find session user: %w", err)
	}
	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, false, err
	}
	return &u, true, nil
}

// Seed bootstraps the first user. It refuses to do anything once any user
// exists, so re-running it can't silently add an unlisted admin account.
func (s *Store) Seed(username, plaintextPassword string) (*User, error) {
	n, err := s.UserCount()
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, ErrAlreadySeeded
	}
	return s.CreateUserFromPassword(username, plaintextPassword)
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginHandler authenticates a username/password pair and starts a session.
// It is deliberately the ONLY unauthenticated API route, and it caps its
// request body so an unauthenticated caller can't force a huge allocation.
// A missing username and a wrong password return the same 401, so the
// endpoint can't be used to enumerate usernames.
func (s *Store) LoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		// Distinguish "body too large" (413) from "body not valid JSON"
		// (400) - the cap is doing its job either way, but the status code
		// should say which.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			json.NewEncoder(w).Encode(map[string]string{"error": "request body too large"})
			return
		}
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	user, found, err := s.FindUserByUsername(req.Username)
	if err != nil {
		http.Error(w, "could not look up user", http.StatusInternalServerError)
		return
	}
	if !found {
		// Keep the unknown-username path on the same bcrypt work as the
		// known-username path so response timing can't be used to enumerate
		// usernames (verified experimentally: ~130x gap without this).
		user = &User{Password: []byte(dummyPasswordHash)}
	}
	if bcrypt.CompareHashAndPassword(user.Password, []byte(req.Password)) != nil {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := s.CreateSession(user.ID)
	if err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}

	SetCookie := func(c *http.Cookie) {
		c.Secure = cookieSecure
		http.SetCookie(w, c)
	}
	SetCookie(&http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	w.WriteHeader(http.StatusNoContent)
}

// LogoutHandler revokes the caller's sessions. Because tokens are no longer
// stored in lookupable form, logout revokes by user: all of the calling
// user's sessions are deleted (signing out their other devices too), and no
// other user's sessions are touched. It requires POST so a cross-site
// top-level navigation (which SameSite=Lax would still attach the cookie
// to) can't sign the user out.
func (s *Store) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" {
		if user, found, err := s.SessionUser(cookie.Value); err == nil && found {
			// A failed revoke can only be a database error; the cookie is
			// cleared either way, so don't fail the logout over it.
			_ = s.RevokeUserSessions(user.ID)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   cookieSecure,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

// --- Request context & middleware ------------------------------------------

type userContextKey struct{}

// UserFromContext returns the authenticated user a middleware installed on
// the request, or ok=false for unauthenticated requests.
func UserFromContext(ctx context.Context) (*User, bool) {
	user, ok := ctx.Value(userContextKey{}).(*User)
	return user, ok && user != nil
}

// withUser attaches user to the request context so handlers downstream of
// the middleware can identify the caller (e.g. /api/me).
func withUser(ctx context.Context, user *User) context.Context {
	return context.WithValue(ctx, userContextKey{}, user)
}

// Authenticated resolves the request's session cookie to its user.
func (s *Store) Authenticated(r *http.Request) (*User, bool) {
	cookie, err := r.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		return nil, false
	}
	user, found, err := s.SessionUser(cookie.Value)
	if err != nil || !found {
		return nil, false
	}
	return user, true
}

// RequireAPI protects JSON API endpoints, responding 401 when
// unauthenticated, and exposes the current user to the handler via the
// request context.
func (s *Store) RequireAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.Authenticated(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(withUser(r.Context(), user)))
	}
}

// RequirePage protects page/static routes, redirecting to the login page
// when unauthenticated. Like RequireAPI it exposes the current user via the
// request context, so a handler serving a page can render it.
func (s *Store) RequirePage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.Authenticated(r)
		if !ok {
			http.Redirect(w, r, "/login.html", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), user)))
	})
}

// --- helpers ----------------------------------------------------------------

func parseTime(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", v, err)
	}
	return t, nil
}

func mustTime(v string) time.Time {
	t, err := parseTime(v)
	if err != nil {
		panic("auth: wrote unparsable timestamp: " + err.Error())
	}
	return t
}
