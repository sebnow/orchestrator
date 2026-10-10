package claude_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/runas"
)

// fakeAuth plays `claude auth`: status prints $FAKE_CLAUDE_STATUS and
// exits with $FAKE_CLAUDE_STATUS_EXIT.
func fakeAuth(args []string) {
	switch args[0] {
	case "status":
		fmt.Print(os.Getenv("FAKE_CLAUDE_STATUS"))
		code, _ := strconv.Atoi(os.Getenv("FAKE_CLAUDE_STATUS_EXIT"))
		os.Exit(code)
	}
	fmt.Fprintln(os.Stderr, "fake claude: unknown auth command", args)
	os.Exit(2)
}

// The statuses Claude Code 2.1.289 printed, the logged-in one with the
// owner's identity replaced.
const (
	loggedInStatus = `{
  "loggedIn": true,
  "authMethod": "claude.ai",
  "apiProvider": "firstParty",
  "analyticsDisabled": false,
  "projectsDirectory": "/home/orch-agent/.claude/projects",
  "configDirectory": "/home/orch-agent/.claude",
  "email": "owner@example.com",
  "orgId": "6f1c2a9e-0000-4000-8000-000000000001",
  "orgName": "Example",
  "subscriptionType": "team"
}`
	loggedOutStatus = `{
  "loggedIn": false,
  "authMethod": "none",
  "apiProvider": "firstParty",
  "analyticsDisabled": false,
  "projectsDirectory": "/home/orch-agent/.claude/projects",
  "configDirectory": "/home/orch-agent/.claude"
}`
)

func TestGivenAuthStatusWhenReadThenLoginMethodAndAccountComeFromIt(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		exit string
		want harness.LoginStatus
	}{
		"logged in": {loggedInStatus, "0", harness.LoginStatus{LoggedIn: true, Method: "claude.ai", Account: "owner@example.com/6f1c2a9e-0000-4000-8000-000000000001"}},
		// Claude Code exits 1 when logged out.
		"logged out":    {loggedOutStatus, "1", harness.LoginStatus{Method: "none"}},
		"no org":        {`{"loggedIn":true,"authMethod":"claude.ai","email":"owner@example.com"}`, "0", harness.LoginStatus{LoggedIn: true, Method: "claude.ai", Account: "owner@example.com"}},
		"no identity":   {`{"loggedIn":true,"authMethod":"oauth_token"}`, "0", harness.LoginStatus{LoggedIn: true, Method: "oauth_token"}},
		"out, with org": {`{"loggedIn":false,"authMethod":"none","email":"owner@example.com"}`, "1", harness.LoginStatus{Method: "none"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := fakeHarness(t)
			t.Setenv("FAKE_CLAUDE_STATUS", tc.out)
			t.Setenv("FAKE_CLAUDE_STATUS_EXIT", tc.exit)

			got, err := h.LoginStatus(t.Context(), runas.User{})

			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("status = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestGivenAuthStatusThatIsNotAStatusWhenReadThenItFails(t *testing.T) {
	for name, out := range map[string]string{"not JSON": "error: unknown command", "no loggedIn": `{"authMethod":"none"}`} {
		t.Run(name, func(t *testing.T) {
			h := fakeHarness(t)
			t.Setenv("FAKE_CLAUDE_STATUS", out)
			t.Setenv("FAKE_CLAUDE_STATUS_EXIT", "1")

			if got, err := h.LoginStatus(t.Context(), runas.User{}); err == nil {
				t.Errorf("status = %+v, want an error", got)
			}
		})
	}
}

// recordingSudo stands in for sudo: it writes its arguments to the file
// args and runs the command after "--" as the fake claude, printing the
// status in the file status. Its environment, as sudo's, holds only PATH.
func recordingSudo(args, status string) string {
	return `#!/bin/sh
echo "$@" >` + args + `
while [ "$1" != "--" ]; do shift; done
shift
FAKE_CLAUDE=1 FAKE_CLAUDE_STATUS=$(cat ` + status + `) exec "$@"
`
}

func TestGivenHarnessUserWhenTheStatusIsReadThenClaudeRunsAsThatUserThroughSudo(t *testing.T) {
	dir := t.TempDir()
	args, status, sudo := filepath.Join(dir, "args"), filepath.Join(dir, "status"), filepath.Join(dir, "sudo")
	if err := os.WriteFile(status, []byte(loggedInStatus), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sudo, []byte(recordingSudo(args, status)), 0o700); err != nil {
		t.Fatal(err)
	}
	h := fakeHarness(t)

	got, err := h.LoginStatus(t.Context(), runas.User{Name: "orch-agent", Sudo: sudo})

	if err != nil {
		t.Fatal(err)
	}
	if !got.LoggedIn {
		t.Errorf("status = %+v, want logged in", got)
	}
	recorded, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	if want := "-n -u orch-agent -D / -- " + self + " auth status --json"; strings.TrimSpace(string(recorded)) != want {
		t.Errorf("sudo ran with %q, want %q", recorded, want)
	}
}
