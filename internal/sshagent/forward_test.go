package sshagent_test

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/sshagent"
)

// shortTempDir returns a directory under /tmp: a unix socket's path must
// fit in about a hundred bytes, which t.TempDir on macOS exceeds.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sshagent-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// fakeAgent listens on a socket under dir and answers each connection
// with "agent got " and everything the connection sent, once the sender
// has closed its writing side.
func fakeAgent(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "agent.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				got, _ := io.ReadAll(conn)
				conn.Write(append([]byte("agent got "), got...))
			}()
		}
	}()
	return path
}

func startForwarder(t *testing.T) (*sshagent.Forwarder, string) {
	t.Helper()
	dir := shortTempDir(t)
	f, err := sshagent.Forward(dir, fakeAgent(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f, dir
}

func TestGivenForwarderWhenAClientTalksToItThenTheBytesReachTheAgentAndItsAnswerComesBack(t *testing.T) {
	f, _ := startForwarder(t)

	conn, err := net.Dial("unix", f.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := "\x00\x00\x00\x01\x0b"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	conn.(*net.UnixConn).CloseWrite()
	answer, err := io.ReadAll(conn)

	if err != nil || string(answer) != "agent got "+request {
		t.Errorf("answer = %q, %v; want the agent's answer to the request", answer, err)
	}
}

func TestGivenForwarderThenItsSocketIsOpenToEveryoneInADirectoryOthersCanEnterButNotList(t *testing.T) {
	f, parent := startForwarder(t)

	socket, err := os.Stat(f.Path())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Stat(filepath.Dir(f.Path()))
	if err != nil {
		t.Fatal(err)
	}

	if socket.Mode().Type() != os.ModeSocket || socket.Mode().Perm() != 0o666 {
		t.Errorf("socket mode = %s; want a socket with mode 0666", socket.Mode())
	}
	if dir.Mode().Perm() != 0o711 || filepath.Dir(filepath.Dir(f.Path())) != parent {
		t.Errorf("directory %s mode = %s; want 0711 under %s", filepath.Dir(f.Path()), dir.Mode(), parent)
	}
	if name := filepath.Base(f.Path()); len(name) < len("agent-.sock")+20 || !strings.HasPrefix(name, "agent-") {
		t.Errorf("socket name = %q; want one that cannot be guessed", name)
	}
}

func TestGivenForwarderWhenClosedThenItsDirectoryIsGoneAndOpenRelaysEnd(t *testing.T) {
	dir := shortTempDir(t)
	block, err := net.Listen("unix", filepath.Join(dir, "silent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer block.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := block.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	f, err := sshagent.Forward(dir, filepath.Join(dir, "silent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", f.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	agentSide := <-accepted
	defer agentSide.Close()

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Dir(f.Path())); !os.IsNotExist(err) {
		t.Errorf("stat directory after close = %v; want it gone", err)
	}
	if n, err := conn.Read(make([]byte, 1)); err == nil {
		t.Errorf("read from a closed relay = %d bytes; want its end", n)
	}
}
