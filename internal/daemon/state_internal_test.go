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

func TestGivenStateFileOfAnEarlierDaemonWhenLoadingThenEachTaskKeepsItsAckedSeq(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(StatePath(dir), []byte(`{"last_command":4,"tasks":{"task-a":9}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := mustLoadState(t, dir)

	rec, ok := s.record("task-a")
	if !ok || rec.Acked != 9 || rec.resumable() {
		t.Errorf("record = %+v, %v", rec, ok)
	}
}

// endedProcess records the end of a process of task as the service does,
// with session and seq.
func endedProcess(t *testing.T, s *state, dir string, task protocol.TaskID, session string, seq uint64) {
	t.Helper()
	j, err := s.openJournal(task, func(taskRecord) (*journal, error) {
		return createJournal(dir, task, protocol.Harness{Name: "fake"})
	})
	if err != nil {
		t.Fatal(err)
	}
	j.close()
	err = s.closeJournal(task, func(rec *taskRecord) {
		rec.Seq, rec.Session = seq, session
		rec.Settings = &taskSettings{Model: "fake-model"}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGivenResumableTaskHeldWholeWhenDroppingItsJournalThenTheJournalGoesAndTheRecordStays(t *testing.T) {
	dir := t.TempDir()
	s := mustLoadState(t, dir)
	s.recordStart(1, "task-a")
	endedProcess(t, s, dir, "task-a", "session-1", 7)
	s.recordAcked("task-a", 7)

	dropped, err := s.dropJournal("task-a", 7, JournalPath(dir, "task-a"))

	if err != nil || !dropped {
		t.Fatalf("drop = %v, %v", dropped, err)
	}
	if _, err := os.Stat(JournalPath(dir, "task-a")); !os.IsNotExist(err) {
		t.Errorf("journal still there: %v", err)
	}
	rec, ok := mustLoadState(t, dir).record("task-a")
	if !ok || rec.Session != "session-1" || rec.Seq != 7 || !rec.resumable() {
		t.Errorf("record after reload = %+v, %v", rec, ok)
	}
}

func TestGivenTaskThatCannotBeResumedWhenDroppingItsJournalThenItIsForgotten(t *testing.T) {
	dir := t.TempDir()
	s := mustLoadState(t, dir)
	s.recordStart(1, "task-a")
	endedProcess(t, s, dir, "task-a", "", 3)

	dropped, err := s.dropJournal("task-a", 3, JournalPath(dir, "task-a"))

	if err != nil || !dropped {
		t.Fatalf("drop = %v, %v", dropped, err)
	}
	if mustLoadState(t, dir).known("task-a") {
		t.Error("task still known")
	}
	if mustLoadState(t, dir).lastCommand() != 1 {
		t.Error("forgetting a task lost the last command")
	}
}

func TestGivenJournalHeldByAProcessOrNotHeldWholeWhenDroppingThenItStays(t *testing.T) {
	dir := t.TempDir()
	s := mustLoadState(t, dir)
	s.recordStart(1, "task-a")
	endedProcess(t, s, dir, "task-a", "session-1", 7)

	notHeld, _ := s.dropJournal("task-a", 6, JournalPath(dir, "task-a"))
	s.openJournal("task-a", func(taskRecord) (*journal, error) { return &journal{}, nil })
	open, _ := s.dropJournal("task-a", 7, JournalPath(dir, "task-a"))

	if notHeld || open {
		t.Errorf("dropped: not held whole %v, held by a process %v", notHeld, open)
	}
	if _, err := os.Stat(JournalPath(dir, "task-a")); err != nil {
		t.Errorf("journal: %v", err)
	}
}
