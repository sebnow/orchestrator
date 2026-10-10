package daemon

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// testMirrors are mirrors in a new state directory, reaching the remote
// without a key file.
func testMirrors(t *testing.T) *mirrors {
	t.Helper()
	return newMirrors(t.TempDir(), "", false)
}

func TestGivenNoMirrorWhenUpdatingThenABareMirrorOfTheRemotesBranchesAndTagsIsMadeUnderTheStateDirectory(t *testing.T) {
	repo := makeTestRepo(t)
	stateDir := t.TempDir()
	m := newMirrors(stateDir, "", false)

	path, err := m.update(t.Context(), repo.url)
	if err != nil {
		t.Fatal(err)
	}

	if filepath.Dir(path) != filepath.Join(stateDir, "mirrors") || path != m.path(repo.url) {
		t.Errorf("mirror at %s, want it under %s/mirrors", path, stateDir)
	}
	if got := git(t, path, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("bare = %s", got)
	}
	if got := git(t, path, "config", "remote.origin.url"); got != repo.url {
		t.Errorf("origin = %s, want %s", got, repo.url)
	}
	for ref, want := range map[string]string{"refs/heads/main": repo.second, "refs/heads/feature": repo.featureCommit, "refs/tags/v1": repo.first} {
		if got := git(t, path, "rev-parse", ref); got != want {
			t.Errorf("%s = %s, want %s", ref, got, want)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(stateDir, "mirrors"))
	if len(entries) != 1 {
		t.Errorf("mirrors directory holds %d entries, want the mirror alone", len(entries))
	}
}

func TestGivenAMirrorWhenTheRemoteMovesAndItIsUpdatedThenItFetchesTheNewCommits(t *testing.T) {
	repo := makeTestRepo(t)
	m := testMirrors(t)
	if _, err := m.update(t.Context(), repo.url); err != nil {
		t.Fatal(err)
	}
	earlier := pushTaskBranchFrom(t, repo)

	path, err := m.update(t.Context(), repo.url)
	if err != nil {
		t.Fatal(err)
	}

	if got := git(t, path, "rev-parse", "refs/heads/"+protocol.TaskBranch(deliveryTask)); got != earlier {
		t.Errorf("task branch = %s, want the pushed %s", got, earlier)
	}
}

// The harness user clones from the mirror, so whatever the daemon's
// umask, every directory of the mirror is open to every user and every
// file readable.
func TestGivenARestrictiveUmaskWhenAMirrorIsMadeThenEveryUserMayReadIt(t *testing.T) {
	repo := makeTestRepo(t)
	m := testMirrors(t)
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	path, err := m.update(t.Context(), repo.url)
	if err != nil {
		t.Fatal(err)
	}

	var closed []string
	err = filepath.WalkDir(m.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o004)
		if d.IsDir() {
			want = 0o005
		}
		if info.Mode().Perm()&want != want {
			closed = append(closed, info.Mode().Perm().String()+" "+p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) > 0 {
		t.Errorf("closed to other users in %s:\n%s", path, strings.Join(closed, "\n"))
	}
}

func TestGivenARepositoryThatDoesNotExistWhenUpdatingThenTheFetchFailsAndTheMirrorIsKeptForTheNextTry(t *testing.T) {
	m := testMirrors(t)
	missing := httpsAlias(t, filepath.Join(t.TempDir(), "missing.git"))

	_, err := m.update(t.Context(), missing)

	if err == nil || !strings.Contains(err.Error(), "git fetch") {
		t.Errorf("err = %v, want git fetch's", err)
	}
	if entries, _ := os.ReadDir(m.dir); len(entries) != 1 || strings.HasPrefix(entries[0].Name(), ".new-") {
		t.Errorf("mirrors directory = %v, want the mirror alone", entries)
	}
}

func TestGivenATaskWhenItsRepositoryIsRecordedThenItIsReadBackUntilForgotten(t *testing.T) {
	m := testMirrors(t)
	if repo, err := m.recorded(deliveryTask); err != nil || repo != "" {
		t.Errorf("before = %q, %v; want none", repo, err)
	}

	if err := m.record(deliveryTask, "git@github.com:octocat/Hello-World.git"); err != nil {
		t.Fatal(err)
	}

	if repo, err := m.recorded(deliveryTask); err != nil || repo != "git@github.com:octocat/Hello-World.git" {
		t.Errorf("recorded = %q, %v", repo, err)
	}
	if info, err := os.Stat(m.repos); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("records directory: %v, %v; want mode 0700", info, err)
	}
	if err := m.forget(deliveryTask); err != nil {
		t.Fatal(err)
	}
	if repo, err := m.recorded(deliveryTask); err != nil || repo != "" {
		t.Errorf("after forgetting = %q, %v; want none", repo, err)
	}
	if err := m.forget(deliveryTask); err != nil {
		t.Errorf("forgetting again: %v", err)
	}
}

func TestGivenAKeyWhenMakingMirrorsThenSSHUsesItAndOffersItAloneOnlyWhenAsked(t *testing.T) {
	key := "/state dir/it's/ssh_ed25519"
	for identitiesOnly, want := range map[bool]string{
		false: `GIT_SSH_COMMAND=ssh -i '/state dir/it'\''s/ssh_ed25519' -o BatchMode=yes`,
		true:  `GIT_SSH_COMMAND=ssh -i '/state dir/it'\''s/ssh_ed25519' -o BatchMode=yes -o IdentitiesOnly=yes`,
	} {
		env := newMirrors("/state", key, identitiesOnly).env
		if !slices.Contains(env, want) || !slices.Contains(env, "GIT_CONFIG_GLOBAL=/dev/null") || !slices.Contains(env, "GIT_TERMINAL_PROMPT=0") {
			t.Errorf("identities only %v: env = %q, want %q", identitiesOnly, env, want)
		}
		ssh := 0
		for _, kv := range env {
			if strings.HasPrefix(kv, "GIT_SSH_COMMAND=") {
				ssh++
			}
		}
		if ssh != 1 {
			t.Errorf("env = %q, want GIT_SSH_COMMAND once", env)
		}
	}
}

func TestGivenKnownHostsFilesWhenMakingMirrorsThenSSHChecksHostKeysStrictlyAgainstThemAlone(t *testing.T) {
	env := newMirrors("/state", "/state/ssh_ed25519", true, "/state/known_hosts", `/Users/o w"ner/.ssh/known_hosts`).env

	want := `GIT_SSH_COMMAND=ssh -i '/state/ssh_ed25519' -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes ` +
		`-o 'UserKnownHostsFile=/state/known_hosts "/Users/o w\"ner/.ssh/known_hosts"'`
	if !slices.Contains(env, want) {
		t.Errorf("env = %q, want %q", env, want)
	}
}

// TestGivenKnownHostsFilesWhenSSHReadsTheCommandThenItTakesEachFileWhole
// runs the command git would run through sh, with -G, which prints the
// configuration ssh would connect with and connects to nothing.
func TestGivenKnownHostsFilesWhenSSHReadsTheCommandThenItTakesEachFileWhole(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh on PATH")
	}
	dir := filepath.Join(t.TempDir(), "state dir")
	var command string
	for _, kv := range newMirrors(dir, "", false, filepath.Join(dir, "known_hosts"), "/home/owner/.ssh/known_hosts").env {
		if value, ok := strings.CutPrefix(kv, "GIT_SSH_COMMAND="); ok {
			command = value
		}
	}

	out, err := exec.CommandContext(t.Context(), "sh", "-c", command+" -F /dev/null -G git.example.com").Output()
	if err != nil {
		t.Fatalf("%s -G: %v", command, err)
	}

	config := map[string]string{}
	for line := range strings.Lines(string(out)) {
		name, value, _ := strings.Cut(strings.TrimSpace(line), " ")
		config[name] = value
	}
	if got, want := config["userknownhostsfile"], filepath.Join(dir, "known_hosts")+" /home/owner/.ssh/known_hosts"; got != want {
		t.Errorf("userknownhostsfile %q, want %q", got, want)
	}
	if got := config["stricthostkeychecking"]; got != "true" {
		t.Errorf("stricthostkeychecking %q, want true", got)
	}
	if got := config["batchmode"]; got != "yes" {
		t.Errorf("batchmode %q, want yes", got)
	}
}
