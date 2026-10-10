package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/component"
)

// The owner authenticates with one token, as a bearer token on the owner
// API or through the login form, which starts a session kept in a
// cookie (docs/adr/2026-10-08-owner-authentication.md). The server
// keeps only SHA-256 hashes of the token and of session ids. Both are
// 256 random bits, so guessing one from its hash is infeasible.
const (
	ownerTokenSetting = "owner_token_sha256"
	sessionLifetime   = 30 * 24 * time.Hour
	// sessionCookie has the __Host- prefix, so browsers accept it only
	// with Secure, Path=/ and no Domain.
	sessionCookie = "__Host-session"
	// maxLoginBytes bounds a login form, which carries only the token.
	maxLoginBytes = 4 << 10
)

var errWrongToken = errors.New("not the owner's token")

// newSecret returns 256 random bits, encoded for a header or a cookie.
func newSecret() string {
	secret := make([]byte, 32)
	rand.Read(secret)
	return base64.RawURLEncoding.EncodeToString(secret)
}

func secretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// IssueOwnerToken makes a new owner token, keeps its hash in place of
// the previous token's, and ends every session. The token itself is
// returned and not kept.
func (s *Store) IssueOwnerToken(ctx context.Context) (string, error) {
	token := newSecret()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("issue owner token: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO settings (name, value) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value`, ownerTokenSetting, secretHash(token))
	if err != nil {
		return "", fmt.Errorf("issue owner token: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
		return "", fmt.Errorf("issue owner token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("issue owner token: %w", err)
	}
	return token, nil
}

// HasOwnerToken reports whether an owner token has been issued.
func (s *Store) HasOwnerToken(ctx context.Context) (bool, error) {
	_, err := ownerTokenHash(ctx, s.db)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func ownerTokenHash(ctx context.Context, db queryer) (string, error) {
	var hash string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, ownerTokenSetting).Scan(&hash)
	return hash, err
}

// isOwnerToken reports whether token is the owner's. It is not, when no
// token has been issued.
func isOwnerToken(ctx context.Context, db queryer, token string) (bool, error) {
	hash, err := ownerTokenHash(ctx, db)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read owner token: %w", err)
	}
	return subtle.ConstantTimeCompare([]byte(secretHash(token)), []byte(hash)) == 1, nil
}

// login starts a session that lasts sessionLifetime from now, if token
// is the owner's, and returns its id. Sessions that have expired are
// deleted.
func (s *Store) login(ctx context.Context, token string, now time.Time) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("log in: %w", err)
	}
	defer tx.Rollback()
	owner, err := isOwnerToken(ctx, tx, token)
	if err != nil {
		return "", err
	}
	if !owner {
		return "", errWrongToken
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.Unix()); err != nil {
		return "", fmt.Errorf("log in: %w", err)
	}
	session := newSecret()
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions (id_sha256, expires_at) VALUES (?, ?)`,
		secretHash(session), now.Add(sessionLifetime).Unix())
	if err != nil {
		return "", fmt.Errorf("log in: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("log in: %w", err)
	}
	return session, nil
}

// isSession reports whether session is a session that has not expired
// at now.
func (s *Store) isSession(ctx context.Context, session string, now time.Time) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id_sha256 = ? AND expires_at > ?`,
		secretHash(session), now.Unix()).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read session: %w", err)
	}
	return true, nil
}

func (s *Store) logout(ctx context.Context, session string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_sha256 = ?`, secretHash(session)); err != nil {
		return fmt.Errorf("log out: %w", err)
	}
	return nil
}

// ownerOnly serves next only to the owner: a request with an
// Authorization header must carry the owner's token as a bearer token,
// and one without must carry a session cookie. Otherwise an API request
// gets 401, an htmx request gets 401 with HX-Redirect to the login form,
// and any other request is redirected to it. An insecure server serves
// every request.
func (s *Server) ownerOnly(next http.Handler) http.Handler {
	if s.insecure {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, err := s.isOwner(r)
		if err != nil {
			s.internalError(w, err)
			return
		}
		switch {
		case owner:
			next.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/v1/"):
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "the owner's token is required", http.StatusUnauthorized)
		case fromHTMX(r):
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
		default:
			redirect(w, r, "/login")
		}
	})
}

func (s *Server) isOwner(r *http.Request) (bool, error) {
	if header := r.Header.Get("Authorization"); header != "" {
		scheme, token, _ := strings.Cut(header, " ")
		if !strings.EqualFold(scheme, "Bearer") {
			return false, nil
		}
		return isOwnerToken(r.Context(), s.store.db, strings.TrimSpace(token))
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return false, nil
	}
	return s.store.isSession(r.Context(), cookie.Value, time.Now())
}

func (s *Server) routeLogin(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", s.getLogin)
	mux.HandleFunc("POST /login", s.postLogin)
	mux.HandleFunc("POST /logout", s.postLogout)
}

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	if s.insecure {
		redirect(w, r, "/")
		return
	}
	s.writeHTML(w, http.StatusOK, component.LoginPage(""))
}

// postLogin starts a session for the holder of the owner's token, and
// sends the browser to the dashboard.
func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	if s.insecure {
		redirect(w, r, "/")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "read form: "+err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now()
	session, err := s.store.login(r.Context(), strings.TrimSpace(r.PostForm.Get("token")), now)
	if errors.Is(err, errWrongToken) {
		s.writeHTML(w, http.StatusUnauthorized, component.LoginPage("That is not the owner's token."))
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    session,
		Path:     "/",
		MaxAge:   int(sessionLifetime / time.Second),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	redirect(w, r, "/")
}

// postLogout ends the browser's session, if it has one, and sends it to
// the login form.
func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	if s.insecure {
		redirect(w, r, "/")
		return
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if err := s.store.logout(r.Context(), cookie.Value); err != nil {
			s.internalError(w, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Path:     "/",
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	redirect(w, r, "/login")
}
