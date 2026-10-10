package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/runas"
)

// loginHarness is a fakeHarness that reports a login the test sets.
type loginHarness struct {
	*fakeHarness
	mu     sync.Mutex
	status harness.LoginStatus
	err    error
	users  []runas.User
}

func (h *loginHarness) LoginStatus(_ context.Context, user runas.User) (harness.LoginStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.users = append(h.users, user)
	return h.status, h.err
}

func (h *loginHarness) set(status harness.LoginStatus, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status, h.err = status, err
}

// reportedFacts returns the facts the server holds of the test daemon.
func reportedFacts(t *testing.T, srv *serverFixture) map[string]string {
	t.Helper()
	var view struct {
		Facts map[string]string `json:"facts"`
	}
	status, body := srv.try(t, http.MethodGet, "/v1/daemons/"+string(testDaemon), nil)
	if status != http.StatusOK || json.Unmarshal(body, &view) != nil {
		return nil
	}
	return view.Facts
}

func loginFacts(facts map[string]string) [3]string {
	return [3]string{facts[protocol.FactLogin], facts[protocol.FactLoginMethod], facts[protocol.FactAccount]}
}

func TestGivenHarnessThatReportsItsLoginWhenTheDaemonConnectsAndTheLoginLapsesThenTheFactsFollow(t *testing.T) {
	srv := startServer(t)
	h := &loginHarness{status: harness.LoginStatus{LoggedIn: true, Method: "claude.ai", Account: "owner@example.com/org-1"}}
	user := runas.User{Name: "orch-agent"}
	runDaemonAs(t, srv.url, t.TempDir(), func(cfg *Config) {
		h.fakeHarness = cfg.Harness.(*fakeHarness)
		cfg.Harness = h
		cfg.HarnessUser = user
		cfg.WorkspaceDir = t.TempDir()
		cfg.loginInterval = 20 * time.Millisecond
	})

	eventually(t, "the login reported at connect", func() bool {
		return loginFacts(reportedFacts(t, srv)) == [3]string{"yes", "claude.ai", "owner@example.com/org-1"}
	})
	h.set(harness.LoginStatus{Method: "none"}, nil)
	eventually(t, "the lapsed login reported on the timer", func() bool {
		return loginFacts(reportedFacts(t, srv)) == [3]string{"no", "none", ""}
	})
	h.set(harness.LoginStatus{}, errors.New("claude auth status: exit status 2"))
	time.Sleep(100 * time.Millisecond)
	if got := loginFacts(reportedFacts(t, srv)); got != [3]string{"no", "none", ""} {
		t.Errorf("facts after a failed read = %q, want the last read kept", got)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, got := range h.users {
		if got != user {
			t.Fatalf("the login was read as %+v, want the harness user", got)
		}
	}
}

func TestGivenAccountThatIsNotALabelValueWhenReportedThenItIsLeftOutAndTheOtherFactsStay(t *testing.T) {
	srv := startServer(t)
	h := &loginHarness{status: harness.LoginStatus{LoggedIn: true, Method: "claude.ai", Account: "two words"}}
	runDaemonAs(t, srv.url, t.TempDir(), func(cfg *Config) {
		h.fakeHarness = cfg.Harness.(*fakeHarness)
		cfg.Harness = h
	})

	eventually(t, "the login reported without the account", func() bool {
		facts := reportedFacts(t, srv)
		return loginFacts(facts) == [3]string{"yes", "claude.ai", ""} && facts[protocol.FactOS] != ""
	})
}

func TestGivenHarnessThatCannotReportItsLoginWhenTheDaemonConnectsThenNoLoginFactIsReported(t *testing.T) {
	srv := startServer(t)
	runDaemonAs(t, srv.url, t.TempDir(), func(*Config) {})

	eventually(t, "the facts reported", func() bool { return reportedFacts(t, srv)[protocol.FactOS] != "" })
	if got := loginFacts(reportedFacts(t, srv)); got != [3]string{} {
		t.Errorf("login facts = %q, want none", got)
	}
}
