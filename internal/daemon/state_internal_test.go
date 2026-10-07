package daemon

import (
	"os"
	"slices"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func mustLoadState(t *testing.T, stateDir string) *state {
	t.Helper()
	s, err := loadState(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGivenNoStateFileWhenLoadingThenTheStateIsEmpty(t *testing.T) {
	s := mustLoadState(t, t.TempDir())

	if s.lastCommand() != 0 || len(s.tasks()) != 0 {
		t.Errorf("state = %+v", s.saved)
	}
}

func TestGivenRecordedCommandsAndAcksWhenLoadingAgainThenTheyAreRestored(t *testing.T) {
	dir := t.TempDir()
	s := mustLoadState(t, dir)
	if err := s.recordStart(3, "task-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.recordStart(5, "task-b"); err != nil {
		t.Fatal(err)
	}
	if err := s.recordCommand(7); err != nil {
		t.Fatal(err)
	}
	if err := s.recordAcked("task-a", 12); err != nil {
		t.Fatal(err)
	}

	loaded := mustLoadState(t, dir)

	if loaded.lastCommand() != 7 {
		t.Errorf("last command = %d, want 7", loaded.lastCommand())
	}
	if got := loaded.tasks(); !slices.Equal(got, []protocol.TaskID{"task-a", "task-b"}) {
		t.Errorf("tasks = %v", got)
	}
	if loaded.acked("task-a") != 12 || loaded.acked("task-b") != 0 {
		t.Errorf("acked = %v", loaded.saved.Tasks)
	}
}

func TestGivenKnownTaskWhenForgettingItThenItIsUnknownAfterReload(t *testing.T) {
	dir := t.TempDir()
	s := mustLoadState(t, dir)
	s.recordStart(1, "task-a")

	if err := s.forget("task-a"); err != nil {
		t.Fatal(err)
	}

	if mustLoadState(t, dir).known("task-a") {
		t.Error("forgotten task still known after reload")
	}
	if mustLoadState(t, dir).lastCommand() != 1 {
		t.Error("forgetting a task lost the last command")
	}
}

func TestGivenUnwritableStateDirWhenRecordingThenTheErrorIsReturnedAndTheStateIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	s := mustLoadState(t, dir)
	s.recordStart(1, "task-a")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	err := s.recordStart(2, "task-b")

	if err == nil {
		t.Fatal("no error")
	}
	if s.lastCommand() != 1 || s.known("task-b") {
		t.Errorf("state changed after a failed write: %+v", s.saved)
	}
}

func TestGivenCorruptStateFileWhenLoadingThenItFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(StatePath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadState(dir); err == nil {
		t.Error("no error")
	}
}
