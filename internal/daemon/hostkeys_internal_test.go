package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const (
	testHostKey      = "git.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	otherTestHostKey = "git.example.org ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
)

func TestGivenHarnessUserOrNotWhenListingKnownHostsFilesThenTheDaemonUsersOwnIsAddedOnlyWithout(t *testing.T) {
	for name, tc := range map[string]struct {
		home        string
		harnessUser bool
		want        []string
	}{
		"harness user":    {home: "/home/orchestrator", harnessUser: true, want: []string{"/state/known_hosts"}},
		"single user":     {home: "/Users/owner", want: []string{"/state/known_hosts", "/Users/owner/.ssh/known_hosts"}},
		"single, no home": {want: []string{"/state/known_hosts"}},
	} {
		if got := knownHostsFiles("/state", tc.home, tc.harnessUser); !slices.Equal(got, tc.want) {
			t.Errorf("%s: files = %q, want %q", name, got, tc.want)
		}
	}
}

func TestGivenHostKeysOnTheServerWhenTheDaemonConnectsAndTheyChangeThenItsKnownHostsFollowsReadableByItsUserOnly(t *testing.T) {
	srv := startServer(t)
	putHostKeys := func(text string) {
		t.Helper()
		if status, body := srv.try(t, http.MethodPut, "/v1/host-keys", map[string]string{"forge": text}); status != http.StatusOK {
			t.Fatalf("PUT host keys: %d %s", status, body)
		}
	}
	putHostKeys(testHostKey)
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, knownHostsName)
	knownHosts := func() string {
		data, _ := os.ReadFile(path)
		return string(data)
	}

	runDaemon(t, srv.url, stateDir)

	eventually(t, "the known_hosts written at connect", func() bool { return knownHosts() == testHostKey+"\n" })
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
	putHostKeys(testHostKey + "\n" + otherTestHostKey)
	eventually(t, "the known_hosts rewritten on a change", func() bool { return knownHosts() == testHostKey+"\n"+otherTestHostKey+"\n" })
	putHostKeys("")
	eventually(t, "the known_hosts emptied", func() bool {
		info, err := os.Stat(path)
		return err == nil && info.Size() == 0
	})
	leftovers, err := filepath.Glob(filepath.Join(stateDir, ".known_hosts-*"))
	if err != nil || len(leftovers) > 0 {
		t.Errorf("temporary files left: %q, %v", leftovers, err)
	}
}
