// Package sshagent lets the harness user reach the daemon's ssh agent
// (docs/adr/2026-10-08-harness-user.md). The agent's own socket is open
// only to the daemon's user; a Forwarder serves a socket the harness
// user can open and relays each connection to the agent byte for byte.
package sshagent

import (
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
)

const (
	// dirMode lets every user reach the socket by its name, and nobody
	// but the daemon's user list the directory to learn the name.
	dirMode = 0o711
	// socketMode lets every user who reaches the socket connect to it;
	// connecting needs write permission.
	socketMode = 0o666
)

// Forwarder relays connections on its socket to an ssh agent's socket.
type Forwarder struct {
	dir      string
	path     string
	target   string
	listener *net.UnixListener

	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

// Forward listens on a new socket in a new directory under parent and
// relays every connection to the agent socket target. The directory has
// mode 0711 and the socket a random name and mode 0666, so that a user
// without a group in common with the daemon's can connect once told the
// socket's path, and no other user can list the directory to find it.
func Forward(parent, target string) (*Forwarder, error) {
	dir, err := os.MkdirTemp(parent, "orchestrator-agent-")
	if err != nil {
		return nil, err
	}
	f, err := listen(dir, target)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	// serve counts in wg, so that the relays it adds are waited for too.
	f.wg.Go(f.serve)
	return f, nil
}

func listen(dir, target string) (*Forwarder, error) {
	if err := os.Chmod(dir, dirMode); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "agent-"+rand.Text()+".sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, socketMode); err != nil {
		listener.Close()
		return nil, err
	}
	return &Forwarder{dir: dir, path: path, target: target, listener: listener, conns: map[net.Conn]struct{}{}}, nil
}

// Path is the socket to give the harness user as SSH_AUTH_SOCK.
func (f *Forwarder) Path() string {
	return f.path
}

// Close stops listening, ends every relayed connection, and removes the
// socket and its directory.
func (f *Forwarder) Close() error {
	err := f.listener.Close()
	f.mu.Lock()
	f.closed = true
	for conn := range f.conns {
		conn.Close()
	}
	f.mu.Unlock()
	f.wg.Wait()
	return errors.Join(err, os.RemoveAll(f.dir))
}

func (f *Forwarder) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		if !f.track(conn) {
			conn.Close()
			return
		}
		f.wg.Go(func() {
			defer f.untrack(conn)
			f.relay(conn)
		})
	}
}

// track registers conn so Close can end it; it reports false once Close
// has begun.
func (f *Forwarder) track(conn net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.conns[conn] = struct{}{}
	return true
}

// untrack closes conn and forgets it.
func (f *Forwarder) untrack(conn net.Conn) {
	f.mu.Lock()
	delete(f.conns, conn)
	f.mu.Unlock()
	conn.Close()
}

// relay copies bytes both ways between client and the agent until both
// directions have ended, passing each side's end of writing on to the
// other.
func (f *Forwarder) relay(client net.Conn) {
	agent, err := net.Dial("unix", f.target)
	if err != nil {
		return
	}
	if !f.track(agent) {
		agent.Close()
		return
	}
	defer f.untrack(agent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(agent, client)
		agent.(*net.UnixConn).CloseWrite()
	}()
	io.Copy(client, agent)
	client.(*net.UnixConn).CloseWrite()
	<-done
}
