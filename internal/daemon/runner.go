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
)

// gitEnv turns git's prompts off and keeps git off the user's and the
// system's git configuration, whose credential helpers and URL rewrites
// would otherwise apply. GIT_SSH_COMMAND puts ssh in batch mode, so that
// an unknown host key or a key's passphrase fails the command rather
// than waits for an answer nobody gives.
var gitEnv = []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_SSH_COMMAND=ssh -o BatchMode=yes"}

// runner runs the commands that touch task workspaces, as the daemon's
// own user, with git from PATH.
type runner struct{}

func (r runner) runGit(ctx context.Context, dir string, args ...string) error {
	return r.runGitTo(ctx, dir, nil, args...)
}

// runGitTo runs git in dir, writing its standard output to stdout.
func (r runner) runGitTo(ctx context.Context, dir string, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv...)
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

// isClone reports whether dir holds a git clone.
func (r runner) isClone(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// makeDir creates the empty directory dir.
func (r runner) makeDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// remove deletes path with everything in it. Removing what does not
// exist succeeds.
func (r runner) remove(path string) error {
	return os.RemoveAll(path)
}
