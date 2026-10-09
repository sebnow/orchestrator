package daemon

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// prepareWorkspace makes dir the directory task works in: an empty
// directory when ws is nil, otherwise a clone of ws.Repo on task's
// branch, which starts at ws.Ref or, when the remote has it already, is
// the remote's (docs/adr/2026-10-08-work-delivery.md). A ref that names
// a branch or tag is cloned shallow; any other ref, such as a commit,
// needs a full clone and a checkout.
//
// The repository must be an https:// or ssh URL. git runs with prompts
// turned off and ignores the user's and the system's git configuration,
// whose credential helpers and URL rewrites would otherwise apply; ssh
// still reads the ~/.ssh of the user git runs as. A failed clone leaves
// no directory behind. As the harness user, the directory dir is in
// must exist already.
//
// gitName and gitEmail become the clone's local user.name and user.email,
// for the agent's own commits; either empty uses the default identity
// "orchestrator" <orchestrator@localhost>.
func (r runner) prepareWorkspace(ctx context.Context, dir string, task protocol.TaskID, ws *protocol.Workspace, gitName, gitEmail string) error {
	if ws == nil {
		return r.makeDir(ctx, dir)
	}
	if err := requireRemote(ws.Repo); err != nil {
		return err
	}
	// git would read a leading dash as an option.
	if strings.HasPrefix(ws.Ref, "-") {
		return fmt.Errorf("ref %q is not a ref", ws.Ref)
	}
	if !r.as.Other() {
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return err
		}
	}
	template, cleanup, err := r.cloneTemplate(taskBranch(task))
	if err != nil {
		return err
	}
	defer cleanup()
	clone := append([]string{"clone", "--quiet"}, template...)
	err = r.runGit(ctx, "", slices.Concat(clone, []string{"--depth", "1", "--branch", ws.Ref, "--", ws.Repo, dir})...)
	if err != nil {
		r.remove(dir)
		err = r.runGit(ctx, "", slices.Concat(clone, []string{"--", ws.Repo, dir})...)
		if err == nil {
			err = r.runGit(ctx, dir, "checkout", "--quiet", "--detach", ws.Ref)
		}
	}
	if err == nil {
		err = r.prepareBranch(ctx, dir, task, ws.Ref, gitName, gitEmail)
	}
	if err != nil {
		r.remove(dir)
		return err
	}
	return nil
}

// requireRemote refuses a repository that is not an https:// URL, an
// ssh:// URL or an scp-like ssh address such as git@host:path, each with
// a host. A local path, file:// or git:// would let a task read the
// daemon machine's files or an unauthenticated remote.
func requireRemote(repo string) error {
	refused := fmt.Errorf("repository %q is not an https:// URL, an ssh:// URL or an ssh address such as git@host:path", repo)
	if strings.Contains(repo, "://") {
		u, err := url.Parse(repo)
		if err != nil || u.Scheme != "https" && u.Scheme != "ssh" || u.Hostname() == "" || strings.HasPrefix(u.Hostname(), "-") {
			return refused
		}
		return nil
	}
	// git reads host:path as ssh only when no slash precedes the colon.
	address, path, found := strings.Cut(repo, ":")
	if !found || path == "" || strings.Contains(address, "/") {
		return refused
	}
	host := address
	if _, after, ok := strings.Cut(address, "@"); ok {
		host = after
	}
	if host == "" || strings.HasPrefix(host, "-") || strings.HasPrefix(address, "-") {
		return refused
	}
	return nil
}

// deleteWorkspace deletes the directory dir that task works in, with
// everything in it. Every workspace a task has worked in is deleted
// through it, so that its work is delivered first: a workspace whose
// task branch holds commits the remote lacks is pushed, and kept when
// the push fails, with an error wrapping errWorkspaceKept. A workspace
// that does not exist is not an error.
func (r runner) deleteWorkspace(dir string, task protocol.TaskID) error {
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	if report := r.deliver(ctx, dir, task); report != nil && report.Error != "" {
		return fmt.Errorf("%w: task %s's branch %s holds work that could not be pushed: %s", errWorkspaceKept, task, report.Branch, report.Error)
	}
	if err := r.remove(dir); err != nil {
		return fmt.Errorf("delete the workspace of task %s: %w", task, err)
	}
	return nil
}
