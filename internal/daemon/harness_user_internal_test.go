package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/runas"
)

// fakeSudoName is the name the test binary runs under when it stands in
// for sudo. sudo's environment holds only PATH and SSH_AUTH_SOCK, so the
// binary cannot be told by a variable.
const fakeSudoName = "fake-sudo"

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == fakeSudoName {
		runFakeSudo()
		return
	}
	os.Exit(m.Run())
}

// sudoCall is one run of the fake sudo: its arguments, without its own
// name, and its environment.
type sudoCall struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
}

// runFakeSudo plays sudo -n -u USER -D DIR VAR=value... -- COMMAND
// ARG...: it appends the call to "calls" beside it, then runs COMMAND as
// the current user, in DIR, with its own environment, the lines of
// "env" beside it, and the VAR=value settings.
func runFakeSudo() {
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "fake sudo: "+format+"\n", args...)
		os.Exit(90)
	}
	dir := filepath.Dir(os.Args[0])
	call, err := json.Marshal(sudoCall{Args: os.Args[1:], Env: os.Environ()})
	if err != nil {
		fail("%v", err)
	}
	calls, err := os.OpenFile(filepath.Join(dir, "calls"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		fail("%v", err)
	}
	calls.Write(append(call, '\n'))
	calls.Close()

	args := os.Args[1:]
	if len(args) < 5 || args[0] != "-n" || args[1] != "-u" || args[3] != "-D" {
		fail("unexpected arguments %q", args)
	}
	cwd := args[4]
	args = args[5:]
	env := os.Environ()
	if extra, err := os.ReadFile(filepath.Join(dir, "env")); err == nil {
		env = append(env, strings.Split(strings.TrimSpace(string(extra)), "\n")...)
	}
	for len(args) > 0 && args[0] != "--" {
		if !strings.Contains(args[0], "=") {
			fail("unexpected argument %q before --", args[0])
		}
		env = append(env, args[0])
		args = args[1:]
	}
	if len(args) < 2 {
		fail("no command after --")
	}
	if err := os.Chdir(cwd); err != nil {
		fail("%v", err)
	}
	fail("exec %s: %v", args[1], syscall.Exec(args[1], args[1:], env))
}

// fakeSudo is a fake sudo for one test.
type fakeSudo struct {
	path string
}

// newFakeSudo installs a fake sudo whose commands also get extraEnv.
func newFakeSudo(t *testing.T, extraEnv []string) fakeSudo {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, fakeSudoName)
	if err := os.Symlink(self, path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte(strings.Join(extraEnv, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	return fakeSudo{path: path}
}

func (s fakeSudo) calls(t *testing.T) []sudoCall {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(s.path), "calls"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []sudoCall
	lines := bufio.NewScanner(strings.NewReader(string(data)))
	for lines.Scan() {
		var call sudoCall
		if err := json.Unmarshal(lines.Bytes(), &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	return calls
}

// command returns the command sudo was asked to run in call, from its
// absolute path on.
func (c sudoCall) command() []string {
	i := slices.Index(c.Args, "--")
	if i < 0 {
		return nil
	}
	return c.Args[i+1:]
}

func lookPath(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// harnessRunner runs the workspace commands through the fake sudo as the
// harness user "orch-agent", with the git and rm on PATH.
func harnessRunner(t *testing.T, sudo fakeSudo) runner {
	t.Helper()
	return runner{
		as:     runas.User{Name: "orch-agent", Sudo: sudo.path},
		gitCmd: lookPath(t, "git"),
		rmCmd:  lookPath(t, "rm"),
	}
}

// gitConfigEnv is the git configuration the test's environment gives git,
// such as httpsAlias's rewrites, which sudo would not pass on.
func gitConfigEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool { return !strings.HasPrefix(kv, "GIT_CONFIG_") })
}

func TestGivenHarnessUserWhenRunningGitThenSudoRunsItWithTheGitSettingsBeforeTheSeparatorAndOnlyPathAndTheAgent(t *testing.T) {
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("SSH_AUTH_SOCK", "/tmp/ssh-daemon/agent.1")
	sudo := newFakeSudo(t, nil)
	workspace := t.TempDir()
	r := runner{
		as:     runas.User{Name: "orch-agent", SSHAuthSock: "/tmp/orchestrator-agent-1/agent-0123.sock", Sudo: sudo.path},
		gitCmd: lookPath(t, "true"),
	}

	if err := r.runGit(t.Context(), workspace, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}

	calls := sudo.calls(t)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one", calls)
	}
	want := []string{"-n", "-u", "orch-agent", "-D", workspace,
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_SSH_COMMAND=ssh -o BatchMode=yes",
		"--", r.gitCmd, "status", "--porcelain"}
	if !slices.Equal(calls[0].Args, want) {
		t.Errorf("sudo args = %q, want %q", calls[0].Args, want)
	}
	if want := []string{"PATH=" + os.Getenv("PATH"), "SSH_AUTH_SOCK=/tmp/orchestrator-agent-1/agent-0123.sock"}; !slices.Equal(calls[0].Env, want) {
		t.Errorf("sudo env = %q, want %q", calls[0].Env, want)
	}
}

func TestGivenHarnessUserWhenATaskWithARepositoryRunsAndEndsThenEveryWorkspaceCommandRunsThroughSudo(t *testing.T) {
	repo := makeTestRepo(t)
	sudo := newFakeSudo(t, gitConfigEnv())
	r := harnessRunner(t, sudo)
	dir := filepath.Join(t.TempDir(), string(deliveryTask))

	if err := r.prepareWorkspace(t.Context(), dir, deliveryTask, &protocol.Workspace{Repo: repo.url, Ref: "main"}, "", ""); err != nil {
		t.Fatal(err)
	}
	commitAsAgent(t, dir, "work.txt")
	if out, err := tryGit(dir, "push", "origin", "HEAD:refs/heads/other"); err == nil || !strings.Contains(out, "pre-push: this workspace may push only") {
		t.Errorf("push of another branch = %v: %s; want the hook from the template to refuse it", err, out)
	}
	pushed := r.deliver(t.Context(), dir, deliveryTask)
	if pushed == nil || pushed.Error != "" || pushed.Ahead != 1 || remoteRef(t, repo, "refs/heads/"+taskBranch(deliveryTask)) != pushed.Commit {
		t.Errorf("delivered %+v; want the task branch pushed", pushed)
	}
	if err := r.deleteWorkspace(dir, deliveryTask); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("workspace after deletion: %v; want it gone", err)
	}
	calls := sudo.calls(t)
	var commands []string
	for _, call := range calls {
		command := call.command()
		if len(command) < 2 || command[0] != r.gitCmd && command[0] != r.rmCmd {
			t.Errorf("sudo ran %q; want only git and rm by absolute path", command)
			continue
		}
		commands = append(commands, filepath.Base(command[0])+" "+command[1])
	}
	for _, want := range []string{"git clone", "git config", "git ls-remote", "git status", "git rev-list", "git rev-parse", "git push", "rm -rf"} {
		if !slices.Contains(commands, want) {
			t.Errorf("commands through sudo = %q; want %q among them", commands, want)
		}
	}
	clone := calls[0].command()
	template := clone[slices.Index(clone, "--template")+1]
	if !strings.HasPrefix(template, runas.TempDir+"/") {
		t.Errorf("clone = %q; want a hook template under %s", clone, runas.TempDir)
	}
	if _, err := os.Stat(template); !os.IsNotExist(err) {
		t.Errorf("hook template after the clone: %v; want it deleted", err)
	}
}

// commitAsAgent commits a new file in the clone in dir, as the agent
// would.
func commitAsAgent(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", name}, {"commit", "--quiet", "-m", "add " + name}} {
		if out, err := tryGit(dir, args...); err != nil {
			t.Fatalf("git %s: %v: %s", args[0], err, out)
		}
	}
}

func TestGivenHarnessUserWhenATaskWithoutARepositoryStartsThenGitMakesAnEmptyDirectory(t *testing.T) {
	sudo := newFakeSudo(t, nil)
	r := harnessRunner(t, sudo)
	dir := filepath.Join(t.TempDir(), "task-1")

	if err := r.prepareWorkspace(t.Context(), dir, "task-1", nil, "", ""); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Errorf("workspace entries = %v, %v; want an empty directory", entries, err)
	}
	var commands [][]string
	for _, call := range sudo.calls(t) {
		commands = append(commands, call.command())
	}
	want := [][]string{{r.gitCmd, "init", "--quiet", "--", dir}, {r.rmCmd, "-rf", "--", filepath.Join(dir, ".git")}}
	if !slices.EqualFunc(commands, want, slices.Equal) {
		t.Errorf("commands through sudo = %q, want %q", commands, want)
	}
}

func TestGivenHarnessUserWhenDeliveringFromAMissingWorkspaceThenNothingIsReported(t *testing.T) {
	r := harnessRunner(t, newFakeSudo(t, nil))

	if pushed := r.deliver(t.Context(), filepath.Join(t.TempDir(), "missing"), deliveryTask); pushed != nil {
		t.Errorf("delivered %+v; want nothing", pushed)
	}
}

func TestGivenNoWorkspaceDirectoryWhenResolvingThenItIsUnderTheStateDirectoryUnlessAHarnessUserNeedsOne(t *testing.T) {
	stateDir := t.TempDir()
	if dir, err := resolveWorkspaceDir(stateDir, "", runas.User{}); err != nil || dir != filepath.Join(stateDir, "workspaces") {
		t.Errorf("without a harness user = %q, %v; want workspaces under the state directory", dir, err)
	}
	if dir, err := resolveWorkspaceDir(stateDir, "", runas.User{Name: "orch-agent"}); err == nil {
		t.Errorf("with a harness user = %q; want an error", dir)
	}
}

func TestGivenWorkspaceDirectoryWhenResolvingThenItIsAbsoluteAndMustExistForAHarnessUser(t *testing.T) {
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("ws", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, user := range []runas.User{{}, {Name: "orch-agent"}} {
		if dir, err := resolveWorkspaceDir("/state", "ws", user); err != nil || dir != filepath.Join(cwd, "ws") {
			t.Errorf("user %q: %q, %v; want %s", user.Name, dir, err, filepath.Join(cwd, "ws"))
		}
	}
	if dir, err := resolveWorkspaceDir("/state", "missing", runas.User{}); err != nil || dir != filepath.Join(cwd, "missing") {
		t.Errorf("missing without a harness user = %q, %v; want it accepted, to be created", dir, err)
	}
	if _, err := resolveWorkspaceDir("/state", "missing", runas.User{Name: "orch-agent"}); err == nil || !strings.Contains(err.Error(), "owned by the harness user orch-agent") {
		t.Errorf("missing with a harness user: %v; want an error saying to create it", err)
	}
}

func TestGivenHarnessUserWhenServingThenTheHarnessRunsAsThatUserInTheWorkspaceDirectory(t *testing.T) {
	srv := startServer(t)
	workspaces := t.TempDir()
	sudo := newFakeSudo(t, nil)
	d := runDaemonAs(t, srv.url, t.TempDir(), func(cfg *Config) {
		cfg.WorkspaceDir = workspaces
		cfg.HarnessUser = runas.User{Name: "orch-agent", SSHAuthSock: "/tmp/agent.sock", Sudo: sudo.path}
	})

	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", Model: "fake-model", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)

	if want := filepath.Join(workspaces, string(task)); proc.spec.Workdir != want {
		t.Errorf("workdir = %q, want %q", proc.spec.Workdir, want)
	}
	if want := (runas.User{Name: "orch-agent", SSHAuthSock: "/tmp/agent.sock", Sudo: sudo.path}); proc.spec.RunAs != want {
		t.Errorf("run as = %+v, want %+v", proc.spec.RunAs, want)
	}
	if info, err := os.Stat(proc.spec.Workdir); err != nil || !info.IsDir() {
		t.Errorf("workspace: %v", err)
	}
	if calls := sudo.calls(t); len(calls) == 0 || calls[0].command()[1] != "init" {
		t.Errorf("sudo calls = %+v; want the workspace made through sudo", calls)
	}
}

// runDaemonAs runs a daemon as runDaemon does, with adjust applied to its
// configuration.
func runDaemonAs(t *testing.T, server *url.URL, stateDir string, adjust func(*Config)) *daemonFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &daemonFixture{harness: newFakeHarness(), processes: newFakeProcesses(), stateDir: stateDir, cancel: cancel, done: make(chan error, 1)}
	cfg := Config{
		Server:          server,
		ID:              testDaemon,
		StateDir:        stateDir,
		Harness:         f.harness,
		Gateway:         startTestGateway(t),
		Log:             testLogger(t),
		MinBackoff:      5 * time.Millisecond,
		MaxBackoff:      50 * time.Millisecond,
		ShutdownTimeout: time.Second,
		processes:       f.processes,
	}
	adjust(&cfg)
	go func() { f.done <- Serve(ctx, cfg) }()
	t.Cleanup(func() { f.stop(t) })
	return f
}

func TestGivenHarnessUserWhenKillingAProcessLeftBehindThenItIsSentSIGTERM(t *testing.T) {
	child := exec.Command("sh", "-c", "trap 'exit 7' TERM; echo ready; while :; do sleep 0.05; done")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill() })
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	if err := (psTable{terminate: true}).kill(child.Process.Pid); err != nil {
		t.Fatal(err)
	}

	child.Wait()
	if code := child.ProcessState.ExitCode(); code != 7 {
		t.Errorf("child ended with %s; want exit 7 from its SIGTERM trap", child.ProcessState)
	}
}
