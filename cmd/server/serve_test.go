package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

func TestGivenSchedulingFlagsOutOfRangeWhenStartingThenTheServerRefusesAndNamesTheFlag(t *testing.T) {
	for _, tc := range []struct {
		flag, value string
	}{
		{"-filler-threshold", "-0.1"},
		{"-filler-threshold", "1.5"},
		{"-low-threshold", "2"},
		{"-daemon-timeout", "30s"},
		{"-permissions", "deny-all"},
	} {
		dbPath := filepath.Join(t.TempDir(), "server.db")

		status, _, stderr := runCommand("-insecure-loopback", "-db", dbPath, tc.flag, tc.value)

		if status != 2 || !strings.Contains(stderr, tc.flag) {
			t.Errorf("%s %s: status %d, stderr %q; want 2 naming the flag", tc.flag, tc.value, status, stderr)
		}
		if _, err := os.Stat(dbPath); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s %s: the database was opened: %v", tc.flag, tc.value, err)
		}
	}
}

func TestGivenCAKeyThatIsNotTheClientCAsWhenStartingThenTheServerRefusesAndNamesTheFlag(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	mustRun(t, "init-ca", "-pki-dir", dir)
	mustRun(t, "init-ca", "-pki-dir", other)
	mustRun(t, "issue-server-cert", "-pki-dir", dir, "-host", "127.0.0.1")

	status, _, stderr := runCommand("-listen", "127.0.0.1:0", "-db", filepath.Join(dir, "server.db"),
		"-tls-cert", filepath.Join(dir, "server.crt"), "-tls-key", filepath.Join(dir, "server.key"),
		"-client-ca", filepath.Join(dir, "ca.crt"), "-ca-key", filepath.Join(other, "ca.key"))
	if status != 1 || !strings.Contains(stderr, "-ca-key") {
		t.Errorf("status %d, stderr %q; want 1 naming -ca-key", status, stderr)
	}
}

func TestGivenHetznerTokenWhenStartingWithoutWhatProvisioningNeedsThenTheServerRefusesAndNamesIt(t *testing.T) {
	dir := t.TempDir()
	mustRun(t, "init-ca", "-pki-dir", dir)
	mustRun(t, "issue-server-cert", "-pki-dir", dir, "-host", "127.0.0.1")
	t.Setenv("HETZNER_TOKEN", "test-token-not-a-secret")
	tlsFlags := []string{"-listen", "127.0.0.1:0", "-db", filepath.Join(dir, "server.db"),
		"-tls-cert", filepath.Join(dir, "server.crt"), "-tls-key", filepath.Join(dir, "server.key"),
		"-client-ca", filepath.Join(dir, "ca.crt")}
	withKey := append(slices.Clone(tlsFlags), "-ca-key", filepath.Join(dir, "ca.key"))

	for _, c := range []struct {
		args []string
		want string
	}{
		{tlsFlags, "-ca-key"},
		{append(slices.Clone(withKey), "-daemon-binary-url", "https://example.com/daemon"), "-public-url"},
		{append(slices.Clone(withKey), "-public-url", "https://orchestrator.example:8443"), "-daemon-binary-url"},
		{append(slices.Clone(withKey), "-public-url", "http://orchestrator.example:8443", "-daemon-binary-url", "https://example.com/daemon"), "-public-url"},
	} {
		status, _, stderr := runCommand(c.args...)
		if status != 2 || !strings.Contains(stderr, c.want) || strings.Contains(stderr, "test-token-not-a-secret") {
			t.Errorf("%v: status %d, stderr %q; want 2 naming %s", c.args[len(tlsFlags):], status, stderr, c.want)
		}
	}
}

func TestGivenHetznerTokenFileOthersCanReadWhenStartingThenTheServerRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hetzner-token")
	if err := os.WriteFile(path, []byte("test-token-not-a-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, _, stderr := runCommand("-insecure-loopback", "-db", filepath.Join(t.TempDir(), "server.db"), "-hetzner-token-file", path)
	if status != 1 || !strings.Contains(stderr, "-hetzner-token-file") || strings.Contains(stderr, "test-token-not-a-secret") {
		t.Errorf("status %d, stderr %q; want 1 naming -hetzner-token-file", status, stderr)
	}
}

// TestMain keeps a Hetzner token in the environment of whoever runs the
// tests from turning provisioning on in the servers they start.
func TestMain(m *testing.M) {
	os.Unsetenv("HETZNER_TOKEN")
	os.Exit(m.Run())
}

func TestGivenBadBackupScheduleWhenStartingThenTheServerRefuses(t *testing.T) {
	for _, args := range [][]string{{"-backup-keep", "0"}, {"-backup-every", "-1h"}} {
		dbPath := filepath.Join(t.TempDir(), "server.db")
		status, _, stderr := runCommand(append([]string{"-insecure-loopback", "-db", dbPath}, args...)...)
		if status != 2 || !strings.Contains(stderr, args[0]) {
			t.Errorf("%v: status %d, stderr %q; want 2 naming %s", args, status, stderr, args[0])
		}
	}
}

func TestGivenPartOfABucketWhenStartingThenTheServerRefusesAndNamesWhatIsMissing(t *testing.T) {
	credentials := filepath.Join(t.TempDir(), "backup-s3")
	if err := os.WriteFile(credentials, []byte("access_key=AK\nsecret_key=hidden-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	full := map[string]string{"-backup-s3-endpoint": "https://fsn1.your-objectstorage.com", "-backup-s3-region": "fsn1",
		"-backup-s3-bucket": "backups", "-backup-s3-credentials": credentials}
	for missing := range full {
		args := []string{"-insecure-loopback", "-db", filepath.Join(t.TempDir(), "server.db")}
		for name, value := range full {
			if name != missing {
				args = append(args, name, value)
			}
		}
		status, _, stderr := runCommand(args...)
		if status != 2 || !strings.Contains(stderr, missing) {
			t.Errorf("without %s: status %d, stderr %q; want 2 naming it", missing, status, stderr)
		}
	}
	for _, endpoint := range []string{"http://fsn1.your-objectstorage.com", "fsn1.your-objectstorage.com"} {
		args := []string{"-insecure-loopback", "-db", filepath.Join(t.TempDir(), "server.db")}
		for name, value := range full {
			if name == "-backup-s3-endpoint" {
				value = endpoint
			}
			args = append(args, name, value)
		}
		status, _, stderr := runCommand(args...)
		if status != 2 || !strings.Contains(stderr, "-backup-s3-endpoint") {
			t.Errorf("endpoint %s: status %d, stderr %q; want 2 naming -backup-s3-endpoint", endpoint, status, stderr)
		}
	}
}

func TestGivenBucketCredentialsOthersCanReadWhenStartingThenTheServerRefusesWithoutShowingThem(t *testing.T) {
	credentials := filepath.Join(t.TempDir(), "backup-s3")
	if err := os.WriteFile(credentials, []byte("access_key=AK\nsecret_key=hidden-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, _, stderr := runCommand("-insecure-loopback", "-db", filepath.Join(t.TempDir(), "server.db"),
		"-backup-s3-endpoint", "https://fsn1.your-objectstorage.com", "-backup-s3-region", "fsn1",
		"-backup-s3-bucket", "backups", "-backup-s3-credentials", credentials)
	if status != 1 || !strings.Contains(stderr, "-backup-s3-credentials") || strings.Contains(stderr, "hidden-secret") {
		t.Errorf("status %d, stderr %q; want 1 naming -backup-s3-credentials without the secret", status, stderr)
	}
}

func TestGivenDaemonBinariesDirWhenNoDaemonBinaryURLIsGivenThenItDefaultsToTheServersOwnRoute(t *testing.T) {
	for _, tc := range []struct {
		daemonURL, publicURL, dir, want string
	}{
		{"", "https://orchestrator.example:8443", "/srv/binaries", "https://orchestrator.example:8443/daemon/linux-{arch}"},
		{"", "https://orchestrator.example:8443/", "/srv/binaries", "https://orchestrator.example:8443/daemon/linux-{arch}"},
		{"https://downloads.example/daemon-{arch}", "https://orchestrator.example:8443", "/srv/binaries", "https://downloads.example/daemon-{arch}"},
		{"", "https://orchestrator.example:8443", "", ""},
		{"", "", "/srv/binaries", ""},
	} {
		if got := defaultDaemonBinaryURL(tc.daemonURL, tc.publicURL, tc.dir); got != tc.want {
			t.Errorf("defaultDaemonBinaryURL(%q, %q, %q) = %q, want %q", tc.daemonURL, tc.publicURL, tc.dir, got, tc.want)
		}
	}
}

func TestGivenDaemonBinariesDirThatIsNotADirectoryWhenStartingThenTheServerRefuses(t *testing.T) {
	status, _, stderr := runCommand("-insecure-loopback", "-db", filepath.Join(t.TempDir(), "server.db"), "-daemon-binaries-dir", filepath.Join(t.TempDir(), "missing"))
	if status != 2 || !strings.Contains(stderr, "-daemon-binaries-dir") {
		t.Errorf("status %d, stderr %q; want 2 naming the flag", status, stderr)
	}
}
