package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/pki"
)

// runCommand runs the command with args and returns its status and what
// it wrote to stdout and stderr.
func runCommand(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	status := run(args, &stdout, &stderr)
	return status, stdout.String(), stderr.String()
}

func mustRun(t *testing.T, args ...string) {
	t.Helper()
	if status, stdout, stderr := runCommand(args...); status != 0 {
		t.Fatalf("%v: status %d\n%s%s", args, status, stdout, stderr)
	}
}

func TestGivenNewPKIDirWhenTheCAAndCertificatesAreIssuedThenTheyVerifyAgainstTheCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	mustRun(t, "init-ca", "-pki-dir", dir)
	mustRun(t, "issue-server-cert", "-pki-dir", dir, "-host", "orchestrator.example", "-host", "192.0.2.10")
	mustRun(t, "issue-daemon-cert", "-pki-dir", dir, "-id", "vps-1")

	server, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	daemonDir := filepath.Join(dir, "daemons", "vps-1")
	daemon, err := tls.LoadX509KeyPair(filepath.Join(daemonDir, "daemon.crt"), filepath.Join(daemonDir, "daemon.key"))
	if err != nil {
		t.Fatal(err)
	}
	// The daemon verifies the server with the CA copied beside its key.
	roots, err := pki.LoadPool(filepath.Join(daemonDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"orchestrator.example", "192.0.2.10"} {
		if _, err := server.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host}); err != nil {
			t.Errorf("server certificate for %s: %v", host, err)
		}
	}
	_, err = daemon.Leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Errorf("daemon certificate: %v", err)
	}
	if id, err := pki.DaemonID(daemon.Leaf); err != nil || id != "vps-1" {
		t.Errorf("daemon id = %q, %v; want vps-1", id, err)
	}
}

func TestGivenIssuedDaemonCertificateWhenIssuedAgainThenItIsRefusedAndKept(t *testing.T) {
	dir := t.TempDir()
	mustRun(t, "init-ca", "-pki-dir", dir)
	mustRun(t, "issue-daemon-cert", "-pki-dir", dir, "-id", "laptop")

	status, _, stderr := runCommand("issue-daemon-cert", "-pki-dir", dir, "-id", "laptop")
	if status != 1 || !strings.Contains(stderr, "exists") {
		t.Errorf("status %d, stderr %q; want 1 saying the file exists", status, stderr)
	}
}

func TestGivenMissingFlagsWhenASubcommandRunsThenItPrintsItsUsage(t *testing.T) {
	for _, args := range [][]string{
		{"init-ca"},
		{"issue-server-cert", "-pki-dir", t.TempDir()},
		{"issue-daemon-cert", "-pki-dir", t.TempDir()},
	} {
		status, _, stderr := runCommand(args...)
		if status != 2 || !strings.Contains(stderr, "Usage: server "+args[0]) {
			t.Errorf("%v: status %d, stderr %q; want 2 and the usage", args, status, stderr)
		}
	}
}

func TestGivenUnknownSubcommandWhenRunThenItListsTheSubcommands(t *testing.T) {
	status, _, stderr := runCommand("issue-cert")
	if status != 2 || !strings.Contains(stderr, "init-ca, issue-daemon-cert, issue-server-cert") {
		t.Errorf("status %d, stderr %q; want 2 and the subcommands", status, stderr)
	}
}
