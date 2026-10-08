package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

var oldHarness = protocol.Harness{Name: "fake", Version: "0.9"}

// writeJournal writes a journal for task with a harness_started event and
// count harness output events, as an earlier daemon process would have.
func writeJournal(t *testing.T, stateDir string, task protocol.TaskID, outputs int) {
	t.Helper()
	j, err := createJournal(stateDir, task, oldHarness)
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	if _, err := j.appendControl(protocol.KindHarnessStarted, protocol.HarnessStarted{PID: 1}); err != nil {
		t.Fatal(err)
	}
	for range outputs {
		if _, err := j.appendOutput([]byte(`{"type":"assistant"}`)); err != nil {
			t.Fatal(err)
		}
	}
}

func recoverIn(t *testing.T, stateDir string) (*state, *bytes.Buffer) {
	t.Helper()
	st := mustLoadState(t, stateDir)
	var logs bytes.Buffer
	d := New(stateDir, newFakeHarness(), nil, nil)
	if err := d.recoverTasks(st, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}
	return st, &logs
}

func assertEndsWithRestartExit(t *testing.T, events []protocol.Event, wantSeq uint64, wantHarness protocol.Harness) {
	t.Helper()
	for idx, event := range events {
		if event.Seq != uint64(idx+1) {
			t.Fatalf("event %d has seq %d", idx, event.Seq)
		}
	}
	last := events[len(events)-1]
	var exit protocol.HarnessExited
	if err := json.Unmarshal(last.Payload, &exit); err != nil {
		t.Fatal(err)
	}
	if last.Seq != wantSeq || last.Kind != protocol.KindHarnessExited || exit.ExitCode != -1 || exit.Error != "daemon restarted" {
		t.Errorf("last event = seq %d %s %s", last.Seq, last.Kind, last.Payload)
	}
	if last.Harness != wantHarness {
		t.Errorf("harness = %+v, want %+v", last.Harness, wantHarness)
	}
}

func TestGivenJournalWithoutExitWhenRecoveringThenHarnessExitedDaemonRestartedIsAppended(t *testing.T) {
	stateDir := t.TempDir()
	writeJournal(t, stateDir, "task-1", 2)
	mustLoadState(t, stateDir).recordStart(4, "task-1")

	recoverIn(t, stateDir)

	events := readJournalFile(t, JournalPath(stateDir, "task-1"))
	assertEndsWithRestartExit(t, events, 4, oldHarness)
}

func TestGivenJournalThatEndsInExitWhenRecoveringThenItIsUnchanged(t *testing.T) {
	stateDir := t.TempDir()
	writeJournal(t, stateDir, "task-1", 1)
	j, _, err := reopenJournal(stateDir, "task-1", testHarness, 0)
	if err != nil {
		t.Fatal(err)
	}
	j.appendControl(protocol.KindHarnessExited, protocol.HarnessExited{ExitCode: 0})
	j.close()
	mustLoadState(t, stateDir).recordStart(1, "task-1")
	before, _ := os.ReadFile(JournalPath(stateDir, "task-1"))

	recoverIn(t, stateDir)

	after, _ := os.ReadFile(JournalPath(stateDir, "task-1"))
	if !bytes.Equal(before, after) {
		t.Errorf("journal changed:\n%s\nbecame\n%s", before, after)
	}
}

func TestGivenJournalWithATornLastLineWhenRecoveringThenTheLineIsCutAndTheExitFollowsTheLastWholeEvent(t *testing.T) {
	stateDir := t.TempDir()
	writeJournal(t, stateDir, "task-1", 1)
	f, err := os.OpenFile(JournalPath(stateDir, "task-1"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(f, `{"task_id":"task-1","seq":3,"kind":"harn`)
	f.Close()
	mustLoadState(t, stateDir).recordStart(1, "task-1")

	recoverIn(t, stateDir)

	events := readJournalFile(t, JournalPath(stateDir, "task-1"))
	assertEndsWithRestartExit(t, events, 3, oldHarness)
}

func TestGivenAcceptedTaskWithoutAJournalWhenRecoveringThenItsJournalHoldsOnlyTheExit(t *testing.T) {
	stateDir := t.TempDir()
	mustLoadState(t, stateDir).recordStart(1, "task-1")

	recoverIn(t, stateDir)

	events := readJournalFile(t, JournalPath(stateDir, "task-1"))
	assertEndsWithRestartExit(t, events, 1, testHarness)
}

func TestGivenJournalTheStateDoesNotKnowWhenRecoveringThenItIsAdoptedAndEnded(t *testing.T) {
	stateDir := t.TempDir()
	writeJournal(t, stateDir, "task-1", 0)

	st, _ := recoverIn(t, stateDir)

	if !slices.Equal(st.tasks(), []protocol.TaskID{"task-1"}) || st.acked("task-1") != 0 {
		t.Errorf("state = %+v", st.saved)
	}
	if !mustLoadState(t, stateDir).known("task-1") {
		t.Error("adoption not saved")
	}
	assertEndsWithRestartExit(t, readJournalFile(t, JournalPath(stateDir, "task-1")), 2, oldHarness)
}

func TestGivenACorruptJournalWhenRecoveringThenItIsLoggedAndLeftAndOtherTasksAreRecovered(t *testing.T) {
	stateDir := t.TempDir()
	writeJournal(t, stateDir, "task-1", 0)
	writeJournal(t, stateDir, "task-2", 0)
	corrupt := []byte("not an event\n")
	if err := os.WriteFile(JournalPath(stateDir, "task-1"), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	_, logs := recoverIn(t, stateDir)

	if got, _ := os.ReadFile(JournalPath(stateDir, "task-1")); !bytes.Equal(got, corrupt) {
		t.Errorf("corrupt journal changed to %q", got)
	}
	if !bytes.Contains(logs.Bytes(), []byte("task=task-1")) {
		t.Errorf("logs = %s", logs)
	}
	assertEndsWithRestartExit(t, readJournalFile(t, JournalPath(stateDir, "task-2")), 2, oldHarness)
}

// startingProcess records, as the service does before starting a process,
// that task has a process with its journal open.
func startingProcess(t *testing.T, stateDir string, task protocol.TaskID, rec taskRecord) {
	t.Helper()
	st := mustLoadState(t, stateDir)
	if err := st.updateTask(task, func(r *taskRecord) { *r = rec }); err != nil {
		t.Fatal(err)
	}
	j, err := st.openJournal(task, func(rec taskRecord) (*journal, error) {
		j, _, err := reopenJournal(stateDir, task, oldHarness, rec.Seq)
		if errors.Is(err, fs.ErrNotExist) {
			return createJournalAfter(stateDir, task, oldHarness, rec.Seq)
		}
		return j, err
	})
	if err != nil {
		t.Fatal(err)
	}
	j.close()
}

func TestGivenProcessStartingAfterAnEndedOneWhenRecoveringThenItsJournalEndsAsRestartedAndTheTaskIsNotResumed(t *testing.T) {
	stateDir := t.TempDir()
	writeJournal(t, stateDir, "task-1", 1)
	j, _, err := reopenJournal(stateDir, "task-1", oldHarness, 0)
	if err != nil {
		t.Fatal(err)
	}
	j.appendControl(protocol.KindHarnessExited, protocol.HarnessExited{})
	j.close()
	settings := &taskSettings{Model: "fake-model"}
	startingProcess(t, stateDir, "task-1", taskRecord{Seq: 3, Session: "session-1", Settings: settings})

	st, _ := recoverIn(t, stateDir)

	assertEndsWithRestartExit(t, readJournalFile(t, JournalPath(stateDir, "task-1")), 4, oldHarness)
	if rec, _ := st.record("task-1"); !rec.Ended || rec.Running || rec.Seq != 4 {
		t.Errorf("record = %+v", rec)
	}
}

func TestGivenProcessStartingWithItsJournalDeletedWhenRecoveringThenANewJournalContinuesAfterTheRecordedSeq(t *testing.T) {
	stateDir := t.TempDir()
	settings := &taskSettings{Model: "fake-model"}
	startingProcess(t, stateDir, "task-1", taskRecord{Acked: 6, Seq: 6, Session: "session-1", Settings: settings})

	recoverIn(t, stateDir)

	events := readJournalFile(t, JournalPath(stateDir, "task-1"))
	if len(events) != 1 || events[0].Seq != 7 || events[0].Kind != protocol.KindHarnessExited {
		t.Errorf("journal = %+v", events)
	}
}

func TestGivenTaskBetweenProcessesWhenRecoveringThenItIsLeftResumable(t *testing.T) {
	stateDir := t.TempDir()
	st := mustLoadState(t, stateDir)
	st.updateTask("task-1", func(r *taskRecord) {
		*r = taskRecord{Acked: 6, Seq: 6, Session: "session-1", Settings: &taskSettings{Model: "fake-model"}}
	})

	recovered, _ := recoverIn(t, stateDir)

	if _, err := os.Stat(JournalPath(stateDir, "task-1")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("journal: %v", err)
	}
	if rec, _ := recovered.record("task-1"); !rec.resumable() {
		t.Errorf("record = %+v", rec)
	}
}
