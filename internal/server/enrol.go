package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// DefaultEnrolmentTokenLifetime is how long an enrolment token lasts
// unless told otherwise.
const DefaultEnrolmentTokenLifetime = time.Hour

// maxEnrolmentBytes bounds an enrolment, a token and a certificate
// signing request of a few hundred bytes each.
const maxEnrolmentBytes = 16 << 10

// errEnrolmentRefused reports an enrolment token that does not enrol
// the daemon it names: unknown, used, expired or for another daemon.
// Only a new token helps.
var errEnrolmentRefused = errors.New("enrolment refused")

// IssueEnrolmentToken makes a token that enrols daemon once, until
// lifetime from now, and keeps its secret's hash. The token itself is
// returned and not kept.
func (s *Store) IssueEnrolmentToken(ctx context.Context, daemon protocol.DaemonID, lifetime time.Duration) (protocol.EnrolmentToken, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.EnrolmentToken{}, fmt.Errorf("issue enrolment token: %w", err)
	}
	defer tx.Rollback()
	token, err := insertEnrolmentToken(ctx, tx, daemon, s.now(), lifetime)
	if err != nil {
		return protocol.EnrolmentToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.EnrolmentToken{}, fmt.Errorf("issue enrolment token: %w", err)
	}
	return token, nil
}

// insertEnrolmentToken makes a token that enrols daemon until lifetime
// after now, and deletes the tokens that expired by now.
func insertEnrolmentToken(ctx context.Context, tx *sql.Tx, daemon protocol.DaemonID, now time.Time, lifetime time.Duration) (protocol.EnrolmentToken, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM enrolment_tokens WHERE expires_at <= ?`, now.Unix()); err != nil {
		return protocol.EnrolmentToken{}, fmt.Errorf("issue enrolment token: %w", err)
	}
	token := protocol.EnrolmentToken{Daemon: daemon, Secret: newSecret()}
	_, err := tx.ExecContext(ctx, `INSERT INTO enrolment_tokens (secret_sha256, daemon_id, expires_at) VALUES (?, ?, ?)`,
		secretHash(token.Secret), string(daemon), now.Add(lifetime).Unix())
	if err != nil {
		return protocol.EnrolmentToken{}, fmt.Errorf("issue enrolment token: %w", err)
	}
	return token, nil
}

// enrol spends token and returns what sign returns, the daemon's
// certificate. The token is spent only when sign succeeds, so a request
// the CA refuses can be corrected and sent again with the same token.
func (s *Store) enrol(ctx context.Context, token protocol.EnrolmentToken, sign func() ([]byte, error)) ([]byte, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("enrol daemon %q: %w", token.Daemon, err)
	}
	defer tx.Rollback()
	hash := secretHash(token.Secret)
	var daemon string
	var expires int64
	err = tx.QueryRowContext(ctx, `SELECT daemon_id, expires_at FROM enrolment_tokens WHERE secret_sha256 = ?`, hash).Scan(&daemon, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: the token is unknown or was used", errEnrolmentRefused)
	}
	if err != nil {
		return nil, fmt.Errorf("enrol daemon %q: %w", token.Daemon, err)
	}
	if daemon != string(token.Daemon) {
		return nil, fmt.Errorf("%w: the token is not for daemon %q", errEnrolmentRefused, token.Daemon)
	}
	if !s.now().Before(time.Unix(expires, 0)) {
		return nil, fmt.Errorf("%w: the token expired at %s", errEnrolmentRefused, time.Unix(expires, 0).UTC().Format(time.RFC3339))
	}
	cert, err := sign()
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM enrolment_tokens WHERE secret_sha256 = ?`, hash); err != nil {
		return nil, fmt.Errorf("enrol daemon %q: %w", token.Daemon, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("enrol daemon %q: %w", token.Daemon, err)
	}
	return cert, nil
}

// postEnrol issues a daemon its certificate in exchange for an
// enrolment token and a certificate signing request
// (docs/adr/2026-10-10-vps-provisioning.md). It needs no client
// certificate: the token is what authenticates the request.
func (s *Server) postEnrol(w http.ResponseWriter, r *http.Request) {
	if s.ca == nil {
		http.Error(w, "enrolment is off: the server was started without the CA's key", http.StatusNotFound)
		return
	}
	var req protocol.Enrolment
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, maxEnrolmentBytes), &req); err != nil {
		http.Error(w, "decode enrolment: "+err.Error(), http.StatusBadRequest)
		return
	}
	token, err := protocol.ParseEnrolmentToken(req.Token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cert, err := s.store.enrol(r.Context(), token, func() ([]byte, error) {
		return s.ca.SignDaemon(token.Daemon, []byte(req.CSR))
	})
	switch {
	case errors.Is(err, errEnrolmentRefused):
		s.log.Warn("enrolment refused", "daemon", token.Daemon, "error", err)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	case errors.Is(err, pki.ErrInvalidRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	s.log.Info("enrolled daemon", "daemon", token.Daemon)
	writeJSON(w, http.StatusOK, protocol.Enrolled{Certificate: string(cert), CA: string(s.ca.CertPEM())})
}
