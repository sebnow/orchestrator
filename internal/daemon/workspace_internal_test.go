package daemon

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// testRepo is a bare repository with two commits on main, the first
// tagged v1, and a branch "feature" off the first with one more commit.
// url is an https URL that git, in this test, rewrites to bare.
type testRepo struct {
	bare          string
	url           string
	first, second string
	featureCommit string
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, work, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", name)
	git(t, work, "commit", "--quiet", "-m", "add "+name)
	return git(t, work, "rev-parse", "HEAD")
}

func makeTestRepo(t *testing.T) testRepo {
	t.Helper()
	work := t.TempDir()
	git(t, work, "init", "--quiet", "-b", "main")
	var r testRepo
	r.first = commitFile(t, work, "file.txt", "one")
	git(t, work, "tag", "v1")
	git(t, work, "checkout", "--quiet", "-b", "feature")
	r.featureCommit = commitFile(t, work, "feature.txt", "feature")
	git(t, work, "checkout", "--quiet", "main")
	r.second = commitFile(t, work, "file.txt", "two")
	r.bare = filepath.Join(t.TempDir(), "repo.git")
	git(t, "", "clone", "--quiet", "--bare", work, r.bare)
	r.url = httpsAlias(t, r.bare)
	return r
}

// httpsAlias returns an https URL that git, for the rest of the test,
// rewrites to the local repository path. The daemon clones https URLs
// only, and the rewrite comes from the environment, which the daemon's
// git still reads, rather than from the git configuration files it
// ignores.
func httpsAlias(t *testing.T, path string) string {
	t.Helper()
	// The rewrite covers the directory only, so that a rewrite of the
	// whole URL, being longer, would take precedence over it.
	prefix := "https://repos.invalid/" + filepath.Base(filepath.Dir(path)) + "/"
	count := 0
	if raw := os.Getenv("GIT_CONFIG_COUNT"); raw != "" {
		count, _ = strconv.Atoi(raw)
	}
	t.Setenv("GIT_CONFIG_KEY_"+strconv.Itoa(count), "url."+filepath.Dir(path)+"/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_"+strconv.Itoa(count), prefix)
	t.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(count+1))
	return prefix + filepath.Base(path)
}

func readWorkspaceFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestGivenNoWorkspaceWhenPreparingThenTheDirectoryIsCreatedEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "workspaces", "task-1")

	if err := prepareWorkspace(t.Context(), dir, nil); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Errorf("entries = %v, err = %v", entries, err)
	}
}

func TestGivenRefsThatGitCanCloneWhenPreparingThenTheWorkspaceIsCheckedOutAtThem(t *testing.T) {
	repo := makeTestRepo(t)
	cases := []struct {
		name, ref, wantHead string
	}{
		{"branch", "main", repo.second},
		{"other branch", "feature", repo.featureCommit},
		{"tag", "v1", repo.first},
		{"commit", repo.first, repo.first},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "workspaces", "task-1")

			if err := prepareWorkspace(t.Context(), dir, &protocol.Workspace{Repo: repo.url, Ref: c.ref}); err != nil {
				t.Fatal(err)
			}

			if head := git(t, dir, "rev-parse", "HEAD"); head != c.wantHead {
				t.Errorf("HEAD = %s, want %s", head, c.wantHead)
			}
		})
	}
}

func TestGivenRefThatIsNotInTheRepositoryWhenPreparingThenGitsErrorIsReturnedAndNoDirectoryIsLeft(t *testing.T) {
	repo := makeTestRepo(t)
	dir := filepath.Join(t.TempDir(), "task-1")

	err := prepareWorkspace(t.Context(), dir, &protocol.Workspace{Repo: repo.url, Ref: "no-such-ref"})

	if err == nil || !strings.Contains(err.Error(), "git checkout") || !strings.Contains(err.Error(), "no-such-ref") {
		t.Errorf("err = %v", err)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("workspace left behind: %v", statErr)
	}
}

func TestGivenRepositoryThatDoesNotExistWhenPreparingThenGitsCloneErrorIsReturned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "task-1")

	err := prepareWorkspace(t.Context(), dir, &protocol.Workspace{Repo: httpsAlias(t, filepath.Join(t.TempDir(), "missing.git")), Ref: "main"})

	if err == nil || !strings.Contains(err.Error(), "git clone") || !strings.Contains(err.Error(), "missing.git") {
		t.Errorf("err = %v", err)
	}
}

func TestGivenRefThatLooksLikeAnOptionWhenPreparingThenItIsRefused(t *testing.T) {
	repo := makeTestRepo(t)
	dir := filepath.Join(t.TempDir(), "task-1")

	err := prepareWorkspace(t.Context(), dir, &protocol.Workspace{Repo: repo.url, Ref: "--upload-pack=touch pwned"})

	if err == nil || !strings.Contains(err.Error(), "is not a ref") {
		t.Errorf("err = %v", err)
	}
}

// The owner's git configuration must not reach the clone: a URL rewrite
// there sent a public https clone to an ssh remote, and a credential
// helper there would lend the daemon's credentials.
func TestGivenOwnerGitConfigThatRewritesTheRepositoryWhenPreparingThenTheCloneIgnoresIt(t *testing.T) {
	repo := makeTestRepo(t)
	config := filepath.Join(t.TempDir(), "gitconfig")
	rewrite := "[url \"" + filepath.Join(t.TempDir(), "missing.git") + "\"]\n\tinsteadOf = " + repo.url + "\n"
	if err := os.WriteFile(config, []byte(rewrite), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	dir := filepath.Join(t.TempDir(), "task-1")

	if err := prepareWorkspace(t.Context(), dir, &protocol.Workspace{Repo: repo.url, Ref: "main"}); err != nil {
		t.Fatal(err)
	}

	if head := git(t, dir, "rev-parse", "HEAD"); head != repo.second {
		t.Errorf("HEAD = %s, want %s", head, repo.second)
	}
}

func TestGivenRepositoryThatIsNeitherHTTPSNorSSHWhenPreparingThenItIsRefusedBeforeGitRuns(t *testing.T) {
	repo := makeTestRepo(t)
	for _, url := range []string{
		repo.bare,
		"file://" + repo.bare,
		"git://github.com/octocat/Hello-World.git",
		"http://github.com/octocat/Hello-World",
		"https:///no-host",
		"ssh:///no-host",
		"./relative:path",
		"/absolute/with:colon",
		"-oProxyCommand=touch pwned:x",
		"git@-oProxyCommand=x:path",
		"host:",
	} {
		t.Run(url, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "task-1")

			err := prepareWorkspace(t.Context(), dir, &protocol.Workspace{Repo: url, Ref: "main"})

			if err == nil || !strings.Contains(err.Error(), "is not an https:// URL, an ssh:// URL or an ssh address") {
				t.Errorf("err = %v", err)
			}
			if _, statErr := os.Stat(dir); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("workspace created: %v", statErr)
			}
		})
	}
}

func TestGivenHTTPSOrSSHRepositoryWhenCheckedThenItIsAccepted(t *testing.T) {
	for _, url := range []string{
		"https://github.com/octocat/Hello-World",
		"ssh://git@github.com/octocat/Hello-World.git",
		"ssh://github.com:2222/octocat/Hello-World.git",
		"git@github.com:octocat/Hello-World.git",
		"github.com:octocat/Hello-World.git",
	} {
		if err := requireRemote(url); err != nil {
			t.Errorf("%s: %v", url, err)
		}
	}
}
