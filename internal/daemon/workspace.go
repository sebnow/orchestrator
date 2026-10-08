package daemon

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// prepareWorkspace makes dir the directory a task works in: an empty
// directory when ws is nil, otherwise a clone of ws.Repo checked out at
// ws.Ref. A ref that names a branch or tag is cloned shallow; any other
// ref, such as a commit, needs a full clone and a checkout.
//
// The repository must be an https:// URL. git runs with no credentials
// of the daemon's and with prompts turned off, so a repository that needs credentials fails rather than waits
// (docs/adr/2026-10-07-task-credentials.md). It ignores the user's and
// the system's git configuration, whose credential helpers and URL
// rewrites would otherwise apply. A failed clone leaves no directory
// behind.
func prepareWorkspace(ctx context.Context, dir string, ws *protocol.Workspace) error {
	if ws == nil {
		return os.MkdirAll(dir, 0o700)
	}
	if err := requireHTTPS(ws.Repo); err != nil {
		return err
	}
	// git would read a leading dash as an option.
	if strings.HasPrefix(ws.Ref, "-") {
		return fmt.Errorf("ref %q is not a ref", ws.Ref)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if runGit(ctx, "", "clone", "--quiet", "--depth", "1", "--branch", ws.Ref, "--", ws.Repo, dir) == nil {
		return nil
	}
	os.RemoveAll(dir)
	err := runGit(ctx, "", "clone", "--quiet", "--", ws.Repo, dir)
	if err == nil {
		err = runGit(ctx, dir, "checkout", "--quiet", "--detach", ws.Ref)
	}
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	return nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// requireHTTPS refuses a repository that is not an https:// URL. An ssh
// or local repository could be read with the ssh keys or the files of the
// OS user running the daemon, which a task must not borrow
// (docs/adr/2026-10-07-task-credentials.md).
func requireHTTPS(repo string) error {
	u, err := url.Parse(repo)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("repository %q is not an https:// URL; the daemon clones public repositories over https only", repo)
	}
	return nil
}

// deleteWorkspace deletes the directory task works in under stateDir,
// with everything in it. It is the one place the daemon deletes a
// workspace, so that whatever must happen to a workspace's work first
// happens here. A workspace that does not exist is not an error.
func deleteWorkspace(stateDir string, task protocol.TaskID) error {
	if err := os.RemoveAll(workspacePath(stateDir, task)); err != nil {
		return fmt.Errorf("delete the workspace of task %s: %w", task, err)
	}
	return nil
}
