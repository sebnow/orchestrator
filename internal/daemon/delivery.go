package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// A task's work leaves the daemon on a git branch of the task's own,
// which the daemon pushes to the repository it cloned
// (docs/adr/2026-10-08-work-delivery.md).

// pushTimeout bounds one delivery: reading the remote's branches and
// pushing the task's branch.
const pushTimeout = 2 * time.Minute

const (
	// startRefKey is the clone's git configuration key that holds the
	// ref the task was started at; its presence marks a workspace whose
	// work is delivered on a task branch.
	startRefKey = "orchestrator.startref"
	// startCommitRef is the commit the task's ref named when its
	// workspace was cloned. The task branch's commits beyond it are its
	// work.
	startCommitRef = "refs/orchestrator/start"
)

// taskBranch is the branch task's work is delivered on.
func taskBranch(task protocol.TaskID) string {
	return "orchestrator/" + string(task)
}

// deliveryPrompt tells the agent of task how its work leaves the
// workspace. It is added to the system prompt of a task with a
// repository.
func deliveryPrompt(task protocol.TaskID) string {
	branch := taskBranch(task)
	return "Your working directory is a git clone of the task's repository, checked out on branch " + branch + ". " +
		"Commit your work to " + branch + " as you go, in commits with clear messages, and stay on that branch. " +
		"Never push: the orchestrator pushes " + branch + " for you at the end of every turn, " +
		"and a hook in the clone refuses any other push. Work you leave uncommitted is not delivered."
}

// prepareBranch puts the clone in dir, checked out at ref, on task's
// branch, and marks it as a workspace whose work is delivered. When the
// remote already has the branch, as for a task moved off a lost daemon
// (docs/adr/2026-10-08-daemon-loss.md), the branch is checked out from
// the remote, so the work pushed from there continues; otherwise it is
// created at ref. The clone gets a pre-push hook that refuses every ref
// but the branch.
//
// gitName and gitEmail, or the default identity "orchestrator"
// <orchestrator@localhost> when either is empty, become the clone's
// local user.name and user.email; commit.gpgsign and tag.gpgsign are set
// to false, so the daemon user's own signing settings do not apply to
// the agent's commits.
func prepareBranch(ctx context.Context, dir string, task protocol.TaskID, ref, gitName, gitEmail string) error {
	branch := taskBranch(task)
	if gitName == "" || gitEmail == "" {
		gitName, gitEmail = "orchestrator", "orchestrator@localhost"
	}
	hooks, err := filepath.Abs(filepath.Join(dir, ".git", "hooks"))
	if err != nil {
		return err
	}
	if err := installPrePushHook(hooks, branch); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"update-ref", startCommitRef, "HEAD"},
		{"config", "--local", startRefKey, ref},
		// The agent's git reads the owner's configuration, whose
		// core.hooksPath would otherwise bypass the hook.
		{"config", "--local", "core.hooksPath", hooks},
		{"config", "--local", "user.name", gitName},
		{"config", "--local", "user.email", gitEmail},
		{"config", "--local", "commit.gpgsign", "false"},
		{"config", "--local", "tag.gpgsign", "false"},
	} {
		if _, err := gitOutput(ctx, dir, args...); err != nil {
			return err
		}
	}
	remote, err := gitOutput(ctx, dir, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if remote == "" {
		_, err = gitOutput(ctx, dir, "checkout", "--quiet", "-b", branch)
		return err
	}
	fetch := []string{"fetch", "--quiet"}
	// The branch's commits beyond ref are counted against ref, which a
	// shallow clone cannot do.
	if shallow, err := gitOutput(ctx, dir, "rev-parse", "--is-shallow-repository"); err != nil {
		return err
	} else if shallow == "true" {
		fetch = append(fetch, "--unshallow")
	}
	tracking := "refs/remotes/origin/" + branch
	if _, err := gitOutput(ctx, dir, append(fetch, "origin", "+refs/heads/"+branch+":"+tracking)...); err != nil {
		return err
	}
	_, err = gitOutput(ctx, dir, "checkout", "--quiet", "-b", branch, tracking)
	return err
}

// installPrePushHook writes a pre-push hook into hooks that refuses to
// push any ref but branch, or to delete branch.
func installPrePushHook(hooks, branch string) error {
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		return err
	}
	script := `#!/bin/sh
# Installed by the orchestrator daemon. This workspace delivers its work
# on one branch, which the daemon pushes; nothing else may be pushed.
allowed=refs/heads/` + branch + `
while read -r local_ref local_sha remote_ref remote_sha; do
	if [ "$remote_ref" != "$allowed" ]; then
		echo "pre-push: this workspace may push only $allowed, not $remote_ref" >&2
		exit 1
	fi
	case "$local_sha" in
	*[!0]*) ;;
	*)
		echo "pre-push: $allowed may not be deleted" >&2
		exit 1
		;;
	esac
done
exit 0
`
	return os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(script), 0o700)
}

// deliver pushes task's branch from the workspace dir when the branch
// holds commits beyond the task's start that the remote's branch lacks,
// and reports the push. It reports nothing when dir is not a workspace
// whose work is delivered, or when there is nothing to push. A push that
// fails, or that deliver refuses, is reported with Error set.
func deliver(ctx context.Context, dir string, task protocol.TaskID) *protocol.BranchPushed {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return nil
	}
	startRef, err := gitOutput(ctx, dir, "config", "--local", "--get", startRefKey)
	if err != nil || startRef == "" {
		return nil
	}
	branch := taskBranch(task)
	report := &protocol.BranchPushed{Branch: branch}
	commit, err := gitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		report.Error = "the workspace has no branch " + branch
		return report
	}
	report.Commit = commit
	ahead, err := gitOutput(ctx, dir, "rev-list", "--count", commit, "--not", startCommitRef)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	if report.Ahead, err = strconv.Atoi(ahead); err != nil {
		report.Error = "count the branch's commits: " + err.Error()
		return report
	}
	if report.Ahead == 0 {
		return nil
	}
	if report.Uncommitted, err = countUncommitted(ctx, dir); err != nil {
		report.Error = err.Error()
		return report
	}
	remoteHead, remoteCommit, err := remoteBranches(ctx, dir, branch)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	if remoteCommit == commit {
		return nil
	}
	if err := pushBranch(ctx, dir, branch, startRef, remoteHead); err != nil {
		report.Error = err.Error()
	}
	return report
}

// pushBranch pushes branch from the clone in dir to origin, without
// force. It refuses a branch that is the task's start ref, startRef, or
// the remote's default branch, remoteHead, so that the daemon never
// pushes either.
func pushBranch(ctx context.Context, dir, branch, startRef, remoteHead string) error {
	full := "refs/heads/" + branch
	if branch == startRef || full == startRef {
		return fmt.Errorf("refused to push %s: it is the ref the task started from", branch)
	}
	if full == remoteHead {
		return fmt.Errorf("refused to push %s: it is the remote's default branch", branch)
	}
	_, err := gitOutput(ctx, dir, "push", "--quiet", "origin", full+":"+full)
	return err
}

// remoteBranches returns the ref the remote's HEAD names, such as
// refs/heads/main, and the commit its branch holds; either is empty when
// the remote has none.
func remoteBranches(ctx context.Context, dir, branch string) (head, commit string, err error) {
	out, err := gitOutput(ctx, dir, "ls-remote", "--symref", "origin", "HEAD", "refs/heads/"+branch)
	if err != nil {
		return "", "", err
	}
	lines := bufio.NewScanner(strings.NewReader(out))
	for lines.Scan() {
		fields := strings.Split(lines.Text(), "\t")
		if len(fields) != 2 {
			continue
		}
		switch {
		case fields[1] == "HEAD" && strings.HasPrefix(fields[0], "ref: "):
			head = strings.TrimPrefix(fields[0], "ref: ")
		case fields[1] == "refs/heads/"+branch:
			commit = fields[0]
		}
	}
	return head, commit, nil
}

// countUncommitted counts the files in the clone in dir that are
// modified, added, deleted or untracked and not ignored.
func countUncommitted(ctx context.Context, dir string) (int, error) {
	out, err := gitOutput(ctx, dir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return 0, err
	}
	if out == "" {
		return 0, nil
	}
	return strings.Count(out, "\n") + 1, nil
}

// errWorkspaceKept reports a workspace that was not deleted because its
// work could not be delivered.
var errWorkspaceKept = errors.New("workspace kept")

// gitOutput runs git as runGit does and returns its standard output,
// trimmed.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	var stdout bytes.Buffer
	if err := runGitTo(ctx, dir, &stdout, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// joinPrompts appends extra to a system prompt, as a paragraph of its
// own.
func joinPrompts(prompt, extra string) string {
	if prompt == "" {
		return extra
	}
	return prompt + "\n\n" + extra
}
