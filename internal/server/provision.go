package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/hetzner"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// hetznerTimeout bounds each call the server makes to the Hetzner API.
const hetznerTimeout = time.Minute

// Provisioning is how the server creates daemon VPSes at Hetzner
// (docs/adr/2026-10-10-vps-provisioning.md).
type Provisioning struct {
	Hetzner *hetzner.Client
	// ServerType, Location and Image name what each VPS is created as,
	// such as "cx23", "fsn1" and "debian-13".
	ServerType, Location, Image string
	// DaemonURL is where a VPS downloads the daemon binary from. The
	// literal "{arch}" is replaced with "amd64" or "arm64" according to
	// ServerType; a URL without it is used unchanged.
	DaemonURL string
	// PublicURL is the server's address as a VPS's daemon dials it.
	PublicURL string
}

// vpsState is how far a provisioned VPS has come.
type vpsState string

const (
	// vpsCreating: the server asked Hetzner for the VPS, and its daemon
	// has not enrolled.
	vpsCreating vpsState = "creating"
	// vpsEnrolled: its daemon got its certificate.
	vpsEnrolled vpsState = "enrolled"
	// vpsConnected is shown, never stored: enrolled, and its daemon has
	// its command stream open.
	vpsConnected vpsState = "connected"
	// vpsDestroying: the owner asked for it to be destroyed, and Hetzner
	// has not yet confirmed it.
	vpsDestroying vpsState = "destroying"
	vpsDestroyed  vpsState = "destroyed"
)

var (
	errProvisioningOff = errors.New("provisioning is off: the server was started without a Hetzner token")
	errUnknownVPS      = errors.New("not a VPS the server provisioned")
	errVPSDestroyed    = errors.New("VPS already destroyed")
	// errHetzner reports a Hetzner API call that failed; the owner may
	// try again.
	errHetzner = errors.New("Hetzner refused or failed")
)

// vps is a VPS the server provisioned. ServerID is 0 until Hetzner
// answered the request to create it.
type vps struct {
	Daemon     protocol.DaemonID
	ServerID   hetzner.ServerID
	ServerType string
	Location   string
	CreatedAt  time.Time
	State      vpsState
}

// vpsView is a VPS as the owner API reports it, with State connected
// while its daemon is.
type vpsView struct {
	Daemon     protocol.DaemonID `json:"daemon"`
	ServerID   hetzner.ServerID  `json:"server_id,omitempty"`
	ServerType string            `json:"server_type"`
	Location   string            `json:"location"`
	CreatedAt  time.Time         `json:"created_at"`
	State      vpsState          `json:"state"`
}

// insertVPS records a VPS for daemon as creating, and makes the
// enrolment token its daemon enrols with.
func (s *Store) insertVPS(ctx context.Context, daemon protocol.DaemonID, serverType, location string) (protocol.EnrolmentToken, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.EnrolmentToken{}, fmt.Errorf("record VPS %q: %w", daemon, err)
	}
	defer tx.Rollback()
	now := s.now()
	_, err = tx.ExecContext(ctx, `INSERT INTO vpses (daemon_id, server_type, location, created_at, state) VALUES (?, ?, ?, ?, ?)`,
		string(daemon), serverType, location, formatTime(now.UTC()), string(vpsCreating))
	if err != nil {
		return protocol.EnrolmentToken{}, fmt.Errorf("record VPS %q: %w", daemon, err)
	}
	token, err := insertEnrolmentToken(ctx, tx, daemon, now, DefaultEnrolmentTokenLifetime)
	if err != nil {
		return protocol.EnrolmentToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.EnrolmentToken{}, fmt.Errorf("record VPS %q: %w", daemon, err)
	}
	return token, nil
}

// forgetVPS deletes the record of a VPS that was never created, and its
// daemon's enrolment tokens.
func (s *Store) forgetVPS(ctx context.Context, daemon protocol.DaemonID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("forget VPS %q: %w", daemon, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM vpses WHERE daemon_id = ?`, string(daemon)); err != nil {
		return fmt.Errorf("forget VPS %q: %w", daemon, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM enrolment_tokens WHERE daemon_id = ?`, string(daemon)); err != nil {
		return fmt.Errorf("forget VPS %q: %w", daemon, err)
	}
	return tx.Commit()
}

func (s *Store) setVPSServer(ctx context.Context, daemon protocol.DaemonID, server hetzner.ServerID) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE vpses SET server_id = ? WHERE daemon_id = ?`, int64(server), string(daemon)); err != nil {
		return fmt.Errorf("record VPS %q's server: %w", daemon, err)
	}
	return nil
}

// setVPSState records the VPS's state. A VPS being destroyed loses its
// daemon's enrolment tokens.
func (s *Store) setVPSState(ctx context.Context, daemon protocol.DaemonID, state vpsState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record VPS %q %s: %w", daemon, state, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE vpses SET state = ? WHERE daemon_id = ?`, string(state), string(daemon)); err != nil {
		return fmt.Errorf("record VPS %q %s: %w", daemon, state, err)
	}
	if state == vpsDestroying || state == vpsDestroyed {
		if _, err := tx.ExecContext(ctx, `DELETE FROM enrolment_tokens WHERE daemon_id = ?`, string(daemon)); err != nil {
			return fmt.Errorf("record VPS %q %s: %w", daemon, state, err)
		}
	}
	return tx.Commit()
}

// vpses returns the VPSes the server provisioned, oldest first; those
// destroyed only when withDestroyed is set.
func (s *Store) vpses(ctx context.Context, withDestroyed bool) ([]vps, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT daemon_id, server_id, server_type, location, created_at, state FROM vpses
		WHERE ?1 OR state <> ?2
		ORDER BY created_at, daemon_id`, withDestroyed, string(vpsDestroyed))
	if err != nil {
		return nil, fmt.Errorf("read VPSes: %w", err)
	}
	defer rows.Close()
	var list []vps
	for rows.Next() {
		v, err := scanVPS(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, v)
	}
	return list, rows.Err()
}

// vpsOf returns the VPS daemon runs on, or errUnknownVPS.
func (s *Store) vpsOf(ctx context.Context, daemon protocol.DaemonID) (vps, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT daemon_id, server_id, server_type, location, created_at, state FROM vpses WHERE daemon_id = ?`, string(daemon))
	v, err := scanVPS(row)
	if errors.Is(err, sql.ErrNoRows) {
		return vps{}, fmt.Errorf("daemon %q: %w", daemon, errUnknownVPS)
	}
	return v, err
}

func scanVPS(row interface{ Scan(...any) error }) (vps, error) {
	var v vps
	var daemon, createdAt, state string
	var server sql.NullInt64
	if err := row.Scan(&daemon, &server, &v.ServerType, &v.Location, &createdAt, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return vps{}, err
		}
		return vps{}, fmt.Errorf("read VPS: %w", err)
	}
	created, err := parseTime(createdAt)
	if err != nil {
		return vps{}, fmt.Errorf("read VPS %q: %w", daemon, err)
	}
	v.Daemon, v.ServerID, v.CreatedAt, v.State = protocol.DaemonID(daemon), hetzner.ServerID(server.Int64), created, vpsState(state)
	return v, nil
}

// newVPSDaemonID returns "vps-" and eight random lowercase letters and
// digits, which make a hostname Hetzner accepts as a server's name.
func newVPSDaemonID() protocol.DaemonID {
	random := make([]byte, 5)
	rand.Read(random)
	return protocol.DaemonID("vps-" + strings.ToLower(base32.StdEncoding.EncodeToString(random)))
}

// vpsLabels are the Hetzner labels of the VPS daemon runs on, which mark
// it as the orchestrator's.
func vpsLabels(daemon protocol.DaemonID) map[string]string {
	return map[string]string{"orchestrator": "1", "daemon": string(daemon)}
}

// provision creates a VPS whose daemon enrols with a new token, and
// records it as creating. A VPS that Hetzner did not create is not
// recorded.
func (s *Server) provision(ctx context.Context) (vps, error) {
	p := s.provisioning
	if p == nil || s.ca == nil {
		return vps{}, errProvisioningOff
	}
	daemon := newVPSDaemonID()
	token, err := s.store.insertVPS(ctx, daemon, p.ServerType, p.Location)
	if err != nil {
		return vps{}, err
	}
	userData, err := renderUserData(userDataInput{Token: token, CA: s.ca.CertPEM(), ServerURL: p.PublicURL, DaemonURL: daemonBinaryURL(p.DaemonURL, p.ServerType)})
	if err == nil {
		err = s.createVPS(ctx, daemon, userData)
	}
	if err != nil {
		if forgetErr := s.store.forgetVPS(context.WithoutCancel(ctx), daemon); forgetErr != nil {
			s.log.Error("forget the VPS that was not created", "daemon", daemon, "error", forgetErr)
		}
		return vps{}, err
	}
	s.log.Info("provisioned VPS", "daemon", daemon, "server_type", p.ServerType, "location", p.Location)
	return s.store.vpsOf(ctx, daemon)
}

// createVPS asks Hetzner for daemon's VPS and records its server. The
// owner's request ending does not cut the call short, since a server
// Hetzner created would be left unrecorded. When the call fails, any
// server Hetzner created for daemon anyway is deleted.
func (s *Server) createVPS(ctx context.Context, daemon protocol.DaemonID, userData string) error {
	p := s.provisioning
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hetznerTimeout)
	defer cancel()
	server, err := p.Hetzner.CreateServer(ctx, hetzner.CreateServer{
		Name: string(daemon), ServerType: p.ServerType, Image: p.Image, Location: p.Location,
		Labels: vpsLabels(daemon), UserData: userData,
	})
	if err == nil {
		if err = s.store.setVPSServer(ctx, daemon, server.ID); err == nil {
			return nil
		}
	} else {
		err = fmt.Errorf("%w: %w", errHetzner, err)
	}
	if cleanupErr := s.deleteVPSServers(ctx, daemon, server.ID); cleanupErr != nil {
		s.log.Error("delete the server of a VPS whose creation failed; delete it by hand", "daemon", daemon, "error", cleanupErr)
	}
	return err
}

// deleteVPSServers deletes server, unless it is 0, and every other
// server labelled as daemon's VPS. A server already gone is no error.
func (s *Server) deleteVPSServers(ctx context.Context, daemon protocol.DaemonID, server hetzner.ServerID) error {
	client := s.provisioning.Hetzner
	servers, err := client.Servers(ctx, "orchestrator=1,daemon="+string(daemon))
	if err != nil {
		return err
	}
	ids := make([]hetzner.ServerID, 0, len(servers)+1)
	for _, found := range servers {
		ids = append(ids, found.ID)
	}
	if server != 0 && !slices.Contains(ids, server) {
		ids = append(ids, server)
	}
	for _, id := range ids {
		if err := client.DeleteServer(ctx, id); err != nil && !errors.Is(err, hetzner.ErrNotFound) {
			return err
		}
	}
	return nil
}

// destroy deletes the VPS daemon runs on and records it destroyed. It
// is destroying while Hetzner is asked, and stays so if Hetzner fails,
// for the owner to try again.
func (s *Server) destroy(ctx context.Context, daemon protocol.DaemonID) (vps, error) {
	if s.provisioning == nil {
		return vps{}, errProvisioningOff
	}
	v, err := s.store.vpsOf(ctx, daemon)
	if err != nil {
		return vps{}, err
	}
	if v.State == vpsDestroyed {
		return vps{}, fmt.Errorf("daemon %q: %w", daemon, errVPSDestroyed)
	}
	if err := s.store.setVPSState(ctx, daemon, vpsDestroying); err != nil {
		return vps{}, err
	}
	s.daemonChanged(daemon)
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hetznerTimeout)
	defer cancel()
	if err := s.deleteVPSServers(callCtx, daemon, v.ServerID); err != nil {
		return vps{}, fmt.Errorf("destroy VPS %q: %w: %w", daemon, errHetzner, err)
	}
	if err := s.store.setVPSState(callCtx, daemon, vpsDestroyed); err != nil {
		return vps{}, err
	}
	s.log.Info("destroyed VPS", "daemon", daemon, "server", v.ServerID)
	return s.store.vpsOf(callCtx, daemon)
}

// vpsView is v as the owner sees it.
func (s *Server) vpsView(v vps, connected []protocol.DaemonID) vpsView {
	view := vpsView(v)
	if v.State == vpsEnrolled && slices.Contains(connected, v.Daemon) {
		view.State = vpsConnected
	}
	return view
}

// provisionStatus is the status an owner's provisioning request gets
// for err.
func provisionStatus(err error) int {
	switch {
	case errors.Is(err, errProvisioningOff), errors.Is(err, errUnknownVPS):
		return http.StatusNotFound
	case errors.Is(err, errVPSDestroyed):
		return http.StatusConflict
	case errors.Is(err, errHetzner):
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

// writeProvisionError answers an owner API request that err failed.
func (s *Server) writeProvisionError(w http.ResponseWriter, err error) {
	status := provisionStatus(err)
	if status == http.StatusInternalServerError {
		s.internalError(w, err)
		return
	}
	http.Error(w, err.Error(), status)
}

// postProvision creates a VPS and answers 201 with it.
func (s *Server) postProvision(w http.ResponseWriter, r *http.Request) {
	v, err := s.provision(r.Context())
	if err != nil {
		s.writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.vpsView(v, s.connectedDaemons()))
}

// postDestroy destroys the VPS the path's daemon runs on, and answers
// with it.
func (s *Server) postDestroy(w http.ResponseWriter, r *http.Request) {
	daemon, ok := daemonFromPath(w, r)
	if !ok {
		return
	}
	v, err := s.destroy(r.Context(), daemon)
	if err != nil {
		s.writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.vpsView(v, s.connectedDaemons()))
}

// getVPSes lists every VPS the server provisioned, destroyed ones
// included, oldest first.
func (s *Server) getVPSes(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.vpses(r.Context(), true)
	if err != nil {
		s.internalError(w, err)
		return
	}
	connected := s.connectedDaemons()
	views := make([]vpsView, len(list))
	for idx, v := range list {
		views[idx] = s.vpsView(v, connected)
	}
	writeJSON(w, http.StatusOK, views)
}
