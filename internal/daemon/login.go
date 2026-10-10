package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

const (
	// loginRefresh is how often the daemon reads its harness's login
	// between connections, so that a login that lapses shows
	// (docs/adr/2026-10-10-harness-login.md).
	loginRefresh = 10 * time.Minute
	// loginStatusTimeout bounds reading the login.
	loginStatusTimeout = 30 * time.Second
	// loginTimeout ends a login that has had no code for this long.
	loginTimeout = 10 * time.Minute
	// loginEventTimeout bounds sending a login event, retries included.
	loginEventTimeout = 30 * time.Second
)

// readLogin reads the harness's login, as the harness user, into the
// login facts, and reports whether they changed. A harness that cannot
// report its login has none of them. When the login cannot be read the
// facts stay as they were, and the failure is logged. A method or an
// account that is not a valid label value is left out, since the server
// would refuse every fact for it.
func (s *service) readLogin(ctx context.Context) bool {
	login, ok := s.cfg.Harness.(harness.Login)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, loginStatusTimeout)
	defer cancel()
	status, err := login.LoginStatus(ctx, s.cfg.HarnessUser)
	if err != nil {
		s.log.Warn("read the harness's login", "error", err)
		return false
	}
	changed := s.updateFacts(func(facts protocol.Facts) {
		delete(facts, protocol.FactLoginMethod)
		delete(facts, protocol.FactAccount)
		facts[protocol.FactLogin] = protocol.LoginNo
		if status.LoggedIn {
			facts[protocol.FactLogin] = protocol.LoginYes
		}
		for key, value := range map[string]string{protocol.FactLoginMethod: status.Method, protocol.FactAccount: status.Account} {
			switch {
			case protocol.ValidLabelValue(value):
				facts[key] = value
			case value != "":
				s.log.Warn("the harness's login names what a fact cannot hold; left out", "fact", key, "value", value)
			}
		}
	})
	if changed {
		facts := s.currentFacts()
		s.log.Info("login", "login", facts[protocol.FactLogin], "method", facts[protocol.FactLoginMethod], "account", facts[protocol.FactAccount])
	}
	return changed
}

// refreshLogin reads the harness's login and sends the server the
// daemon's facts when that changed them. A report that fails is logged;
// the next connection reports the facts again.
func (s *service) refreshLogin(ctx context.Context) {
	if !s.readLogin(ctx) {
		return
	}
	if err := s.reportFacts(ctx); err != nil {
		s.log.Warn("report facts", "error", err)
	}
}

// watchLogin refreshes the login facts every loginInterval until ctx
// ends.
func (s *service) watchLogin(ctx context.Context) {
	if _, ok := s.cfg.Harness.(harness.Login); !ok {
		return
	}
	interval := s.cfg.loginInterval
	if interval <= 0 {
		interval = loginRefresh
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.refreshLogin(ctx)
	}
}

// loginRun is the login the daemon runs for the CommandLogin command.
// codes carries the code the owner gives it; done is closed once it has
// ended and been reported.
type loginRun struct {
	command uint64
	codes   chan string
	cancel  context.CancelFunc
	done    chan struct{}
}

// applyDaemonCommand applies a command to the daemon itself, in the
// background, so that the command stream is not held up.
func (s *service) applyDaemonCommand(command protocol.Command) {
	switch command.Kind {
	case protocol.CommandLogin:
		s.startLogin(command.ID)
	case protocol.CommandLoginCode:
		code, err := decodePayload[protocol.LoginCode](command)
		if err != nil {
			s.log.Warn("command not applied", "command", command.ID, "kind", command.Kind, "error", err)
			return
		}
		s.giveLoginCode(code)
	default:
		s.log.Warn("command not applied", "command", command.ID, "kind", command.Kind, "error", "unknown command kind")
	}
}

// loginWait is how long a login waits for its code.
func (s *service) loginWait() time.Duration {
	if s.cfg.loginWait > 0 {
		return s.cfg.loginWait
	}
	return loginTimeout
}

// startLogin starts the harness's login for the CommandLogin command,
// ending the login that runs already, if one does: one login runs at a
// time, and the owner's latest is the one they look at.
func (s *service) startLogin(command uint64) {
	login, ok := s.cfg.Harness.(harness.Login)
	if !ok {
		s.workers.Go(func() {
			s.sendLoginEvent(protocol.LoginEvent{Kind: protocol.KindLoginFinished, Login: command, Error: "the daemon's harness cannot be logged in from the server"})
		})
		return
	}
	ctx, cancel := context.WithTimeout(s.stopping, s.loginWait())
	run := &loginRun{command: command, codes: make(chan string, 1), cancel: cancel, done: make(chan struct{})}
	s.loginMu.Lock()
	previous := s.login
	s.login = run
	s.loginMu.Unlock()
	s.log.Info("login started", "command", command)
	s.workers.Go(func() {
		if previous != nil {
			previous.cancel()
			<-previous.done
		}
		s.runLogin(ctx, login, run)
	})
}

// runLogin runs the login until the owner's code settles it, the
// harness completes it without one, or ctx ends, and reports how it went.
func (s *service) runLogin(ctx context.Context, login harness.Login, run *loginRun) {
	defer close(run.done)
	defer run.cancel()
	defer func() {
		s.loginMu.Lock()
		if s.login == run {
			s.login = nil
		}
		s.loginMu.Unlock()
	}()
	session, err := login.StartLogin(ctx, s.cfg.HarnessUser)
	if err != nil {
		s.finishLogin(run.command, s.loginEnded(ctx, err))
		return
	}
	defer session.Close()
	s.sendLoginEvent(protocol.LoginEvent{Kind: protocol.KindLoginStarted, Login: run.command, URL: session.URL()})
	var outcome error
	select {
	case code := <-run.codes:
		outcome = session.Submit(code)
	case <-session.Done():
		outcome = session.Err()
	case <-ctx.Done():
		session.Close()
		outcome = s.loginEnded(ctx, ctx.Err())
	}
	s.finishLogin(run.command, outcome)
}

// loginEnded says why a login ended once ctx, its context, has: no code
// came in time, a newer login replaced it, or the daemon is shutting
// down; err when ctx has not ended.
func (s *service) loginEnded(ctx context.Context, err error) error {
	switch {
	case ctx.Err() == nil:
		return err
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("no code came within %s, so the login was ended", s.loginWait())
	case s.stopping.Err() != nil:
		return errors.New("the daemon shut down during the login")
	}
	return errors.New("a newer login replaced this one")
}

// finishLogin reads the harness's login afresh, reporting the facts
// when they changed, and then reports how the login of command ended:
// ok when outcome is nil and the harness reports itself logged in.
func (s *service) finishLogin(command uint64, outcome error) {
	ctx, cancel := context.WithTimeout(context.Background(), loginStatusTimeout)
	defer cancel()
	s.refreshLogin(ctx)
	event := protocol.LoginEvent{Kind: protocol.KindLoginFinished, Login: command}
	switch {
	case outcome != nil:
		event.Error = outcome.Error()
	case s.currentFacts()[protocol.FactLogin] != protocol.LoginYes:
		event.Error = "the login ended, but the harness does not report itself logged in"
	default:
		event.OK = true
	}
	s.log.Info("login finished", "command", command, "ok", event.OK, "error", event.Error)
	s.sendLoginEvent(event)
}

// giveLoginCode gives code to the login it is for. A code for a login
// that no longer runs is answered with that login finished, as it is.
func (s *service) giveLoginCode(code protocol.LoginCode) {
	s.loginMu.Lock()
	run := s.login
	s.loginMu.Unlock()
	if run == nil || run.command != code.Login {
		s.workers.Go(func() {
			s.sendLoginEvent(protocol.LoginEvent{Kind: protocol.KindLoginFinished, Login: code.Login, Error: "that login no longer runs; start another"})
		})
		return
	}
	select {
	case run.codes <- code.Code:
	default:
		s.log.Warn("a second code for a login; ignored", "login", code.Login)
	}
}

// sendLoginEvent POSTs event to the server, trying again with backoff
// for up to loginEventTimeout. A login event is not journaled: one that
// cannot be sent is logged, and the owner starts the login again.
func (s *service) sendLoginEvent(event protocol.LoginEvent) {
	event.Time = time.Now()
	body, err := json.Marshal(event)
	if err != nil {
		s.log.Error("encode login event", "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginEventTimeout)
	defer cancel()
	b := backoff{min: s.cfg.MinBackoff, max: s.cfg.MaxBackoff}
	for {
		err = s.postLoginEvent(ctx, body)
		if err == nil {
			return
		}
		if b.wait(ctx) != nil {
			s.log.Warn("send login event", "kind", event.Kind, "login", event.Login, "error", err)
			return
		}
	}
}

func (s *service) postLoginEvent(ctx context.Context, body []byte) error {
	target := s.cfg.Server.JoinPath("v1", "daemons", string(s.cfg.ID), "login-events")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}
