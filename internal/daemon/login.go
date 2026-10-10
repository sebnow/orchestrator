package daemon

import (
	"context"
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
