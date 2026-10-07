package daemon

import (
	"bytes"
	"encoding/json"
	"io"
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
	j, _, err := reopenJournal(stateDir, "task-1", testHarness)
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
