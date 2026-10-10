package daemon

import (
	"bufio"
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

// prepareBranch puts the clone in dir, checked out at ref, on task's
// branch, and marks it as a workspace whose work is delivered. When the
// remote already has the branch, onRemote, as for a task moved off a
// lost daemon (docs/adr/2026-10-08-daemon-loss.md), the branch is
// checked out from the remote's, which the clone has from the mirror,
// so the work pushed from there continues; otherwise it is created at
// ref. The clone gets a pre-push hook that refuses every ref but the
// branch.
//
// gitName and gitEmail, or the default identity "orchestrator"
// <orchestrator@localhost> when either is empty, become the clone's
// local user.name and user.email; commit.gpgsign and tag.gpgsign are set
// to false, so the daemon user's own signing settings do not apply to
// the agent's commits.
func (r runner) prepareBranch(ctx context.Context, dir string, task protocol.TaskID, ref string, onRemote bool, gitName, gitEmail string) error {
	branch := protocol.TaskBranch(task)
	if gitName == "" || gitEmail == "" {
		gitName, gitEmail = "orchestrator", "orchestrator@localhost"
	}
	hooks, err := filepath.Abs(filepath.Join(dir, ".git", "hooks"))
	if err != nil {
		return err
	}
	// A clone made as the harness user got the hook from its template.
	if !r.as.Other() {
		if err := installPrePushHook(hooks, branch); err != nil {
			return err
		}
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
		if _, err := r.gitOutput(ctx, dir, args...); err != nil {
			return err
		}
	}
	checkout := []string{"checkout", "--quiet", "-b", branch}
	if onRemote {
		checkout = append(checkout, "refs/remotes/origin/"+branch)
	}
	_, err = r.gitOutput(ctx, dir, checkout...)
	return err
}

// installPrePushHook writes the pre-push hook for branch into hooks.
func installPrePushHook(hooks, branch string) error {
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(prePushHook(branch)), 0o700)
}

// prePushHook is a pre-push hook that refuses to push any ref but branch,
// or to delete branch.
func prePushHook(branch string) string {
	return `#!/bin/sh
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
}

// deliver pushes task's branch from the workspace dir when the branch
// holds commits beyond the task's start that the remote's branch lacks,
// and reports the branch when it pushed it or when the workspace holds
// uncommitted files, so that work left uncommitted is seen. With nothing
// to push it pushes nothing and needs no remote. It reports nothing when
// dir is not a workspace whose work is delivered, or when there is
// nothing to push and nothing uncommitted. A push that fails, or that
// deliver refuses, is reported with Error set.
//
// The push goes through the mirror of the repository the daemon
// recorded for the workspace, never one the workspace's configuration
// names, which the agent can change
// (docs/adr/2026-10-10-daemon-push-identity.md).
func (r runner) deliver(ctx context.Context, dir string, task protocol.TaskID) *protocol.BranchPushed {
	if !r.isClone(ctx, dir) {
		return nil
	}
	startRef, err := r.gitOutput(ctx, dir, "config", "--local", "--get", startRefKey)
	if err != nil || startRef == "" {
		return nil
	}
	branch := protocol.TaskBranch(task)
	report := &protocol.BranchPushed{Branch: branch}
	commit, err := r.gitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		report.Error = "the workspace has no branch " + branch
		return report
	}
	report.Commit = commit
	ahead, err := r.gitOutput(ctx, dir, "rev-list", "--count", commit, "--not", startCommitRef)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	if report.Ahead, err = strconv.Atoi(ahead); err != nil {
		report.Error = "count the branch's commits: " + err.Error()
		return report
	}
	if report.Uncommitted, err = r.countUncommitted(ctx, dir); err != nil {
		if report.Ahead == 0 {
			return nil
		}
		report.Error = err.Error()
		return report
	}
	if report.Ahead == 0 {
		return uncommittedOnly(report)
	}
	var repo string
	if r.mirrors != nil {
		if repo, err = r.mirrors.recorded(task); err != nil {
			report.Error = "read the workspace's repository: " + err.Error()
			return report
		}
	}
	if repo == "" {
		report.Error = "the daemon has no record of the workspace's repository, so it cannot push " + branch
		return report
	}
	mirror := r.mirrors.path(repo)
	defer r.mirrors.lock(mirror)()
	// A mirror deleted since the workspace was cloned is made again: the
	// branch comes from the workspace with all its history.
	if err := r.mirrors.create(ctx, mirror, repo); err != nil {
		report.Error = fmt.Sprintf("mirror of %s: %v", repo, err)
		return report
	}
	remoteHead, remoteCommit, err := r.remoteBranches(ctx, mirror, branch)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	if remoteCommit == commit {
		return uncommittedOnly(report)
	}
	if err := r.pushBranch(ctx, mirror, dir, branch, startRef, remoteHead); err != nil {
		report.Error = err.Error()
	}
	return report
}

// uncommittedOnly is report, of a branch with nothing to push, when the
// workspace holds uncommitted files, or else nil.
func uncommittedOnly(report *protocol.BranchPushed) *protocol.BranchPushed {
	if report.Uncommitted == 0 {
		return nil
	}
	return report
}

// pushBranch fetches branch from the workspace dir into the mirror, then
// pushes it from the mirror to the remote, without force. It refuses a
// branch that is the task's start ref, startRef, or the remote's default
// branch, remoteHead, so that the daemon never pushes either. The caller
// holds the mirror's lock.
//
// The fetch replaces the mirror's copy of branch whatever it held: the
// mirror only stages the push, and the push without force is what
// refuses a branch whose history the agent rewrote. Its upload-pack
// runs in the workspace as the user who owns it, the harness user when
// there is one, since git runs what a repository's configuration names
// (git help git, SECURITY).
func (r runner) pushBranch(ctx context.Context, mirror, dir, branch, startRef, remoteHead string) error {
	full := "refs/heads/" + branch
	if branch == startRef || full == startRef {
		return fmt.Errorf("refused to push %s: it is the ref the task started from", branch)
	}
	if full == remoteHead {
		return fmt.Errorf("refused to push %s: it is the remote's default branch", branch)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("workspace %s is not an absolute path", dir)
	}
	fetch := []string{"fetch", "--quiet", "--no-write-fetch-head"}
	if r.as.Other() {
		argv := r.as.SudoArgv("", r.git(), []string{"upload-pack"}, gitEnv)
		quoted := make([]string, len(argv))
		for i, arg := range argv {
			quoted[i] = shellQuote(arg)
		}
		fetch = append(fetch, "--upload-pack="+strings.Join(quoted, " "))
	}
	// The workspace is a local repository; the daemon's ssh agent has no
	// business there, nor, through sudo, with the harness user.
	if _, err := r.mirrors.git(ctx, mirror, []string{"SSH_AUTH_SOCK="}, append(fetch, dir, "+"+full+":"+full)...); err != nil {
		return err
	}
	_, err := r.mirrors.git(ctx, mirror, nil, "push", "--quiet", "origin", full+":"+full)
	return err
}

// remoteBranches returns the ref the remote of mirror names as its HEAD,
// such as refs/heads/main, and the commit its branch holds; either is
// empty when the remote has none.
func (r runner) remoteBranches(ctx context.Context, mirror, branch string) (head, commit string, err error) {
	out, err := r.mirrors.git(ctx, mirror, nil, "ls-remote", "--symref", "origin", "HEAD", "refs/heads/"+branch)
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
func (r runner) countUncommitted(ctx context.Context, dir string) (int, error) {
	out, err := r.gitOutput(ctx, dir, "status", "--porcelain", "--untracked-files=all")
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
