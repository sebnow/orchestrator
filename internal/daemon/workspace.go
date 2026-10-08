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
// The repository must be an https:// or ssh URL. git runs with prompts
// turned off and ignores the user's and the system's git configuration,
// whose credential helpers and URL rewrites would otherwise apply; ssh
// still reads the daemon user's ~/.ssh. A failed clone leaves no
// directory behind.
func prepareWorkspace(ctx context.Context, dir string, ws *protocol.Workspace) error {
	if ws == nil {
		return os.MkdirAll(dir, 0o700)
	}
	if err := requireRemote(ws.Repo); err != nil {
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

// runGit runs git in dir. GIT_SSH_COMMAND puts ssh in batch mode, so that
// an unknown host key or a key's passphrase fails the command rather than
// waits for an answer nobody gives.
func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
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

// deleteWorkspace deletes the directory task works in under stateDir,
// with everything in it. Every workspace a task has worked in is deleted
// through it, so that whatever must happen to a workspace's work first
// can happen here. A workspace that does not exist is not an error.
func deleteWorkspace(stateDir string, task protocol.TaskID) error {
	if err := os.RemoveAll(workspacePath(stateDir, task)); err != nil {
		return fmt.Errorf("delete the workspace of task %s: %w", task, err)
	}
	return nil
}
