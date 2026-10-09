package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// The remote in these tests is a local bare repository behind an https
// URL that git rewrites to it (httpsAlias), so the daemon's own URL rule
// applies unchanged.

const deliveryTask protocol.TaskID = "task-1"

// cloneForTask prepares the workspace of deliveryTask at ref.
func cloneForTask(t *testing.T, repo testRepo, ref string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "workspaces", string(deliveryTask))
	if err := prepareWorkspace(t.Context(), dir, deliveryTask, &protocol.Workspace{Repo: repo.url, Ref: ref}, "", ""); err != nil {
		t.Fatal(err)
	}
	return dir
}

// tryGit runs git as the agent would, and returns its combined output
// and error.
func tryGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// remoteRef returns the commit ref holds in the bare repository, or ""
// when it has none.
func remoteRef(t *testing.T, repo testRepo, ref string) string {
	t.Helper()
	out, err := tryGit(repo.bare, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return out
}

// pushTaskBranchFrom puts a branch for deliveryTask on the remote, one
// commit beyond main, as a daemon that ran the task before would have.
func pushTaskBranchFrom(t *testing.T, repo testRepo) string {
	t.Helper()
	work := t.TempDir()
	git(t, "", "clone", "--quiet", repo.bare, work)
	git(t, work, "checkout", "--quiet", "-b", taskBranch(deliveryTask))
	commit := commitFile(t, work, "earlier.txt", "pushed by the lost daemon")
	git(t, work, "push", "--quiet", "origin", taskBranch(deliveryTask))
	return commit
}

func TestGivenRemoteWithoutTheTaskBranchWhenPreparingThenTheBranchStartsAtTheRef(t *testing.T) {
	repo := makeTestRepo(t)

	dir := cloneForTask(t, repo, "main")

	if branch := git(t, dir, "symbolic-ref", "--short", "HEAD"); branch != "orchestrator/task-1" {
		t.Errorf("checked out %s, want orchestrator/task-1", branch)
	}
	if head := git(t, dir, "rev-parse", "HEAD"); head != repo.second {
		t.Errorf("HEAD = %s, want main's %s", head, repo.second)
	}
	if pushed := deliver(t.Context(), dir, deliveryTask); pushed != nil {
		t.Errorf("delivered %+v with no commits", *pushed)
	}
	if remoteRef(t, repo, "refs/heads/orchestrator/task-1") != "" {
		t.Error("the branch was pushed with no commits")
	}
}

func TestGivenRemoteWithTheTaskBranchWhenPreparingThenItIsCheckedOutFromTheRemote(t *testing.T) {
	repo := makeTestRepo(t)
	earlier := pushTaskBranchFrom(t, repo)

	dir := cloneForTask(t, repo, "main")

	if head := git(t, dir, "rev-parse", "HEAD"); head != earlier {
		t.Errorf("HEAD = %s, want the pushed %s", head, earlier)
	}
	if got := readWorkspaceFile(t, dir, "earlier.txt"); got != "pushed by the lost daemon" {
		t.Errorf("earlier.txt = %q", got)
	}
	if pushed := deliver(t.Context(), dir, deliveryTask); pushed != nil {
		t.Errorf("delivered %+v with nothing new", *pushed)
	}

	later := commitFile(t, dir, "later.txt", "the new daemon's")
	pushed := deliver(t.Context(), dir, deliveryTask)

	if pushed == nil || pushed.Error != "" || pushed.Commit != later || pushed.Ahead != 2 {
		t.Fatalf("delivered %+v, want %s two commits ahead of main", pushed, later)
	}
	if got := remoteRef(t, repo, "refs/heads/orchestrator/task-1"); got != later {
		t.Errorf("remote branch = %s, want %s", got, later)
	}
}

func TestGivenCommitsOnTheTaskBranchWhenDeliveringThenTheBranchIsPushedOnceAndReportedWhileFilesStayUncommitted(t *testing.T) {
	repo := makeTestRepo(t)
	dir := cloneForTask(t, repo, "main")
	commit := commitFile(t, dir, "work.txt", "done")
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("left over"), 0o600); err != nil {
		t.Fatal(err)
	}

	pushed := deliver(t.Context(), dir, deliveryTask)

	want := protocol.BranchPushed{Branch: "orchestrator/task-1", Commit: commit, Ahead: 1, Uncommitted: 1}
	if pushed == nil || *pushed != want {
		t.Fatalf("delivered %+v, want %+v", pushed, want)
	}
	if got := remoteRef(t, repo, "refs/heads/orchestrator/task-1"); got != commit {
		t.Errorf("remote branch = %s, want %s", got, commit)
	}
	if got := remoteRef(t, repo, "refs/heads/main"); got != repo.second {
		t.Errorf("remote main = %s, want it untouched at %s", got, repo.second)
	}
	if again := deliver(t.Context(), dir, deliveryTask); again == nil || *again != want {
		t.Errorf("delivered %+v again with a file still uncommitted, want %+v", again, want)
	}
	if err := os.Remove(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if again := deliver(t.Context(), dir, deliveryTask); again != nil {
		t.Errorf("delivered %+v again with nothing new and nothing uncommitted", *again)
	}
}

func TestGivenUncommittedFilesButNoCommitsWhenDeliveringThenTheBranchIsReportedAsItStandsWithoutReachingTheRemote(t *testing.T) {
	repo := makeTestRepo(t)
	dir := cloneForTask(t, repo, "main")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	// With the remote gone, any attempt to read or push it fails.
	hidden := repo.bare + ".hidden"
	if err := os.Rename(repo.bare, hidden); err != nil {
		t.Fatal(err)
	}

	pushed := deliver(t.Context(), dir, deliveryTask)

	want := protocol.BranchPushed{Branch: "orchestrator/task-1", Commit: repo.second, Ahead: 0, Uncommitted: 2}
	if pushed == nil || *pushed != want {
		t.Fatalf("delivered %+v, want %+v", pushed, want)
	}
	if err := os.Rename(hidden, repo.bare); err != nil {
		t.Fatal(err)
	}
	if got := remoteRef(t, repo, "refs/heads/orchestrator/task-1"); got != "" {
		t.Errorf("remote branch = %s, want none pushed", got)
	}
}

func TestGivenTheWorkspaceHookWhenAnythingButTheTaskBranchIsPushedThenItIsRefused(t *testing.T) {
	repo := makeTestRepo(t)
	dir := cloneForTask(t, repo, "main")
	commit := commitFile(t, dir, "work.txt", "done")

	for _, refspec := range []string{"HEAD:refs/heads/main", "HEAD:refs/heads/other", "HEAD:refs/tags/v2"} {
		out, err := tryGit(dir, "push", "origin", refspec)
		if err == nil || !strings.Contains(out, "pre-push: this workspace may push only refs/heads/orchestrator/task-1") {
			t.Errorf("push %s: err %v, output %q; want the hook's refusal", refspec, err, out)
		}
	}
	if got := remoteRef(t, repo, "refs/heads/main"); got != repo.second {
		t.Errorf("remote main = %s, want it untouched at %s", got, repo.second)
	}

	if out, err := tryGit(dir, "push", "origin", "orchestrator/task-1"); err != nil {
		t.Fatalf("push of the task branch: %v: %s", err, out)
	}
	if got := remoteRef(t, repo, "refs/heads/orchestrator/task-1"); got != commit {
		t.Errorf("remote branch = %s, want %s", got, commit)
	}
	if out, err := tryGit(dir, "push", "origin", ":refs/heads/orchestrator/task-1"); err == nil || !strings.Contains(out, "may not be deleted") {
		t.Errorf("delete: err %v, output %q; want the hook's refusal", err, out)
	}
}

func TestGivenTheStartRefOrTheRemoteDefaultBranchWhenTheDaemonPushesThenItRefuses(t *testing.T) {
	repo := makeTestRepo(t)
	dir := cloneForTask(t, repo, "feature")
	git(t, dir, "branch", "main", "--force", "HEAD")

	for _, c := range []struct {
		branch, startRef, remoteHead, want string
	}{
		{"feature", "feature", "refs/heads/main", "the ref the task started from"},
		{"feature", "refs/heads/feature", "refs/heads/main", "the ref the task started from"},
		{"main", "feature", "refs/heads/main", "the remote's default branch"},
	} {
		err := pushBranch(t.Context(), dir, c.branch, c.startRef, c.remoteHead)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("push %s (start %s, remote HEAD %s): %v; want refused as %s", c.branch, c.startRef, c.remoteHead, err, c.want)
		}
	}
	if got := remoteRef(t, repo, "refs/heads/main"); got != repo.second {
		t.Errorf("remote main = %s, want it untouched at %s", got, repo.second)
	}
	if got := remoteRef(t, repo, "refs/heads/feature"); got != repo.featureCommit {
		t.Errorf("remote feature = %s, want it untouched at %s", got, repo.featureCommit)
	}
}

func TestGivenTaskBranchNamedAsTheStartRefWhenDeliveringThenThePushIsRefusedAndReported(t *testing.T) {
	repo := makeTestRepo(t)
	dir := cloneForTask(t, repo, "main")
	commitFile(t, dir, "work.txt", "done")
	git(t, dir, "config", "--local", startRefKey, "orchestrator/task-1")

	pushed := deliver(t.Context(), dir, deliveryTask)

	if pushed == nil || !strings.Contains(pushed.Error, "refused to push orchestrator/task-1") {
		t.Fatalf("delivered %+v, want the refusal", pushed)
	}
	if remoteRef(t, repo, "refs/heads/orchestrator/task-1") != "" {
		t.Error("the branch was pushed")
	}
}

func TestGivenUnpushedCommitsWhenTheWorkspaceIsDeletedThenTheyArePushedFirst(t *testing.T) {
	repo := makeTestRepo(t)
	stateDir := t.TempDir()
	dir := workspacePath(stateDir, deliveryTask)
	if err := prepareWorkspace(t.Context(), dir, deliveryTask, &protocol.Workspace{Repo: repo.url, Ref: "main"}, "", ""); err != nil {
		t.Fatal(err)
	}
	commit := commitFile(t, dir, "work.txt", "done")

	if err := deleteWorkspace(stateDir, deliveryTask); err != nil {
		t.Fatal(err)
	}

	if got := remoteRef(t, repo, "refs/heads/orchestrator/task-1"); got != commit {
		t.Errorf("remote branch = %s, want %s", got, commit)
	}
	if !workspaceGone(stateDir, deliveryTask)() {
		t.Error("the workspace was kept")
	}
}

func TestGivenAPushThatFailsWhenTheWorkspaceIsDeletedThenItIsKeptAndTheFailureReturned(t *testing.T) {
	repo := makeTestRepo(t)
	stateDir := t.TempDir()
	dir := workspacePath(stateDir, deliveryTask)
	if err := prepareWorkspace(t.Context(), dir, deliveryTask, &protocol.Workspace{Repo: repo.url, Ref: "main"}, "", ""); err != nil {
		t.Fatal(err)
	}
	commitFile(t, dir, "work.txt", "done")
	// Another daemon pushed the branch meanwhile, so a push without force
	// is rejected.
	pushTaskBranchFrom(t, repo)

	err := deleteWorkspace(stateDir, deliveryTask)

	if !errors.Is(err, errWorkspaceKept) || !strings.Contains(err.Error(), "git push") {
		t.Errorf("err = %v, want the workspace kept for a failed push", err)
	}
	if got := readWorkspaceFile(t, dir, "work.txt"); got != "done" {
		t.Errorf("work.txt = %q, want the work kept", got)
	}
}

func TestGivenTaskWithAWorkspaceWhenItsTurnEndsWithACommitThenTheBranchIsPushedBeforeTheExitIsReported(t *testing.T) {
	repo := makeTestRepo(t)
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{
		Prompt: "Do the work.", Workspace: &protocol.Workspace{Repo: repo.url, Ref: "main"}, PauseLimits: testPauseLimits,
	})
	proc := d.nextProcess(t)
	if !strings.Contains(proc.spec.SystemPrompt, deliveryPrompt(task)) {
		t.Errorf("system prompt = %q, want it to tell the agent how its work is delivered", proc.spec.SystemPrompt)
	}
	commit := commitFile(t, proc.spec.Workdir, "work.txt", "done")

	finishTurn(t, proc, "session-1")
	expectExit(t, proc)

	events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	last, beforeLast := events[len(events)-1], events[len(events)-2]
	var pushed protocol.BranchPushed
	if beforeLast.Kind != protocol.KindBranchPushed || json.Unmarshal(beforeLast.Payload, &pushed) != nil {
		t.Fatalf("events: %s; want branch_pushed right before harness_exited", describe(events))
	}
	want := protocol.BranchPushed{Branch: taskBranch(task), Commit: commit, Ahead: 1}
	if pushed != want || last.Kind != protocol.KindHarnessExited {
		t.Errorf("branch_pushed = %+v, want %+v", pushed, want)
	}
	if got := remoteRef(t, repo, "refs/heads/"+taskBranch(task)); got != commit {
		t.Errorf("remote branch = %s, want %s", got, commit)
	}
}

func TestGivenTaskWithAWorkspaceWhenItsTurnEndsWithFilesUncommittedAndNoCommitThenTheBranchIsReportedBeforeTheExit(t *testing.T) {
	repo := makeTestRepo(t)
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{
		Prompt: "Do the work.", Workspace: &protocol.Workspace{Repo: repo.url, Ref: "main"}, PauseLimits: testPauseLimits,
	})
	proc := d.nextProcess(t)
	if err := os.WriteFile(filepath.Join(proc.spec.Workdir, "draft.txt"), []byte("half done"), 0o600); err != nil {
		t.Fatal(err)
	}

	finishTurn(t, proc, "session-1")
	expectExit(t, proc)

	events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	beforeLast := events[len(events)-2]
	var pushed protocol.BranchPushed
	if beforeLast.Kind != protocol.KindBranchPushed || json.Unmarshal(beforeLast.Payload, &pushed) != nil {
		t.Fatalf("events: %s; want branch_pushed right before harness_exited", describe(events))
	}
	want := protocol.BranchPushed{Branch: taskBranch(task), Commit: repo.second, Uncommitted: 1}
	if pushed != want {
		t.Errorf("branch_pushed = %+v, want %+v", pushed, want)
	}
	if got := remoteRef(t, repo, "refs/heads/"+taskBranch(task)); got != "" {
		t.Errorf("remote branch = %s, want none pushed", got)
	}
}

func TestGivenAGitIdentityWhenPreparingThenTheCloneCommitsAsItWithoutSigning(t *testing.T) {
	repo := makeTestRepo(t)
	dir := filepath.Join(t.TempDir(), "workspaces", string(deliveryTask))
	if err := prepareWorkspace(t.Context(), dir, deliveryTask, &protocol.Workspace{Repo: repo.url, Ref: "main"}, "Agent Smith", "smith@example.com"); err != nil {
		t.Fatal(err)
	}

	if out, err := tryGit(dir, "commit", "--allow-empty", "-m", "x"); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	if out, err := tryGit(dir, "log", "-1", "--format=%an|%ae"); err != nil || out != "Agent Smith|smith@example.com" {
		t.Errorf("log = %q, err %v; want Agent Smith|smith@example.com", out, err)
	}
	if out, err := tryGit(dir, "config", "--local", "commit.gpgsign"); err != nil || out != "false" {
		t.Errorf("commit.gpgsign = %q, err %v; want false", out, err)
	}
}
