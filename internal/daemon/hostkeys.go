package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// knownHostsName, under the state directory, is the known_hosts file
// that the host keys the server sends are written to, and that the
// daemon's ssh checks a remote's host key against.
const knownHostsName = "known_hosts"

// errInvalidHostKeyLine refuses a host key line that would be more than
// one line of the file.
var errInvalidHostKeyLine = errors.New("a host key line holds a line break")

// knownHostsFiles are the files the daemon's ssh checks a remote's host
// key against: the server's host keys under stateDir, and, without a
// harness user, the daemon user's own ~/.ssh/known_hosts as well, so
// that a daemon on the owner's machine still reaches the hosts the owner
// has. home is the daemon user's home directory, "" for none.
func knownHostsFiles(stateDir, home string, harnessUser bool) []string {
	files := []string{filepath.Join(stateDir, knownHostsName)}
	if !harnessUser && home != "" {
		files = append(files, filepath.Join(home, ".ssh", "known_hosts"))
	}
	return files
}

// writeKnownHosts replaces the known_hosts file at path with lines,
// readable by the daemon's user only. It writes a temporary file and
// renames it into place, so that ssh never reads a file half written.
func writeKnownHosts(path string, lines []string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".known_hosts-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	var text strings.Builder
	for _, line := range lines {
		text.WriteString(line + "\n")
	}
	if _, err := tmp.WriteString(text.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// applyHostKeys writes the host keys command carries to the daemon's
// known_hosts. One that cannot be written is logged; the server sends
// the keys again when the daemon next connects.
func (s *service) applyHostKeys(command protocol.Command) {
	keys, err := decodePayload[protocol.HostKeys](command)
	if err == nil {
		for _, line := range keys.Lines {
			if strings.ContainsAny(line, "\r\n") {
				err = errInvalidHostKeyLine
				break
			}
		}
	}
	if err == nil {
		err = writeKnownHosts(filepath.Join(s.cfg.StateDir, knownHostsName), keys.Lines)
	}
	if err != nil {
		s.log.Warn("command not applied", "command", command.ID, "kind", command.Kind, "error", err)
		return
	}
	s.log.Info("host keys written", "command", command.ID, "lines", len(keys.Lines))
}
