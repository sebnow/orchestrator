//go:build live

package claude_test

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/runas"
)

// isolatedClaude returns the real claude on PATH with HOME and
// CLAUDE_CONFIG_DIR in a new directory, so that nothing touches the
// machine's own login, and BROWSER set to a script that only records
// that it ran, so that no browser opens. It spends no quota.
func isolatedClaude(t *testing.T) *claude.Harness {
	t.Helper()
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("no claude on PATH")
	}
	home := t.TempDir()
	browser := filepath.Join(home, "browser")
	if err := os.WriteFile(browser, []byte("#!/bin/sh\necho \"$@\" >"+filepath.Join(home, "browser-ran")+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("BROWSER", browser)
	h, err := claude.New(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	status, err := h.LoginStatus(t.Context(), runas.User{})
	if err != nil {
		t.Fatal(err)
	}
	if status.LoggedIn {
		t.Fatalf("the isolated configuration is logged in: %+v", status)
	}
	return h
}

// startRealLogin starts the real login and checks its URL: Claude
// Code's authorisation page, with the manual callback that shows the
// code, a PKCE challenge and a state.
func startRealLogin(t *testing.T, h *claude.Harness) (harness.LoginSession, url.Values) {
	t.Helper()
	session, err := h.StartLogin(t.Context(), runas.User{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	t.Logf("URL: %s", session.URL())
	parsed, err := url.Parse(session.URL())
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Host != "claude.com" || !strings.HasPrefix(parsed.Path, "/cai/oauth/authorize") ||
		query.Get("redirect_uri") != "https://platform.claude.com/oauth/code/callback" ||
		query.Get("code_challenge") == "" || query.Get("state") == "" {
		t.Errorf("URL = %s, want the authorisation page with the manual callback", session.URL())
	}
	if strings.ContainsAny(session.URL(), "\x1b\x07") {
		t.Errorf("URL %q holds escape characters", session.URL())
	}
	return session, query
}

func TestLiveGivenRealLoginWhenAWrongCodeIsGivenThenItReportsAnErrorRatherThanHanging(t *testing.T) {
	h := isolatedClaude(t)
	session, query := startRealLogin(t, h)
	started := time.Now()

	err := session.Submit("orchestratorLiveCheckWrongCode#" + query.Get("state"))

	t.Logf("outcome after %s: %v", time.Since(started).Round(time.Millisecond), err)
	if err == nil {
		t.Fatal("submit = nil, want the wrong code refused")
	}
	select {
	case <-session.Done():
	default:
		t.Error("the login still runs")
	}
}

func TestLiveGivenRealLoginWhenAMalformedCodeIsGivenThenItReportsAnErrorRatherThanHanging(t *testing.T) {
	h := isolatedClaude(t)
	session, _ := startRealLogin(t, h)
	started := time.Now()

	err := session.Submit("not-a-code")

	t.Logf("outcome after %s: %v", time.Since(started).Round(time.Millisecond), err)
	if err == nil {
		t.Fatal("submit = nil, want the malformed code refused")
	}
	select {
	case <-session.Done():
	default:
		t.Error("the login still runs")
	}
	if status, err := h.LoginStatus(t.Context(), runas.User{}); err != nil || status.LoggedIn {
		t.Errorf("status after the failed logins = %+v, %v; want logged out", status, err)
	}
}
