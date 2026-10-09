package claude

import (
	"os"
	"slices"
	"testing"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/runas"
)

var commandSpec = harness.Spec{
	Workdir: "/srv/workspaces/task-1",
	Model:   "haiku",
	Gateway: harness.Gateway{URL: "http://127.0.0.1:4242/tasks/task-1/mcp", PermissionTool: "permission"},
}

func TestGivenNoHarnessUserWhenBuildingTheCommandThenClaudeRunsDirectlyWithTheDaemonsEnvironment(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("CLAUDECODE", "1")
	h := &Harness{path: "/opt/claude/bin/claude"}
	args, err := arguments(commandSpec)
	if err != nil {
		t.Fatal(err)
	}

	cmd, err := h.command(t.Context(), commandSpec)
	if err != nil {
		t.Fatal(err)
	}

	if want := append([]string{"/opt/claude/bin/claude"}, args...); !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %q, want %q", cmd.Args, want)
	}
	if want := childEnv(os.Environ()); !slices.Equal(cmd.Env, want) {
		t.Errorf("env = %q, want the daemon's without the session markers, %q", cmd.Env, want)
	}
	if cmd.Dir != commandSpec.Workdir || cmd.SysProcAttr != nil {
		t.Errorf("dir = %q, sys = %+v", cmd.Dir, cmd.SysProcAttr)
	}
}

func TestGivenHarnessUserWhenBuildingTheCommandThenSudoRunsClaudeInTheWorkspaceWithOnlyPathAndTheAgent(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/ssh-daemon/agent.1")
	t.Setenv("CLAUDECODE", "1")
	h := &Harness{path: "/opt/claude/bin/claude"}
	spec := commandSpec
	spec.RunAs = runas.User{Name: "orch-agent", SSHAuthSock: "/tmp/orchestrator-agent-1/agent-0123.sock"}
	args, err := arguments(spec)
	if err != nil {
		t.Fatal(err)
	}

	cmd, err := h.command(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}

	want := append([]string{"sudo", "-n", "-u", "orch-agent", "-D", "/srv/workspaces/task-1", "--", "/opt/claude/bin/claude"}, args...)
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %q, want %q", cmd.Args, want)
	}
	if want := []string{"PATH=/usr/bin:/bin", "SSH_AUTH_SOCK=/tmp/orchestrator-agent-1/agent-0123.sock"}; !slices.Equal(cmd.Env, want) {
		t.Errorf("env = %q, want %q", cmd.Env, want)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Errorf("sys = %+v; want a session of its own", cmd.SysProcAttr)
	}
}
