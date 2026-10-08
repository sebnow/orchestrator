package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

var testHarness = protocol.Harness{Name: "fake", Version: "1.0"}

func readJournalFile(t *testing.T, path string) []protocol.Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []protocol.Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(nil, 16<<20)
	for scanner.Scan() {
		var event protocol.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("journal line %q: %v", scanner.Bytes(), err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func newTestJournal(t *testing.T) (*journal, string) {
	t.Helper()
	dir := t.TempDir()
	j, err := createJournal(dir, "task-1", testHarness)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.close() })
	return j, JournalPath(dir, "task-1")
}

func TestGivenNewJournalWhenAppendingEventsThenSeqsStartAtOneAndAreContiguous(t *testing.T) {
	j, path := newTestJournal(t)

	j.appendControl(protocol.KindHarnessStarted, protocol.HarnessStarted{PID: 7, Model: "m", Workdir: "/w"})
	j.appendOutput([]byte(`{"type":"system"}`))
	j.appendControl(protocol.KindPauseAcknowledged, protocol.PauseAcknowledged{Note: "stopped"})

	events := readJournalFile(t, path)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	wantKinds := []protocol.Kind{protocol.KindHarnessStarted, protocol.KindHarnessOutput, protocol.KindPauseAcknowledged}
	for idx, event := range events {
		if event.Seq != uint64(idx+1) || event.Kind != wantKinds[idx] {
			t.Errorf("event %d = seq %d kind %s", idx, event.Seq, event.Kind)
		}
		if event.TaskID != "task-1" || event.Harness != testHarness || event.Time.IsZero() {
			t.Errorf("event %d envelope = %+v", idx, event)
		}
	}
	if string(events[2].Payload) != `{"note":"stopped"}` {
		t.Errorf("payload = %s", events[2].Payload)
	}
}

func TestGivenHarnessLineWithHTMLCharactersWhenJournalingThenThePayloadIsTheLineVerbatim(t *testing.T) {
	j, path := newTestJournal(t)
	line := `{"type":"user","content":"<tool_use_error>a & b</tool_use_error>"}`

	j.appendOutput([]byte(line))

	events := readJournalFile(t, path)
	if string(events[0].Payload) != line {
		t.Errorf("payload = %s, want %s", events[0].Payload, line)
	}
}

func TestGivenHarnessLineThatIsNotJSONWhenJournalingThenThePayloadIsAJSONString(t *testing.T) {
	j, path := newTestJournal(t)

	j.appendOutput([]byte(`warning: something "odd"`))

	events := readJournalFile(t, path)
	var got string
	if err := json.Unmarshal(events[0].Payload, &got); err != nil || got != `warning: something "odd"` {
		t.Errorf("payload = %s (%v)", events[0].Payload, err)
	}
}

func TestGivenConcurrentAppendsWhenJournalingThenFileOrderIsSeqOrderWithoutGaps(t *testing.T) {
	j, path := newTestJournal(t)
	const writers, perWriter = 8, 50

	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for range perWriter {
				if _, err := j.appendOutput([]byte(`{"type":"assistant"}`)); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()

	events := readJournalFile(t, path)
	if len(events) != writers*perWriter {
		t.Fatalf("got %d events, want %d", len(events), writers*perWriter)
	}
	for idx, event := range events {
		if event.Seq != uint64(idx+1) {
			t.Fatalf("line %d has seq %d", idx+1, event.Seq)
		}
	}
}

func TestGivenExistingJournalWhenCreatingItAgainThenErrTaskExists(t *testing.T) {
	dir := t.TempDir()
	first, err := createJournal(dir, "task-1", testHarness)
	if err != nil {
		t.Fatal(err)
	}
	first.close()

	_, err = createJournal(dir, "task-1", testHarness)

	if !errors.Is(err, ErrTaskExists) {
		t.Errorf("err = %v, want ErrTaskExists", err)
	}
}

func TestGivenStateDirThatIsAFileWhenCreatingJournalThenError(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(stateDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := createJournal(stateDir, "task-1", testHarness); err == nil {
		t.Error("no error")
	}
}

func TestGivenClosedJournalWhenAppendingThenEveryAppendFailsAndNoSeqIsSkipped(t *testing.T) {
	j, path := newTestJournal(t)
	j.appendOutput([]byte(`{}`))
	j.close()

	_, err := j.appendOutput([]byte(`{}`))

	if err == nil {
		t.Fatal("append after close succeeded")
	}
	if events := readJournalFile(t, path); len(events) != 1 {
		t.Errorf("got %d events, want 1", len(events))
	}
}

func TestGivenJournalThatContinuesAnEarlierOneWhenReopeningThenItAppendsAfterItsLastSeq(t *testing.T) {
	stateDir := t.TempDir()
	j, err := createJournalAfter(stateDir, "task-1", testHarness, 7)
	if err != nil {
		t.Fatal(err)
	}
	j.appendOutput([]byte(`{"n":8}`))
	j.close()

	reopened, end, err := reopenJournal(stateDir, "task-1", testHarness, 7)
	if err != nil {
		t.Fatal(err)
	}
	event, err := reopened.appendOutput([]byte(`{"n":9}`))
	reopened.close()

	if err != nil || end.seq != 8 || event.Seq != 9 {
		t.Errorf("end %+v, appended seq %d, err %v", end, event.Seq, err)
	}
}

func TestGivenEmptyContinuationJournalWhenReopeningThenItContinuesAfterTheRecordedSeq(t *testing.T) {
	stateDir := t.TempDir()
	j, err := createJournalAfter(stateDir, "task-1", testHarness, 5)
	if err != nil {
		t.Fatal(err)
	}
	j.close()

	reopened, _, err := reopenJournal(stateDir, "task-1", testHarness, 5)
	if err != nil {
		t.Fatal(err)
	}
	event, err := reopened.appendOutput([]byte(`{}`))
	reopened.close()

	if err != nil || event.Seq != 6 {
		t.Errorf("appended seq %d, err %v, want 6", event.Seq, err)
	}
}

func TestGivenJournalWithAGapAfterItsFirstEventWhenScanningThenItIsAnError(t *testing.T) {
	stateDir := t.TempDir()
	j, err := createJournalAfter(stateDir, "task-1", testHarness, 3)
	if err != nil {
		t.Fatal(err)
	}
	j.appendOutput([]byte(`{}`))
	j.seq++
	j.appendOutput([]byte(`{}`))
	j.close()

	if _, err := scanJournal(JournalPath(stateDir, "task-1")); err == nil || !strings.Contains(err.Error(), "seq 6 follows 4") {
		t.Errorf("err = %v", err)
	}
}
