package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/component"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// The owner logs a daemon's harness in from the server
// (docs/adr/2026-10-10-harness-login.md): the server sends the daemon a
// login command, the daemon reports the URL to authorise at, the owner
// gives the code the authorisation shows, and the daemon reports how the
// login ended. The server keeps where each daemon's latest login is in
// memory only: a login lives no longer than the daemon's login process,
// and the owner starts another after a restart of either.

var (
	// errNotConnected refuses a login for a daemon with no command
	// stream open, which would start whenever it next connects.
	errNotConnected = errors.New("the daemon is not connected")
	// errNoLoginWaiting refuses a code when no login waits for one.
	errNoLoginWaiting = errors.New("no login of the daemon waits for a code; start one")
	// errNoCode refuses an empty code.
	errNoCode = errors.New("the code is empty")
)

// maxLoginCode bounds the code the owner pastes.
const maxLoginCode = 4096

// loginPhase is how far a daemon's latest login has got.
type loginPhase string

const (
	// loginRequested: the login command is issued; the daemon has not
	// reported the URL.
	loginRequested loginPhase = "requested"
	// loginStarted: the login waits for the owner's code at URL.
	loginStarted loginPhase = "started"
	// loginCodeSent: the code is issued; the daemon has not reported
	// how the login ended.
	loginCodeSent loginPhase = "code_sent"
	// loginFinished: the login ended, OK or with Error.
	loginFinished loginPhase = "finished"
)

// loginView is a daemon's latest login, as the owner API reports it.
// Command is the id of the login command that started it.
type loginView struct {
	Command uint64     `json:"command"`
	Phase   loginPhase `json:"phase"`
	URL     string     `json:"url,omitempty"`
	OK      bool       `json:"ok"`
	Error   string     `json:"error,omitempty"`
	At      time.Time  `json:"at"`
}

// issueDaemonCommand appends a command to daemon itself, one of the
// kinds protocol.CommandKind.IsDaemons names, to its log. A daemon the
// server has not seen gets errUnknownDaemon.
func (s *Store) issueDaemonCommand(ctx context.Context, daemon protocol.DaemonID, kind protocol.CommandKind, payload json.RawMessage) (protocol.Command, error) {
	if !kind.IsDaemons() {
		return protocol.Command{}, fmt.Errorf("%s is a task's command", kind)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("issue %s: %w", kind, err)
	}
	defer tx.Rollback()
	var known bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM daemons WHERE id = ?)`, string(daemon)).Scan(&known); err != nil {
		return protocol.Command{}, fmt.Errorf("look up daemon %q: %w", daemon, err)
	}
	if !known {
		return protocol.Command{}, fmt.Errorf("%w: %q", errUnknownDaemon, daemon)
	}
	// A host_keys command replaces the daemon's earlier ones, so that the
	// log keeps the latest only.
	if kind == protocol.CommandHostKeys {
		if _, err := tx.ExecContext(ctx, `DELETE FROM commands WHERE daemon_id = ? AND kind = ?`, string(daemon), string(kind)); err != nil {
			return protocol.Command{}, fmt.Errorf("replace the %s of daemon %q: %w", kind, daemon, err)
		}
	}
	command := protocol.Command{DaemonID: daemon, Kind: kind, Time: time.Now().UTC(), Payload: payload}
	var stored any
	if payload != nil {
		stored = string(payload)
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO commands (daemon_id, kind, time, payload) VALUES (?, ?, ?, ?) RETURNING id`,
		string(daemon), string(kind), formatTime(command.Time), stored).Scan(&id)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("issue %s to daemon %q: %w", kind, daemon, err)
	}
	command.ID = uint64(id)
	if err := tx.Commit(); err != nil {
		return protocol.Command{}, fmt.Errorf("issue %s: %w", kind, err)
	}
	s.publish(&effects{issued: []protocol.Command{command}})
	return command, nil
}

// forgetLoginCodes blanks the codes of daemon's login_code commands. A
// code is good once, for the login process that asked for it, so it is
// worthless once that has ended; it is blanked all the same, so that the
// server keeps nothing of a login.
func (s *Store) forgetLoginCodes(ctx context.Context, daemon protocol.DaemonID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE commands SET payload = json_set(payload, '$.code', '')
		WHERE daemon_id = ? AND kind = ? AND json_extract(payload, '$.code') <> ''`,
		string(daemon), string(protocol.CommandLoginCode))
	if err != nil {
		return fmt.Errorf("forget the login codes of daemon %q: %w", daemon, err)
	}
	return nil
}

// loginOf returns daemon's latest login, and false when it has had none
// since the server started.
func (s *Server) loginOf(daemon protocol.DaemonID) (loginView, bool) {
	s.loginsMu.Lock()
	defer s.loginsMu.Unlock()
	login, ok := s.logins[daemon]
	return login, ok
}

// requestLogin sends daemon a login command, which ends a login it runs
// already.
func (s *Server) requestLogin(ctx context.Context, daemon protocol.DaemonID) (loginView, error) {
	if !slices.Contains(s.connectedDaemons(), daemon) {
		return loginView{}, errNotConnected
	}
	command, err := s.store.issueDaemonCommand(ctx, daemon, protocol.CommandLogin, nil)
	if err != nil {
		return loginView{}, err
	}
	login := loginView{Command: command.ID, Phase: loginRequested, At: command.Time}
	s.loginsMu.Lock()
	s.logins[daemon] = login
	s.loginsMu.Unlock()
	s.daemonChanged(daemon)
	return login, nil
}

// submitLoginCode sends daemon the code for its login that waits for
// one.
func (s *Server) submitLoginCode(ctx context.Context, daemon protocol.DaemonID, code string) (loginView, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return loginView{}, errNoCode
	}
	s.loginsMu.Lock()
	login, ok := s.logins[daemon]
	s.loginsMu.Unlock()
	if !ok || login.Phase != loginStarted {
		return loginView{}, errNoLoginWaiting
	}
	payload, err := json.Marshal(protocol.LoginCode{Login: login.Command, Code: code})
	if err != nil {
		return loginView{}, err
	}
	command, err := s.store.issueDaemonCommand(ctx, daemon, protocol.CommandLoginCode, payload)
	if err != nil {
		return loginView{}, err
	}
	s.loginsMu.Lock()
	if current := s.logins[daemon]; current.Command == login.Command && current.Phase == loginStarted {
		login.Phase, login.At = loginCodeSent, command.Time
		s.logins[daemon] = login
	}
	s.loginsMu.Unlock()
	s.daemonChanged(daemon)
	return login, nil
}

// postLoginEvent records how the path's daemon's login goes, and
// answers 204. An event of a login older than the latest is dropped.
func (s *Server) postLoginEvent(w http.ResponseWriter, r *http.Request) {
	daemon, ok := daemonFromPath(w, r)
	if !ok {
		return
	}
	var event protocol.LoginEvent
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, maxFactsBytes), &event); err != nil {
		http.Error(w, "decode login event: "+err.Error(), http.StatusBadRequest)
		return
	}
	if event.Kind != protocol.KindLoginStarted && event.Kind != protocol.KindLoginFinished || event.Login == 0 {
		http.Error(w, fmt.Sprintf("login event: kind %q, login %d", event.Kind, event.Login), http.StatusBadRequest)
		return
	}
	if event.Kind == protocol.KindLoginFinished {
		if err := s.store.forgetLoginCodes(r.Context(), daemon); err != nil {
			s.internalError(w, err)
			return
		}
	}
	s.loginsMu.Lock()
	login, known := s.logins[daemon]
	current := !known || event.Login >= login.Command
	if current {
		login = loginView{Command: event.Login, At: time.Now()}
		switch event.Kind {
		case protocol.KindLoginStarted:
			login.Phase, login.URL = loginStarted, event.URL
		case protocol.KindLoginFinished:
			login.Phase, login.OK, login.Error = loginFinished, event.OK, event.Error
		}
		s.logins[daemon] = login
	}
	s.loginsMu.Unlock()
	if current {
		s.log.Info("login", "daemon", daemon, "login", event.Login, "kind", event.Kind, "ok", event.OK, "error", event.Error)
		s.daemonChanged(daemon)
	}
	w.WriteHeader(http.StatusNoContent)
}

// loginStatus maps a login error to the owner API's status for it.
func loginStatus(err error) int {
	switch {
	case errors.Is(err, errUnknownDaemon):
		return http.StatusNotFound
	case errors.Is(err, errNotConnected), errors.Is(err, errNoLoginWaiting):
		return http.StatusConflict
	case errors.Is(err, errNoCode):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// postDaemonLogin starts a login of the path's daemon and answers 202 with
// the login.
func (s *Server) postDaemonLogin(w http.ResponseWriter, r *http.Request) {
	daemon, ok := s.daemonSummaryOf(w, r)
	if !ok {
		return
	}
	login, err := s.requestLogin(r.Context(), daemon.ID)
	if status := loginStatus(err); err != nil && status != http.StatusInternalServerError {
		http.Error(w, err.Error(), status)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, login)
}

// postDaemonLoginCode gives the path's daemon's login the code in the body,
// {"code": "..."}, and answers 202 with the login.
func (s *Server) postDaemonLoginCode(w http.ResponseWriter, r *http.Request) {
	daemon, ok := s.daemonSummaryOf(w, r)
	if !ok {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, maxLoginCode), &body); err != nil {
		http.Error(w, "decode code: "+err.Error(), http.StatusBadRequest)
		return
	}
	login, err := s.submitLoginCode(r.Context(), daemon.ID, body.Code)
	if status := loginStatus(err); err != nil && status != http.StatusInternalServerError {
		http.Error(w, err.Error(), status)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, login)
}

// daemonLogin is the daemon page's login section for daemon, with
// problem, when set, saying why the owner's last request was refused.
func (s *Server) daemonLogin(daemon daemonSummary, problem string) component.DaemonLoginView {
	view := component.DaemonLoginView{
		ID: string(daemon.ID), Connected: slices.Contains(s.connectedDaemons(), daemon.ID),
		Login: daemon.Facts[protocol.FactLogin], Method: daemon.Facts[protocol.FactLoginMethod], Account: daemon.Facts[protocol.FactAccount],
		Problem: problem,
	}
	if login, ok := s.loginOf(daemon.ID); ok {
		view.Phase, view.URL, view.OK, view.Error = string(login.Phase), login.URL, login.OK, login.Error
	}
	return view
}

// postLoginForm starts a login from the daemon page.
func (s *Server) postLoginForm(w http.ResponseWriter, r *http.Request) {
	daemon, ok := s.daemonSummaryOf(w, r)
	if !ok {
		return
	}
	_, err := s.requestLogin(r.Context(), daemon.ID)
	s.answerLoginForm(w, r, daemon, err)
}

// postLoginCodeForm gives the daemon's login the code the form carries.
func (s *Server) postLoginCodeForm(w http.ResponseWriter, r *http.Request) {
	daemon, ok := s.daemonSummaryOf(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginCode)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "read form: "+err.Error(), http.StatusBadRequest)
		return
	}
	_, err := s.submitLoginCode(r.Context(), daemon.ID, r.PostForm.Get("code"))
	s.answerLoginForm(w, r, daemon, err)
}

// answerLoginForm answers a login form: htmx gets the login section,
// and a browser without JavaScript the daemon page, which on a refusal
// says why.
func (s *Server) answerLoginForm(w http.ResponseWriter, r *http.Request, daemon daemonSummary, err error) {
	status := loginStatus(err)
	if err != nil && status == http.StatusInternalServerError {
		s.internalError(w, err)
		return
	}
	problem := ""
	if err != nil {
		problem = "Not done: " + err.Error() + "."
	}
	if fromHTMX(r) {
		s.writeHTML(w, http.StatusOK, component.OutOfBand(component.RegionDaemonLogin, component.DaemonLogin(s.daemonLogin(daemon, problem))))
		return
	}
	if err != nil {
		s.writeDaemonPage(w, status, daemon, daemon.Labels.String(), "", problem)
		return
	}
	redirect(w, r, "/daemons/"+string(daemon.ID)+"#login")
}

// streamDaemon sends a daemon page its login section as a server-sent
// event when it connects, and again whenever the daemon's login, facts
// or connection change.
func (s *Server) streamDaemon(w http.ResponseWriter, r *http.Request) {
	id, ok := daemonFromPath(w, r)
	if !ok {
		return
	}
	changed, stop := s.WatchDaemon(id)
	defer stop()
	if _, ok := s.daemonSummaryOf(w, r); !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	sender := http.NewResponseController(w)
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()
	for sent := 1; ; sent++ {
		daemons, err := s.store.daemons(r.Context())
		if err != nil {
			if r.Context().Err() == nil {
				s.log.Error("read daemon for its page", "daemon", id, "error", err)
			}
			return
		}
		for _, daemon := range daemons {
			if daemon.ID == id {
				if err := writeEvent(w, fmt.Sprint(sent), component.DaemonLogin(s.daemonLogin(daemon, ""))); err != nil {
					return
				}
			}
		}
		if err := sender.Flush(); err != nil {
			return
		}

	wait:
		for {
			select {
			case <-changed:
				break wait
			case <-keepalive.C:
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				if err := sender.Flush(); err != nil {
					return
				}
			case <-s.ended:
				return
			case <-r.Context().Done():
				return
			}
		}
	}
}
