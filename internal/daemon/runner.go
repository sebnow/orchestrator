package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/runas"
)

// gitEnv turns git's prompts off and keeps git off the user's and the
// system's git configuration, whose credential helpers and URL rewrites
// would otherwise apply. GIT_SSH_COMMAND puts ssh in batch mode, so that
// an unknown host key or a key's passphrase fails the command rather
// than waits for an answer nobody gives.
var gitEnv = []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_SSH_COMMAND=ssh -o BatchMode=yes"}

// removeTimeout bounds deleting a workspace as the harness user.
const removeTimeout = time.Minute

// runner runs the commands that touch task workspaces. The zero runner
// runs them as the daemon's own user, with git from PATH. A runner for a
// harness user runs every one of them as that user, through sudo, with
// git and rm named by absolute path: git runs commands that a
// repository's configuration and hooks name (git help git, SECURITY),
// and the agent can write every workspace
// (docs/adr/2026-10-08-harness-user.md).
type runner struct {
	as     runas.User
	gitCmd string
	rmCmd  string
	// mirrors are the daemon's mirrors of the task repositories, which
	// every git command against a remote runs in, as the daemon's own
	// user; nil has none, and a task with a repository cannot start.
	mirrors *mirrors
}

// newRunner returns the runner for commands run as user. For a harness
// user it finds git and rm on PATH, by the absolute paths the sudoers
// rule must name.
func newRunner(user runas.User) (runner, error) {
	if !user.Other() {
		return runner{}, nil
	}
	r := runner{as: user}
	for name, path := range map[string]*string{"git": &r.gitCmd, "rm": &r.rmCmd} {
		found, err := exec.LookPath(name)
		if err == nil && !filepath.IsAbs(found) {
			err = fmt.Errorf("%s is not an absolute path", found)
		}
		if err != nil {
			return runner{}, fmt.Errorf("find %s for the harness user: %w", name, err)
		}
		*path = found
	}
	return r, nil
}

func (r runner) git() string {
	if r.gitCmd == "" {
		return "git"
	}
	return r.gitCmd
}

func (r runner) runGit(ctx context.Context, dir string, args ...string) error {
	return r.runGitTo(ctx, dir, nil, args...)
}

// runGitTo runs git in dir, which empty leaves to the runner's choice,
// writing its standard output to stdout.
func (r runner) runGitTo(ctx context.Context, dir string, stdout io.Writer, args ...string) error {
	cmd := r.as.Command(ctx, dir, r.git(), args, os.Environ(), gitEnv)
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// gitOutput runs git as runGit does and returns its standard output,
// trimmed.
func (r runner) gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	var stdout bytes.Buffer
	if err := r.runGitTo(ctx, dir, &stdout, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// isClone reports whether dir holds a git clone. As the harness user
// git names the repository itself, so that it does not look for one in
// the directories above dir.
func (r runner) isClone(ctx context.Context, dir string) bool {
	if !r.as.Other() {
		_, err := os.Stat(filepath.Join(dir, ".git"))
		return err == nil
	}
	return r.runGit(ctx, "", "--git-dir", filepath.Join(dir, ".git"), "rev-parse", "--git-dir") == nil
}

// makeDir creates the empty directory dir. The harness user may run only
// git and rm, so git creates the directory with a repository in it, which
// rm then deletes.
func (r runner) makeDir(ctx context.Context, dir string) error {
	if !r.as.Other() {
		return os.MkdirAll(dir, 0o700)
	}
	if err := r.runGit(ctx, "", "init", "--quiet", "--", dir); err != nil {
		return err
	}
	return r.remove(filepath.Join(dir, ".git"))
}

// remove deletes path with everything in it. Removing what does not
// exist succeeds. It runs to the end even once the daemon is stopping,
// within removeTimeout.
func (r runner) remove(path string) error {
	if !r.as.Other() {
		return os.RemoveAll(path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	cmd := r.as.Command(ctx, "", r.rmCmd, []string{"-rf", "--", path}, os.Environ(), nil)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rm %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// cloneTemplate returns the arguments that give a clone made as the
// harness user the pre-push hook for branch, and a function that deletes
// what they refer to. The daemon cannot write into a clone the harness
// user owns, so git copies the hook from a template directory that the
// harness user can read. The zero runner installs the hook itself after
// cloning, and returns no arguments.
func (r runner) cloneTemplate(branch string) ([]string, func(), error) {
	if !r.as.Other() {
		return nil, func() {}, nil
	}
	dir, err := os.MkdirTemp(runas.TempDir, "orchestrator-template-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	hooks := filepath.Join(dir, "hooks")
	hook := filepath.Join(hooks, "pre-push")
	err = os.Mkdir(hooks, 0o755)
	if err == nil {
		err = os.WriteFile(hook, []byte(prePushHook(branch)), 0o755)
	}
	// The modes are set apart from creation, which the umask narrows.
	for _, path := range []string{dir, hooks, hook} {
		if err == nil {
			err = os.Chmod(path, 0o755)
		}
	}
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("write the clone's hook template: %w", err)
	}
	return []string{"--template", dir}, cleanup, nil
}

// harnessFiles returns the directory for files the harness process reads,
// such as its system prompt (harness.Spec.FileDir), and a function that
// deletes it. As the daemon's own user it is "harness" under stateDir,
// emptied of what an earlier run left. The harness user cannot enter
// the state directory, so for that user it is a new directory under
// runas.TempDir that every user may enter but none may list.
func (r runner) harnessFiles(stateDir string) (string, func(), error) {
	if !r.as.Other() {
		dir := filepath.Join(stateDir, "harness")
		err := os.RemoveAll(dir)
		if err == nil {
			err = os.Mkdir(dir, 0o700)
		}
		if err != nil {
			return "", nil, fmt.Errorf("harness file directory: %w", err)
		}
		return dir, func() {}, nil
	}
	dir, err := os.MkdirTemp(runas.TempDir, "orchestrator-harness-")
	if err != nil {
		return "", nil, fmt.Errorf("harness file directory: %w", err)
	}
	cleanup := func() { os.RemoveAll(dir) }
	// The mode is set apart from creation, which the umask narrows.
	if err := os.Chmod(dir, 0o711); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("harness file directory: %w", err)
	}
	return dir, cleanup, nil
}
