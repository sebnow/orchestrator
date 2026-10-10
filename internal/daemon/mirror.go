package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// The daemon keeps a bare mirror of each repository its tasks use, owned
// by its own user. Every git command against a remote runs there, as the
// daemon's user and with the daemon's key; a task's workspace is cloned
// from the mirror, so the harness user never reaches the remote
// (docs/adr/2026-10-10-daemon-push-identity.md).

const (
	// mirrorsDirName, under the state directory, holds the mirrors. It
	// and the mirrors are readable by every user, so that the harness
	// user can clone from them; the state directory lets that user pass
	// through to it by name without listing anything.
	mirrorsDirName = "mirrors"
	// workspaceReposDirName, under the state directory, records the
	// repository each task's workspace was cloned for, out of the
	// agent's reach.
	workspaceReposDirName = "workspace-repos"
	// mirrorFileMode is the mode of the mirrors' files, as git's
	// core.sharedRepository takes it; git gives directories the
	// matching execute bits. It holds whatever the daemon's umask.
	mirrorFileMode = "0644"
)

// mirrors are the daemon's bare mirrors, one per repository URL, under
// dir.
type mirrors struct {
	dir   string
	repos string
	// env is what git against a remote runs with besides the daemon's
	// environment.
	env []string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// newMirrors returns the mirrors under stateDir. git reaches a remote
// over ssh with key, the daemon's private key file, unless key is empty;
// with identitiesOnly ssh offers that key alone, and otherwise also the
// keys of the daemon's ssh agent. ssh checks the remote's host key
// against knownHosts, the files the server's host keys are written to
// and any other the daemon's user keeps, and nowhere else but the
// system's known_hosts; it refuses a host it does not find there.
func newMirrors(stateDir, key string, identitiesOnly bool, knownHosts ...string) *mirrors {
	ssh := "ssh"
	if key != "" {
		ssh += " -i " + shellQuote(key)
	}
	ssh += " -o BatchMode=yes"
	if identitiesOnly {
		ssh += " -o IdentitiesOnly=yes"
	}
	if len(knownHosts) > 0 {
		files := make([]string, len(knownHosts))
		for idx, file := range knownHosts {
			files[idx] = sshQuote(file)
		}
		ssh += " -o StrictHostKeyChecking=yes -o " + shellQuote("UserKnownHostsFile="+strings.Join(files, " "))
	}
	env := slices.DeleteFunc(slices.Clone(gitEnv), func(kv string) bool { return strings.HasPrefix(kv, "GIT_SSH_COMMAND=") })
	return &mirrors{
		dir:   filepath.Join(stateDir, mirrorsDirName),
		repos: filepath.Join(stateDir, workspaceReposDirName),
		env:   append(env, "GIT_SSH_COMMAND="+ssh),
		locks: map[string]*sync.Mutex{},
	}
}

// path returns the mirror of repo: a directory named after the SHA-256
// of the URL, so that the URL needs no escaping.
func (m *mirrors) path(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Join(m.dir, hex.EncodeToString(sum[:])+".git")
}

// lock takes the lock of the mirror at path and returns its release. A
// mirror's refs change in steps that must not interleave: a fetch from
// the remote, and a fetch from a workspace followed by the push.
func (m *mirrors) lock(path string) func() {
	m.mu.Lock()
	l, ok := m.locks[path]
	if !ok {
		l = &sync.Mutex{}
		m.locks[path] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// update brings the mirror of repo up to date with the remote and
// returns its path, creating it first when there is none. It fetches
// every branch and tag, so that a workspace can start at any of them or
// at a commit they hold, and continue a task branch another daemon
// pushed. It does not prune: the branches of tasks whose push failed
// stay in the mirror.
func (m *mirrors) update(ctx context.Context, repo string) (string, error) {
	path := m.path(repo)
	defer m.lock(path)()
	if err := m.create(ctx, path, repo); err != nil {
		return "", fmt.Errorf("mirror of %s: %w", repo, err)
	}
	if _, err := m.git(ctx, path, nil, "fetch", "--quiet", "--no-write-fetch-head", "origin", "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return "", err
	}
	return path, nil
}

// create makes the bare mirror of repo at path unless it exists. It is
// made under a temporary name and renamed into place, so that a failure
// leaves no mirror half made.
func (m *mirrors) create(ctx context.Context, path, repo string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	// The mode is set apart from creation, which the umask narrows.
	if err := os.Chmod(m.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(m.dir, ".new-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	if _, err := m.git(ctx, "", nil, "init", "--quiet", "--bare", "--shared="+mirrorFileMode, "--", tmp); err != nil {
		return err
	}
	for _, kv := range [][2]string{
		{"remote.origin.url", repo},
		// git's housekeeping after a fetch runs before the fetch returns,
		// under the mirror's lock, rather than in a process left behind.
		{"gc.autoDetach", "false"},
		{"maintenance.autoDetach", "false"},
	} {
		if _, err := m.git(ctx, tmp, nil, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return os.Rename(tmp, path)
}

// git runs git as the daemon's user in dir, or where the daemon runs
// when dir is empty, with the settings for reaching the remote and vars
// added, and returns its standard output, trimmed. git runs in a
// session of its own, so that a sudo it starts has no terminal.
func (m *mirrors) git(ctx context.Context, dir string, vars []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), m.env...), vars...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// record notes that the workspace of task was cloned for repo.
func (m *mirrors) record(task protocol.TaskID, repo string) error {
	if err := os.MkdirAll(m.repos, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.repos, string(task)), []byte(repo+"\n"), 0o600)
}

// recorded returns the repository the workspace of task was cloned for,
// or "" when none is recorded.
func (m *mirrors) recorded(task protocol.TaskID) (string, error) {
	data, err := os.ReadFile(filepath.Join(m.repos, string(task)))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(data)), err
}

// forget deletes the record of task's workspace. Forgetting what is not
// recorded succeeds.
func (m *mirrors) forget(task protocol.TaskID) error {
	err := os.Remove(filepath.Join(m.repos, string(task)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// sshQuote quotes s, when it holds a space, a tab or a double quote, as
// one argument of an ssh option that takes several, such as
// UserKnownHostsFile.
func sshQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// shellQuote quotes s as one word for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
