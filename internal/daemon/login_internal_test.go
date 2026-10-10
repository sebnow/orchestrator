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

// loginHarness is a fakeHarness that reports a login the test sets,
// and whose logins take the code "good" and refuse any other.
type loginHarness struct {
	*fakeHarness
	mu       sync.Mutex
	status   harness.LoginStatus
	err      error
	users    []runas.User
	sessions chan *fakeLoginSession
}

func (h *loginHarness) StartLogin(ctx context.Context, user runas.User) (harness.LoginSession, error) {
	session := &fakeLoginSession{url: "https://claude.com/cai/oauth/authorize?state=" + time.Now().Format("150405.000000"), done: make(chan struct{}), harness: h}
	go func() {
		<-ctx.Done()
		session.end(ctx.Err())
	}()
	if h.sessions != nil {
		h.sessions <- session
	}
	return session, nil
}

// fakeLoginSession is a login of a loginHarness.
type fakeLoginSession struct {
	url     string
	harness *loginHarness
	once    sync.Once
	done    chan struct{}
	err     error
}

func (s *fakeLoginSession) URL() string { return s.url }

func (s *fakeLoginSession) Submit(code string) error {
	if code != "good" {
		s.end(errors.New("Login failed: Request failed with status code 400"))
		return s.Err()
	}
	s.harness.set(harness.LoginStatus{LoggedIn: true, Method: "claude.ai", Account: "owner@example.com/org-1"}, nil)
	s.end(nil)
	return s.Err()
}

func (s *fakeLoginSession) end(err error) {
	s.once.Do(func() {
		s.err = err
		close(s.done)
	})
}

func (s *fakeLoginSession) Done() <-chan struct{} { return s.done }

func (s *fakeLoginSession) Err() error {
	<-s.done
	return s.err
}

func (s *fakeLoginSession) Close() { s.end(errors.New("closed")) }

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

// daemonLogin is the daemon's latest login as the owner API reports it.
type daemonLogin struct {
	Command uint64 `json:"command"`
	Phase   string `json:"phase"`
	URL     string `json:"url"`
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
}

func latestLogin(t *testing.T, srv *serverFixture) daemonLogin {
	t.Helper()
	var view struct {
		Login *daemonLogin `json:"login"`
	}
	srv.call(t, http.MethodGet, "/v1/daemons/"+string(testDaemon), nil, &view)
	if view.Login == nil {
		return daemonLogin{}
	}
	return *view.Login
}

// loggedOutDaemon runs a daemon whose harness is not logged in, once it
// has connected, with adjust applied to its configuration.
func loggedOutDaemon(t *testing.T, adjust func(*Config)) (*serverFixture, *loginHarness) {
	t.Helper()
	srv := startServer(t)
	h := &loginHarness{status: harness.LoginStatus{Method: "none"}, sessions: make(chan *fakeLoginSession, 4)}
	runDaemonAs(t, srv.url, t.TempDir(), func(cfg *Config) {
		h.fakeHarness = cfg.Harness.(*fakeHarness)
		cfg.Harness = h
		adjust(cfg)
	})
	eventually(t, "the daemon connected, not logged in", func() bool {
		status, _ := srv.try(t, http.MethodPost, "/v1/daemons/"+string(testDaemon)+"/login", nil)
		return status == http.StatusAccepted
	})
	return srv, h
}

func TestGivenLoggedOutDaemonWhenTheOwnerLogsItInWithTheCodeThenTheLoginFinishesOKAndTheFactsSaySo(t *testing.T) {
	srv, h := loggedOutDaemon(t, func(*Config) {})
	session := <-h.sessions

	eventually(t, "the URL reported", func() bool {
		login := latestLogin(t, srv)
		return login.Phase == "started" && login.URL == session.URL()
	})
	srv.call(t, http.MethodPost, "/v1/daemons/"+string(testDaemon)+"/login/code", map[string]string{"code": "good"}, nil)

	eventually(t, "the login finished", func() bool { return latestLogin(t, srv).Phase == "finished" })
	if login := latestLogin(t, srv); !login.OK || login.Error != "" {
		t.Errorf("login = %+v, want ok", login)
	}
	if got := loginFacts(reportedFacts(t, srv)); got != [3]string{"yes", "claude.ai", "owner@example.com/org-1"} {
		t.Errorf("facts = %q, want logged in, reported before the login finished", got)
	}
}

func TestGivenLoginWhenTheCodeIsRefusedThenTheLoginFinishesWithTheHarnesssError(t *testing.T) {
	srv, h := loggedOutDaemon(t, func(*Config) {})
	<-h.sessions
	eventually(t, "the URL reported", func() bool { return latestLogin(t, srv).Phase == "started" })

	srv.call(t, http.MethodPost, "/v1/daemons/"+string(testDaemon)+"/login/code", map[string]string{"code": "wrong"}, nil)

	eventually(t, "the login finished", func() bool { return latestLogin(t, srv).Phase == "finished" })
	if login := latestLogin(t, srv); login.OK || login.Error != "Login failed: Request failed with status code 400" {
		t.Errorf("login = %+v, want the harness's error", login)
	}
	if got := reportedFacts(t, srv)[protocol.FactLogin]; got != "no" {
		t.Errorf("login fact = %q, want no", got)
	}
}

func TestGivenLoginWhenNoCodeComesInTimeThenItIsEndedAndSaysSo(t *testing.T) {
	srv, h := loggedOutDaemon(t, func(cfg *Config) { cfg.loginWait = 200 * time.Millisecond })
	session := <-h.sessions

	eventually(t, "the login finished", func() bool { return latestLogin(t, srv).Phase == "finished" })

	if login := latestLogin(t, srv); login.Error != "no code came within 200ms, so the login was ended" {
		t.Errorf("login = %+v, want the timeout", login)
	}
	select {
	case <-session.Done():
	default:
		t.Error("the harness's login still runs")
	}
}

func TestGivenLoginUnderWayWhenTheOwnerStartsAnotherThenTheFirstEndsAndTheSecondTakesTheCode(t *testing.T) {
	srv, h := loggedOutDaemon(t, func(*Config) {})
	first := <-h.sessions
	eventually(t, "the first URL reported", func() bool { return latestLogin(t, srv).URL == first.URL() })

	srv.call(t, http.MethodPost, "/v1/daemons/"+string(testDaemon)+"/login", nil, nil)
	second := <-h.sessions

	select {
	case <-first.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the first login still runs")
	}
	eventually(t, "the second URL reported", func() bool { return latestLogin(t, srv).URL == second.URL() })
	srv.call(t, http.MethodPost, "/v1/daemons/"+string(testDaemon)+"/login/code", map[string]string{"code": "good"}, nil)
	eventually(t, "the second login finished ok", func() bool { return latestLogin(t, srv).OK })
}

func TestGivenHarnessThatCannotLogInWhenTheOwnerAsksThenTheLoginFinishesSayingSo(t *testing.T) {
	srv := startServer(t)
	runDaemonAs(t, srv.url, t.TempDir(), func(*Config) {})
	eventually(t, "the login requested", func() bool {
		status, _ := srv.try(t, http.MethodPost, "/v1/daemons/"+string(testDaemon)+"/login", nil)
		return status == http.StatusAccepted
	})

	eventually(t, "the login finished", func() bool { return latestLogin(t, srv).Phase == "finished" })
	if login := latestLogin(t, srv); login.Error != "the daemon's harness cannot be logged in from the server" {
		t.Errorf("login = %+v", login)
	}
}
