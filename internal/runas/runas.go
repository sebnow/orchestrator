// Package runas builds commands that run as the daemon's own OS user or,
// through sudo, as the harness user
// (docs/adr/2026-10-08-harness-user.md).
package runas

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

// TempDir is a directory every user on the machine can reach, for files
// the daemon hands to the harness user. os.TempDir does not do: on
// macOS it is private to the user running the daemon.
const TempDir = "/tmp"

// waitDelay bounds the wait for a command's output once it has been
// sent SIGTERM because its context ended.
const waitDelay = 10 * time.Second

// User is the OS user commands run as. The zero User is the daemon's own
// user.
type User struct {
	// Name is the harness user's login name; empty runs commands as the
	// daemon's own user.
	Name string
	// SSHAuthSock is the ssh agent socket commands run as Name get as
	// SSH_AUTH_SOCK; empty gives them none.
	SSHAuthSock string
	// Sudo is the sudo executable; empty uses sudo on PATH.
	Sudo string
}

// Other reports whether u is a user other than the daemon's own.
func (u User) Other() bool {
	return u.Name != ""
}

// Command returns a command that runs path with args as u, in dir, which
// empty leaves to the caller's choice. vars are VAR=value settings the
// command needs.
//
// As the daemon's own user, path runs directly, with env followed by
// vars as its environment.
//
// As another user, the command is
//
//	sudo -n -u NAME -D DIR VAR=value... -- PATH ARGS...
//
// with DIR "/" when dir is empty, since the daemon's own working
// directory may be closed to that user. sudo refuses a VAR=value that
// sudoers does not keep, rather than dropping it. sudo's own environment
// holds only env's PATH and SSH_AUTH_SOCK set to u.SSHAuthSock, which
// sudo passes on only when sudoers keeps it; sudo sets the user's HOME,
// SHELL, LOGNAME and USER itself. sudo runs in a session of its own, so
// that it has no terminal and passes the command's standard input and
// output through. sudo relays SIGTERM but not SIGKILL to the command,
// so cancelling ctx sends sudo SIGTERM.
func (u User) Command(ctx context.Context, dir, path string, args, env, vars []string) *exec.Cmd {
	if !u.Other() {
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Dir = dir
		cmd.Env = slices.Concat(env, vars)
		return cmd
	}
	argv := u.SudoArgv(dir, path, args, vars)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{}
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	if u.SSHAuthSock != "" {
		cmd.Env = append(cmd.Env, "SSH_AUTH_SOCK="+u.SSHAuthSock)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = waitDelay
	return cmd
}

// SudoArgv returns the command line that runs path with args as u, the
// other user, in dir: sudo and its arguments, as Command runs them, for
// a command that another program starts, such as the upload-pack that
// git fetch runs.
func (u User) SudoArgv(dir, path string, args, vars []string) []string {
	if dir == "" {
		dir = "/"
	}
	sudo := u.Sudo
	if sudo == "" {
		sudo = "sudo"
	}
	return slices.Concat([]string{sudo, "-n", "-u", u.Name, "-D", dir}, vars, []string{"--", path}, args)
}
