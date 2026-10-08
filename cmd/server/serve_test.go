package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGivenInsecureLoopbackWhenListenIsNotALoopbackIPAddressThenTheServerRefusesToStart(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8080", "[::]:8080", ":8080", "localhost:8080", "192.0.2.10:8080"} {
		dbPath := filepath.Join(t.TempDir(), "server.db")

		status, _, stderr := runCommand("-insecure-loopback", "-listen", listen, "-db", dbPath)
		if status != 2 || !strings.Contains(stderr, "loopback") {
			t.Errorf("-listen %s: status %d, stderr %q; want 2 naming loopback", listen, status, stderr)
		}
		if _, err := os.Stat(dbPath); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("-listen %s: the database was opened: %v", listen, err)
		}
	}
}

func TestGivenLoopbackIPAddressWhenCheckedThenItIsAccepted(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:8080", "127.0.0.2:0", "[::1]:8080"} {
		if err := requireLoopback(listen); err != nil {
			t.Errorf("%s: %v", listen, err)
		}
	}
}

func TestGivenNoTLSFlagsWhenNotInsecureThenTheServerRefusesToStart(t *testing.T) {
	status, _, stderr := runCommand("-db", filepath.Join(t.TempDir(), "server.db"))
	if status != 2 || !strings.Contains(stderr, "-tls-cert, -tls-key and -client-ca are required") {
		t.Errorf("status %d, stderr %q; want 2 naming the TLS flags", status, stderr)
	}
}

func TestGivenTLSFlagsWhenInsecureLoopbackThenTheServerRefusesToStart(t *testing.T) {
	status, _, stderr := runCommand("-insecure-loopback", "-db", filepath.Join(t.TempDir(), "server.db"), "-tls-cert", "server.crt")
	if status != 2 || !strings.Contains(stderr, "plain HTTP") {
		t.Errorf("status %d, stderr %q; want 2", status, stderr)
	}
}

func TestGivenTLSButNoOwnerTokenWhenStartingThenTheServerRefusesAndSaysHowToIssueOne(t *testing.T) {
	dir := t.TempDir()
	mustRun(t, "init-ca", "-pki-dir", dir)
	mustRun(t, "issue-server-cert", "-pki-dir", dir, "-host", "127.0.0.1")

	status, _, stderr := runCommand("-listen", "127.0.0.1:0", "-db", filepath.Join(dir, "server.db"),
		"-tls-cert", filepath.Join(dir, "server.crt"), "-tls-key", filepath.Join(dir, "server.key"),
		"-client-ca", filepath.Join(dir, "ca.crt"))
	if status != 1 || !strings.Contains(stderr, "issue-owner-token") {
		t.Errorf("status %d, stderr %q; want 1 naming issue-owner-token", status, stderr)
	}
}
