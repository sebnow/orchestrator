package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/component"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// maxFactsBytes bounds a daemon's facts, a handful of short strings.
const maxFactsBytes = 64 << 10

// setFacts records that daemon reported facts, in place of those it
// reported before, and that it was seen.
func (s *Store) setFacts(ctx context.Context, daemon protocol.DaemonID, facts Labels) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record facts of daemon %q: %w", daemon, err)
	}
	defer tx.Rollback()
	if err := seeDaemon(ctx, tx, daemon, s.now(), nil); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE daemons SET facts = ? WHERE id = ?`, encodeLabels(facts), string(daemon)); err != nil {
		return fmt.Errorf("record facts of daemon %q: %w", daemon, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record facts of daemon %q: %w", daemon, err)
	}
	s.publish(&effects{reschedule: true})
	return nil
}

// setLabels replaces the owner's labels on daemon, which must have been
// seen, or returns errUnknownDaemon.
func (s *Store) setLabels(ctx context.Context, daemon protocol.DaemonID, labels Labels) error {
	result, err := s.db.ExecContext(ctx, `UPDATE daemons SET labels = ? WHERE id = ?`, encodeLabels(labels), string(daemon))
	if err != nil {
		return fmt.Errorf("set labels of daemon %q: %w", daemon, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return fmt.Errorf("set labels of daemon %q: %w", daemon, err)
		}
		return fmt.Errorf("%w: %q", errUnknownDaemon, daemon)
	}
	s.publish(&effects{reschedule: true})
	return nil
}

// putFacts records the facts the path's daemon reports about its machine
// (docs/adr/2026-10-09-agents-and-placement.md) and answers 204.
func (s *Server) putFacts(w http.ResponseWriter, r *http.Request) {
	daemon, ok := daemonFromPath(w, r)
	if !ok {
		return
	}
	var facts protocol.Facts
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, maxFactsBytes), &facts); err != nil {
		http.Error(w, "decode facts: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := Labels(facts).Validate(); err != nil {
		http.Error(w, "facts: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.setFacts(r.Context(), daemon, Labels(facts)); err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// daemonSummaryOf returns the daemon the path names, or writes why not
// and reports false.
func (s *Server) daemonSummaryOf(w http.ResponseWriter, r *http.Request) (daemonSummary, bool) {
	id, ok := daemonFromPath(w, r)
	if !ok {
		return daemonSummary{}, false
	}
	daemons, err := s.store.daemons(r.Context())
	if err != nil {
		s.internalError(w, err)
		return daemonSummary{}, false
	}
	for _, daemon := range daemons {
		if daemon.ID == id {
			return daemon, true
		}
	}
	http.Error(w, fmt.Sprintf("%v: %q", errUnknownDaemon, id), http.StatusNotFound)
	return daemonSummary{}, false
}

// getDaemonPage shows a daemon's facts and labels, with the form that
// sets its labels.
func (s *Server) getDaemonPage(w http.ResponseWriter, r *http.Request) {
	daemon, ok := s.daemonSummaryOf(w, r)
	if !ok {
		return
	}
	s.writeDaemonPage(w, http.StatusOK, daemon, daemon.Labels.String(), "")
}

func (s *Server) writeDaemonPage(w http.ResponseWriter, status int, daemon daemonSummary, labels, problem string) {
	s.writeHTML(w, status, component.Page("Daemon "+string(daemon.ID),
		component.Section("Daemon "+string(daemon.ID), component.DaemonLabels(string(daemon.ID), daemon.Facts, daemon.Labels, Merge(daemon.Facts, daemon.Labels))),
		component.Section("Labels", component.LabelsForm(string(daemon.ID), labels, problem)),
		component.Section("Push key", component.DaemonSSHKey(sshKeyLine(daemon))),
	))
}

// sshKeyLine is the authorized_keys line of the key daemon reported in
// its facts, with the comment the daemon writes in its own public key
// file, or "" when it reported none.
func sshKeyLine(daemon daemonSummary) string {
	key := daemon.Facts[protocol.FactSSHPublicKey]
	if key == "" {
		return ""
	}
	return "ssh-ed25519 " + key + " orchestrator@" + string(daemon.ID)
}

// postLabelsForm replaces a daemon's labels with those the form gives
// and returns the owner to the daemon's page.
func (s *Server) postLabelsForm(w http.ResponseWriter, r *http.Request) {
	daemon, ok := s.daemonSummaryOf(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxOwnerRequestBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "read form: "+err.Error(), http.StatusBadRequest)
		return
	}
	text := r.PostForm.Get("labels")
	labels, err := ParseLabels(text)
	if err != nil {
		s.writeDaemonPage(w, http.StatusUnprocessableEntity, daemon, text, "The labels were not saved: "+strings.TrimPrefix(err.Error(), errInvalidLabels.Error()+": ")+".")
		return
	}
	err = s.store.setLabels(r.Context(), daemon.ID, labels)
	if errors.Is(err, errUnknownDaemon) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	redirect(w, r, "/daemons/"+string(daemon.ID))
}

// daemonView is a daemon as the owner API reports it. Facts are what the
// daemon last reported, its ssh_public_key fact the bare key blob; the
// key is also given whole, as the authorized_keys line SSHPublicKey, and
// bare, as SSHPublicKeyBlob, both empty when the daemon reported none.
// Slots is the daemon's slot count, its own or the server's default, and
// Running counts the tasks holding one. LostSince is set while the
// server holds the daemon lost.
type daemonView struct {
	ID               protocol.DaemonID `json:"id"`
	Labels           Labels            `json:"labels"`
	Facts            Labels            `json:"facts"`
	SSHPublicKey     string            `json:"ssh_public_key,omitempty"`
	SSHPublicKeyBlob string            `json:"ssh_public_key_blob,omitempty"`
	LastSeen         time.Time         `json:"last_seen"`
	Connected        bool              `json:"connected"`
	Lost             bool              `json:"lost"`
	LostSince        *time.Time        `json:"lost_since,omitempty"`
	Slots            int               `json:"slots"`
	Running          int               `json:"running"`
}

// view is daemon as the owner API reports it, connected or not.
func (s *Server) view(daemon daemonSummary, connected bool) daemonView {
	v := daemonView{
		ID: daemon.ID, Labels: daemon.Labels, Facts: daemon.Facts, SSHPublicKey: sshKeyLine(daemon),
		SSHPublicKeyBlob: daemon.Facts[protocol.FactSSHPublicKey], LastSeen: daemon.LastSeen, Connected: connected,
		Lost: daemon.LostAt != nil, LostSince: daemon.LostAt, Slots: s.sched.policy.SlotsPerDaemon, Running: daemon.InUse,
	}
	if daemon.Slots != nil {
		v.Slots = *daemon.Slots
	}
	return v
}

// getDaemons lists every daemon the server has seen, by id.
func (s *Server) getDaemons(w http.ResponseWriter, r *http.Request) {
	daemons, err := s.store.daemons(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	connected := s.connectedDaemons()
	views := make([]daemonView, len(daemons))
	for idx, daemon := range daemons {
		views[idx] = s.view(daemon, slices.Contains(connected, daemon.ID))
	}
	writeJSON(w, http.StatusOK, views)
}

// getDaemon returns the daemon the path names, or 404 for one the server
// has not seen.
func (s *Server) getDaemon(w http.ResponseWriter, r *http.Request) {
	daemon, ok := s.daemonSummaryOf(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.view(daemon, slices.Contains(s.connectedDaemons(), daemon.ID)))
}
