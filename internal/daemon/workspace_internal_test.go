package daemon

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// testRepo is a bare repository with two commits on main, the first
// tagged v1, and a branch "feature" off the first with one more commit.
type testRepo struct {
	bare          string
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
	return r
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

			if err := prepareWorkspace(t.Context(), dir, &protocol.Workspace{Repo: repo.bare, Ref: c.ref}); err != nil {
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

	err := prepareWorkspace(t.Context(), dir, &protocol.Workspace{Repo: repo.bare, Ref: "no-such-ref"})

	if err == nil || !strings.Contains(err.Error(), "git checkout") || !strings.Contains(err.Error(), "no-such-ref") {
		t.Errorf("err = %v", err)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("workspace left behind: %v", statErr)
	}
}

func TestGivenRepositoryThatDoesNotExistWhenPreparingThenGitsCloneErrorIsReturned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "task-1")

	err := prepareWorkspace(t.Context(), dir, &protocol.Workspace{Repo: filepath.Join(t.TempDir(), "missing.git"), Ref: "main"})

	if err == nil || !strings.Contains(err.Error(), "git clone") || !strings.Contains(err.Error(), "missing.git") {
		t.Errorf("err = %v", err)
	}
}

func TestGivenRefThatLooksLikeAnOptionWhenPreparingThenItIsRefused(t *testing.T) {
	repo := makeTestRepo(t)
	dir := filepath.Join(t.TempDir(), "task-1")

	err := prepareWorkspace(t.Context(), dir, &protocol.Workspace{Repo: repo.bare, Ref: "--upload-pack=touch pwned"})

	if err == nil || !strings.Contains(err.Error(), "is not a ref") {
		t.Errorf("err = %v", err)
	}
}
