package claude_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
	case "login":
		fakeLogin()
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
printf '%s\n' "$*" >` + args + `
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

// loginURLText is the URL the fake login prints, with the redirect and
// scopes of the one Claude Code 2.1.289 printed and made-up challenge
// and state.
const loginURLText = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference&code_challenge=CHALLENGE&code_challenge_method=S256&state=STATE"

// fakeLogin plays `claude auth login` as Claude Code 2.1.289 behaved:
// the URL as an OSC 8 hyperlink, then the prompt without a line ending,
// and then for each line of input: "good#STATE" logs in, "malformed" is
// refused on stderr and another awaited, and anything else fails the
// login. At the end of its input it waits until it is killed.
// FAKE_CLAUDE_LOGIN=browser completes the login without a code, and
// FAKE_CLAUDE_LOGIN=broken fails before printing the URL.
func fakeLogin() {
	switch os.Getenv("FAKE_CLAUDE_LOGIN") {
	case "broken":
		fmt.Fprintln(os.Stderr, "cannot reach the login service")
		os.Exit(1)
	}
	fmt.Print("Opening browser to sign in\u2026\n")
	fmt.Print("If the browser didn't open, visit: \x1b]8;;" + loginURLText + "\x07" + loginURLText + "\x1b]8;;\x07\n")
	fmt.Print("Paste code here if prompted > ")
	if os.Getenv("FAKE_CLAUDE_LOGIN") == "browser" {
		time.Sleep(100 * time.Millisecond)
		fmt.Println("Login successful.")
		os.Exit(0)
	}
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		switch in.Text() {
		case "good#STATE":
			fmt.Println("Login successful.")
			os.Exit(0)
		case "malformed":
			fmt.Fprintln(os.Stderr, "Invalid code. Please make sure the full code was copied.")
		default:
			fmt.Fprintln(os.Stderr, "Login failed: Request failed with status code 400")
			os.Exit(1)
		}
	}
	for {
		time.Sleep(time.Hour)
	}
}

func startLogin(t *testing.T) harness.LoginSession {
	t.Helper()
	h := fakeHarness(t)
	session, err := h.StartLogin(t.Context(), runas.User{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	if session.URL() != loginURLText {
		t.Fatalf("URL = %q, want the hyperlink's target %q", session.URL(), loginURLText)
	}
	return session
}

func TestGivenLoginWhenTheCodeIsGoodThenTheURLWasTakenFromTheHyperlinkAndTheLoginSucceeds(t *testing.T) {
	session := startLogin(t)

	if err := session.Submit("good#STATE"); err != nil {
		t.Fatalf("submit = %v, want success", err)
	}
	if err := session.Err(); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestGivenLoginWhenTheCodeIsRefusedThenSubmitReturnsWhatClaudeSaid(t *testing.T) {
	session := startLogin(t)

	err := session.Submit("wrong#STATE")

	if err == nil || err.Error() != "Login failed: Request failed with status code 400" {
		t.Errorf("submit = %v, want claude's complaint", err)
	}
}

func TestGivenLoginWhenTheCodeIsMalformedAndClaudeWaitsForAnotherThenTheLoginIsGivenUpWithItsComplaint(t *testing.T) {
	session := startLogin(t)
	started := time.Now()

	err := session.Submit("malformed")

	if err == nil || err.Error() != "Invalid code. Please make sure the full code was copied." {
		t.Errorf("submit = %v, want claude's complaint", err)
	}
	select {
	case <-session.Done():
	default:
		t.Error("the login still runs")
	}
	if took := time.Since(started); took > 15*time.Second {
		t.Errorf("submit took %s", took)
	}
}

func TestGivenLoginWhenItCompletesWithoutACodeThenItIsDone(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_LOGIN", "browser")
	session := startLogin(t)

	select {
	case <-session.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the login did not end")
	}
	if err := session.Err(); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestGivenLoginThatFailsBeforeItsURLWhenStartedThenTheErrorSaysWhy(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_LOGIN", "broken")
	h := fakeHarness(t)

	_, err := h.StartLogin(t.Context(), runas.User{})

	if err == nil || !strings.Contains(err.Error(), "cannot reach the login service") {
		t.Errorf("err = %v, want claude's complaint", err)
	}
}

func TestGivenLoginWaitingForACodeWhenClosedThenItEnds(t *testing.T) {
	session := startLogin(t)

	session.Close()

	select {
	case <-session.Done():
	default:
		t.Fatal("the login still runs after Close")
	}
	if session.Err() == nil {
		t.Error("err = nil, want the login ended without logging in")
	}
}
