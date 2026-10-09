package runas_test

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sebnow/orchestrator/internal/runas"
)

var env = []string{"HOME=/home/daemon", "PATH=/usr/bin:/bin", "SSH_AUTH_SOCK=/tmp/ssh-daemon/agent.1", "CLAUDECODE=1"}

func TestGivenNoUserWhenBuildingACommandThenThePathRunsDirectlyWithTheEnvironmentAndVars(t *testing.T) {
	cmd := runas.User{}.Command(t.Context(), "/work", "/usr/bin/git", []string{"status"}, env, []string{"GIT_TERMINAL_PROMPT=0"})

	if want := []string{"/usr/bin/git", "status"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %q, want %q", cmd.Args, want)
	}
	if want := append(slices.Clone(env), "GIT_TERMINAL_PROMPT=0"); !slices.Equal(cmd.Env, want) {
		t.Errorf("env = %q, want %q", cmd.Env, want)
	}
	if cmd.Dir != "/work" || cmd.SysProcAttr != nil || cmd.WaitDelay != 0 {
		t.Errorf("dir = %q, sys = %+v, wait delay = %s; want the work directory and nothing else", cmd.Dir, cmd.SysProcAttr, cmd.WaitDelay)
	}
}

func TestGivenHarnessUserWhenBuildingACommandThenSudoRunsItWithVarsBeforeTheSeparatorAndOnlyPathAndTheAgent(t *testing.T) {
	user := runas.User{Name: "agent", SSHAuthSock: "/tmp/orchestrator-agent-1/agent.sock", Sudo: "/usr/bin/sudo"}

	cmd := user.Command(t.Context(), "/work", "/usr/bin/git", []string{"status", "--porcelain"}, env, []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes"})

	want := []string{"/usr/bin/sudo", "-n", "-u", "agent", "-D", "/work",
		"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes",
		"--", "/usr/bin/git", "status", "--porcelain"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %q, want %q", cmd.Args, want)
	}
	if want := []string{"PATH=/usr/bin:/bin", "SSH_AUTH_SOCK=/tmp/orchestrator-agent-1/agent.sock"}; !slices.Equal(cmd.Env, want) {
		t.Errorf("env = %q, want %q", cmd.Env, want)
	}
	if cmd.Dir != "" {
		t.Errorf("dir = %q; want sudo to change directory instead", cmd.Dir)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid || cmd.Cancel == nil {
		t.Errorf("sys = %+v, cancel set = %t; want a session of its own and a cancel that terminates", cmd.SysProcAttr, cmd.Cancel != nil)
	}
}

func TestGivenHarnessUserWithoutAgentOrDirectoryWhenBuildingACommandThenItRunsInRootWithOnlyPath(t *testing.T) {
	cmd := runas.User{Name: "agent"}.Command(t.Context(), "", "/bin/rm", []string{"-rf", "--", "/ws/task-1"}, env, nil)

	if want := []string{"sudo", "-n", "-u", "agent", "-D", "/", "--", "/bin/rm", "-rf", "--", "/ws/task-1"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %q, want %q", cmd.Args, want)
	}
	if want := []string{"PATH=/usr/bin:/bin"}; !slices.Equal(cmd.Env, want) {
		t.Errorf("env = %q, want %q", cmd.Env, want)
	}
}

func TestGivenHarnessUserCommandWhenItsContextEndsThenSudoIsSentSIGTERM(t *testing.T) {
	sudo := filepath.Join(t.TempDir(), "sudo")
	// Stands in for sudo: it says when its trap is set, and exits 7 on
	// SIGTERM.
	script := "#!/bin/sh\ntrap 'exit 7' TERM\necho ready\nwhile :; do sleep 0.05; done\n"
	if err := os.WriteFile(sudo, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := runas.User{Name: "agent", Sudo: sudo}.Command(ctx, "", "/bin/true", nil, env, nil)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	cancel()
	cmd.Wait()

	if code := cmd.ProcessState.ExitCode(); code != 7 {
		t.Errorf("exit = %s; want 7, from the SIGTERM trap", cmd.ProcessState)
	}
}
