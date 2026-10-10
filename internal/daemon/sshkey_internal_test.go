package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGivenAnEmptyStateDirectoryWhenLoadingTheSSHKeyThenAPairIsWrittenOwnerOnlyForThePrivateKey(t *testing.T) {
	stateDir := t.TempDir()

	key, err := loadOrCreateSSHKey(stateDir, "vps-1")
	if err != nil {
		t.Fatal(err)
	}

	if key.Path != filepath.Join(stateDir, "ssh_ed25519") {
		t.Errorf("path = %s", key.Path)
	}
	for path, mode := range map[string]os.FileMode{key.Path: 0o600, key.Path + ".pub": 0o644} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
			t.Errorf("%s: %v, %v; want mode %v", path, info, err, mode)
		}
	}
	line, err := os.ReadFile(key.Path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if want := "ssh-ed25519 " + key.Blob + " orchestrator@vps-1\n"; string(line) != want {
		t.Errorf("public key = %q, want %q", line, want)
	}
	private, err := os.ReadFile(key.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(private), "-----BEGIN OPENSSH PRIVATE KEY-----\n") {
		t.Errorf("private key starts %q", strings.SplitN(string(private), "\n", 2)[0])
	}
}

func TestGivenAKeyWhenLoadingAgainThenTheSameKeyIsReturned(t *testing.T) {
	stateDir := t.TempDir()
	first, err := loadOrCreateSSHKey(stateDir, "vps-1")
	if err != nil {
		t.Fatal(err)
	}

	again, err := loadOrCreateSSHKey(stateDir, "vps-1")

	if err != nil || again != first {
		t.Errorf("again = %+v, %v; want %+v", again, err, first)
	}
}

func TestGivenAPrivateKeyWithoutItsPublicKeyWhenLoadingThenItFailsAndKeepsTheKey(t *testing.T) {
	stateDir := t.TempDir()
	key, err := loadOrCreateSSHKey(stateDir, "vps-1")
	if err != nil {
		t.Fatal(err)
	}
	private, _ := os.ReadFile(key.Path)
	if err := os.Remove(key.Path + ".pub"); err != nil {
		t.Fatal(err)
	}

	_, err = loadOrCreateSSHKey(stateDir, "vps-1")

	if err == nil || !strings.Contains(err.Error(), "restore it") {
		t.Errorf("err = %v; want one asking to restore the public key", err)
	}
	if after, _ := os.ReadFile(key.Path); string(after) != string(private) {
		t.Error("the private key was replaced")
	}
}

func TestGivenAPublicKeyThatIsNotEd25519WhenLoadingThenItFails(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "ssh_ed25519"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC0 me@host",
		"ssh-ed25519 not-base64",
		// The wire encoding of an ssh-rsa key under the ssh-ed25519 name.
		"ssh-ed25519 AAAAB3NzaC1yc2EAAAADAQABAAAAAQC0",
	} {
		if err := os.WriteFile(filepath.Join(stateDir, "ssh_ed25519.pub"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadOrCreateSSHKey(stateDir, "vps-1"); err == nil {
			t.Errorf("%q: loaded; want an error", line)
		}
	}
}

// ssh-keygen reads both files, and the private key's public half is the
// public key file's.
func TestGivenAGeneratedKeyWhenSSHKeygenReadsItThenBothFilesHaveTheSameFingerprint(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen is not on PATH")
	}
	key, err := loadOrCreateSSHKey(t.TempDir(), "vps-1")
	if err != nil {
		t.Fatal(err)
	}
	// ssh-keygen -l on a private key reads the .pub beside it when there
	// is one, so the private key is read from a directory of its own.
	alone := filepath.Join(t.TempDir(), "key")
	private, _ := os.ReadFile(key.Path)
	if err := os.WriteFile(alone, private, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(keygen, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ssh-keygen %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	fromPublic := strings.Fields(run("-l", "-f", key.Path+".pub"))
	fromPrivate := strings.Fields(run("-l", "-f", alone))
	derived := run("-y", "-f", alone)

	if len(fromPublic) < 2 || len(fromPrivate) < 2 || fromPublic[1] != fromPrivate[1] {
		t.Errorf("fingerprints: public %q, private %q", fromPublic, fromPrivate)
	}
	if fields := strings.Fields(derived); len(fields) < 2 || fields[0] != "ssh-ed25519" || fields[1] != key.Blob {
		t.Errorf("ssh-keygen -y = %q, want ssh-ed25519 %s", derived, key.Blob)
	}
}
