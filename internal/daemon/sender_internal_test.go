package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const testDaemon protocol.DaemonID = "daemon-1"

type senderFixture struct {
	server   *serverFixture
	stateDir string
	state    *state
	sender   *sender
	task     protocol.TaskID
	journal  *journal
}

// newSenderFixture creates a task on the server and its journal on the
// daemon, without running the sender.
func newSenderFixture(t *testing.T) *senderFixture {
	t.Helper()
	f := &senderFixture{server: startServer(t), stateDir: t.TempDir()}
	f.server.registerDaemon(t, testDaemon)
	f.task = f.server.createTask(t, testDaemon, protocol.StartTask{Prompt: "p", PauseLimits: testPauseLimits}).TaskID
	f.state = mustLoadState(t, f.stateDir)
	if err := f.state.recordStart(1, f.task); err != nil {
		t.Fatal(err)
	}
	j, err := createJournal(f.stateDir, f.task, testHarness)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.close() })
	f.journal = j
	f.sender = newSender(&http.Client{}, f.server.url, testDaemon, f.stateDir, f.state, testLogger(t),
		backoff{min: 5 * time.Millisecond, max: 20 * time.Millisecond})
	return f
}

func (f *senderFixture) appendOutputs(t *testing.T, count int) {
	t.Helper()
	for range count {
		if _, err := f.journal.appendOutput([]byte(`{"type":"assistant"}`)); err != nil {
			t.Fatal(err)
		}
	}
	f.sender.notify(f.task)
}

func (f *senderFixture) appendExit(t *testing.T) {
	t.Helper()
	if _, err := f.journal.appendControl(protocol.KindHarnessExited, protocol.HarnessExited{}); err != nil {
		t.Fatal(err)
	}
	f.sender.notify(f.task)
}

// run runs the sender until the test ends.
func (f *senderFixture) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.sender.run(ctx, nil)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func (f *senderFixture) waitHeld(t *testing.T, count int) []protocol.Event {
	t.Helper()
	var events []protocol.Event
	eventually(t, "the server to hold the events", func() bool {
		events = f.server.events(t, f.task)
		return len(events) >= count
	})
	return events
}

// postedSeqs returns the seqs of each successful POST of events.
func postedSeqs(t *testing.T, requests []recordedRequest) [][]uint64 {
	t.Helper()
	var batches [][]uint64
	for _, req := range requests {
		if req.endpoint != "events" || req.failed {
			continue
		}
		var events []protocol.Event
		if err := json.Unmarshal(req.body, &events); err != nil {
			t.Fatalf("posted body %q: %v", req.body, err)
		}
		var seqs []uint64
		for _, event := range events {
			seqs = append(seqs, event.Seq)
		}
		batches = append(batches, seqs)
	}
	return batches
}

func TestGivenJournaledEventsWhenSendingThenTheServerHoldsThemAndTheWatermarkIsSaved(t *testing.T) {
	f := newSenderFixture(t)
	f.appendOutputs(t, 3)

	f.run(t)

	events := f.waitHeld(t, 3)
	assertContiguous(t, events)
	eventually(t, "the watermark to be saved", func() bool { return mustLoadState(t, f.stateDir).acked(f.task) == 3 })
	if _, err := os.Stat(JournalPath(f.stateDir, f.task)); err != nil {
		t.Errorf("journal of a running task: %v", err)
	}
}

func TestGivenEndedTaskWhenTheServerHoldsAllOfItThenItsJournalIsDeletedAndTheTaskForgotten(t *testing.T) {
	f := newSenderFixture(t)
	f.run(t)
	f.appendOutputs(t, 2)
	f.appendExit(t)

	f.waitHeld(t, 3)

	eventually(t, "the journal to be deleted", func() bool {
		_, err := os.Stat(JournalPath(f.stateDir, f.task))
		return errors.Is(err, fs.ErrNotExist)
	})
	if mustLoadState(t, f.stateDir).known(f.task) {
		t.Error("task still known")
	}
}

func TestGivenMoreEventsThanABatchHoldsWhenSendingThenTheyGoInOrderedBatchesWithinTheBound(t *testing.T) {
	f := newSenderFixture(t)
	f.sender.batchEvents = 3
	f.appendOutputs(t, 10)

	f.run(t)

	f.waitHeld(t, 10)
	batches := postedSeqs(t, f.server.recorded())
	want := [][]uint64{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}, {10}}
	if !slices.EqualFunc(batches, want, slices.Equal) {
		t.Errorf("batches = %v, want %v", batches, want)
	}
}

func TestGivenEventsLargerThanTheByteBoundWhenSendingThenEachGoesAlone(t *testing.T) {
	f := newSenderFixture(t)
	f.sender.batchBytes = 1
	f.appendOutputs(t, 3)

	f.run(t)

	f.waitHeld(t, 3)
	batches := postedSeqs(t, f.server.recorded())
	want := [][]uint64{{1}, {2}, {3}}
	if !slices.EqualFunc(batches, want, slices.Equal) {
		t.Errorf("batches = %v, want %v", batches, want)
	}
}

func TestGivenUnreachableServerWhenItComesBackThenTheSenderAsksForAcksFirstAndDeliversEverything(t *testing.T) {
	f := newSenderFixture(t)
	f.server.down.Store(true)
	f.appendOutputs(t, 2)
	f.run(t)
	eventually(t, "several failed attempts", func() bool {
		failed := 0
		for _, req := range f.server.recorded() {
			if req.failed {
				failed++
			}
		}
		return failed >= 3
	})
	f.appendOutputs(t, 2)

	f.server.down.Store(false)

	events := f.waitHeld(t, 4)
	assertContiguous(t, events)
	requests := f.server.recorded()
	requests = requests[slices.IndexFunc(requests, func(r recordedRequest) bool { return r.failed }):]
	firstServed := slices.IndexFunc(requests, func(r recordedRequest) bool { return !r.failed })
	if requests[firstServed].endpoint != "acks" {
		t.Errorf("first request after the outage = %s %s, want GET acks", requests[firstServed].method, requests[firstServed].endpoint)
	}
}

func TestGivenServerHoldingEarlyEventsWhenConnectingThenOnlyLaterEventsAreSent(t *testing.T) {
	f := newSenderFixture(t)
	f.appendOutputs(t, 4)
	// Another daemon process sent seq 1 and 2 and crashed before saving
	// the watermark.
	first := readJournalFile(t, JournalPath(f.stateDir, f.task))[:2]
	f.server.call(t, http.MethodPost, "/v1/daemons/"+string(testDaemon)+"/events", first, nil)

	f.run(t)

	f.waitHeld(t, 4)
	batches := postedSeqs(t, f.server.recorded())
	if want := [][]uint64{{1, 2}, {3, 4}}; !slices.EqualFunc(batches, want, slices.Equal) {
		t.Errorf("batches = %v, want %v (the first is the test's own)", batches, want)
	}
}

func TestGivenTaskTheServerDoesNotAssignToTheDaemonWhenSendingThenItIsDroppedAndOtherTasksAreSent(t *testing.T) {
	f := newSenderFixture(t)
	f.appendOutputs(t, 1)
	foreign, err := createJournal(f.stateDir, "foreign-task", testHarness)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.close()
	foreign.appendOutput([]byte(`{}`))
	f.state.recordStart(2, "foreign-task")
	f.sender.notify("foreign-task")

	f.run(t)

	f.waitHeld(t, 1)
	f.appendOutputs(t, 1)
	f.waitHeld(t, 2)
	posts := 0
	for _, req := range f.server.recorded() {
		var events []protocol.Event
		json.Unmarshal(req.body, &events)
		if len(events) > 0 && events[0].TaskID == "foreign-task" {
			posts++
		}
	}
	if posts != 1 {
		t.Errorf("foreign task posted %d times, want once", posts)
	}
}

func TestGivenForgottenTaskWhenNamedAgainAndReconnectingThenItStaysForgotten(t *testing.T) {
	f := newSenderFixture(t)
	f.appendOutputs(t, 1)
	f.appendExit(t)
	if err := f.sender.pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.state.known(f.task) {
		t.Fatal("ended task not forgotten")
	}

	f.sender.notify(f.task)
	f.sender.connected = false
	if err := f.sender.pass(t.Context()); err != nil {
		t.Fatal(err)
	}

	if mustLoadState(t, f.stateDir).known(f.task) {
		t.Error("forgotten task is known again")
	}
}
