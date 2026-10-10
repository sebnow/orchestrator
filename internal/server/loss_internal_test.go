package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// testClock is a clock the test moves by hand.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock {
	return &testClock{at: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

const lossTimeout = 10 * time.Minute

// startLossServer serves with clock as the server's clock and daemons
// lost after lossTimeout.
func startLossServer(t *testing.T, clock *testClock) testServer {
	t.Helper()
	return startTestServerWith(t, Options{
		Now:      clock.now,
		Schedule: SchedulePolicy{FillerThreshold: 0.5, LowThreshold: 0.85, DaemonTimeout: lossTimeout, unreportedSlots: 2},
	})
}

// fakeDaemon is a daemon's command stream that the test can close, as a
// daemon that vanishes would.
type fakeDaemon struct {
	id       protocol.DaemonID
	commands <-chan sseEvent
	vanish   func()
}

// connectDaemon opens daemon's command stream and waits until the server
// counts it connected.
func connectDaemon(t *testing.T, srv testServer, daemon protocol.DaemonID) *fakeDaemon {
	t.Helper()
	return reconnectDaemon(t, srv, daemon, "")
}

// reconnectDaemon is connectDaemon sending lastEventID, the id of the
// last command the daemon applied, when it is not empty.
func reconnectDaemon(t *testing.T, srv testServer, daemon protocol.DaemonID, lastEventID string) *fakeDaemon {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	d := &fakeDaemon{id: daemon, commands: openEventStreamUntil(t, ctx, srv.url+"/v1/daemons/"+string(daemon)+"/commands", lastEventID)}
	waitConnected(t, srv, daemon, true)
	d.vanish = func() {
		cancel()
		waitConnected(t, srv, daemon, false)
	}
	return d
}

func waitConnected(t *testing.T, srv testServer, daemon protocol.DaemonID, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for slices.Contains(srv.connectedDaemons(), daemon) != want {
		if time.Now().After(deadline) {
			t.Fatalf("daemon %s connected = %v after 5s, want %v", daemon, !want, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// nextCommand skips d's commands until one of kind for task arrives.
func (d *fakeDaemon) nextCommand(t *testing.T, task protocol.TaskID, kind protocol.CommandKind) protocol.Command {
	t.Helper()
	for {
		if command := receiveCommand(t, d.commands); command.Kind == kind && command.TaskID == task {
			return command
		}
	}
}

// noCommand fails if d is sent a command of kind for task within a
// moment.
func (d *fakeDaemon) noCommand(t *testing.T, task protocol.TaskID, kind protocol.CommandKind) {
	t.Helper()
	timeout := time.After(200 * time.Millisecond)
	for {
		select {
		case event, ok := <-d.commands:
			if !ok {
				return
			}
			var command protocol.Command
			if json.Unmarshal([]byte(event.data), &command) == nil && command.Kind == kind && command.TaskID == task {
				t.Fatalf("daemon %s was sent %s for task %s", d.id, kind, task)
			}
		case <-timeout:
			return
		}
	}
}

// taskControlEvent is task's event of kind at seq.
func taskControlEvent(task protocol.TaskID, seq uint64, kind protocol.Kind, payload string) protocol.Event {
	e := event(task, seq, payload)
	e.Kind = kind
	return e
}

func taskState(t *testing.T, srv testServer, task protocol.TaskID) taskDetail {
	t.Helper()
	detail, err := srv.store.task(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	return detail
}

func lostAt(t *testing.T, srv testServer, daemon protocol.DaemonID) *time.Time {
	t.Helper()
	daemons, err := srv.store.daemons(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range daemons {
		if d.ID == daemon {
			return d.LostAt
		}
	}
	t.Fatalf("daemon %s not recorded", daemon)
	return nil
}

func decodeStart(t *testing.T, command protocol.Command) protocol.StartTask {
	t.Helper()
	var start protocol.StartTask
	if err := json.Unmarshal(command.Payload, &start); err != nil {
		t.Fatal(err)
	}
	return start
}

// runningOn starts a task on daemon d and has d report its harness
// started and one line of output, seqs 1 and 2.
func runningOn(t *testing.T, srv testServer, d *fakeDaemon, prompt string) protocol.TaskID {
	t.Helper()
	task := queueTaskViaForm(t, srv, string(d.id), prompt, nil)
	srv.pass(t)
	d.nextCommand(t, task, protocol.CommandStartTask)
	status, body := postEvents(t, srv, d.id,
		taskControlEvent(task, 1, protocol.KindHarnessStarted, started),
		event(task, 2, `{"type":"assistant"}`))
	requireAcks(t, status, body, map[protocol.TaskID]uint64{task: 2})
	return task
}

func TestGivenDaemonThatStopsSendingWhenTheTimeoutPassesThenItIsLostAndItsRunningTaskStartsAfreshOnAnotherDaemon(t *testing.T) {
	clock := newTestClock()
	srv := startLossServer(t, clock)
	vps := connectDaemon(t, srv, "vps")
	task := runningOn(t, srv, vps, "count to three")

	vps.vanish()
	clock.advance(lossTimeout - time.Second)
	srv.pass(t)
	if got := taskState(t, srv, task); got.State != TaskRunning || lostAt(t, srv, "vps") != nil {
		t.Fatalf("before the timeout: task %s, vps lost at %v; want running and not lost", got.State, lostAt(t, srv, "vps"))
	}

	laptop := connectDaemon(t, srv, "laptop")
	clock.advance(time.Second)
	srv.pass(t)

	if lostAt(t, srv, "vps") == nil {
		t.Fatal("vps is not lost after the timeout")
	}
	restart := laptop.nextCommand(t, task, protocol.CommandStartTask)
	prompt := decodeStart(t, restart).Prompt
	if !strings.HasPrefix(prompt, "count to three\n\n") || !strings.Contains(prompt, "daemon vps, which has been lost") {
		t.Errorf("new start's prompt = %q, want the first prompt and a note naming the lost daemon", prompt)
	}
	if got := taskState(t, srv, task); got.State != TaskPending || got.DaemonID != "laptop" {
		t.Errorf("task is %s on %q, want pending on laptop", got.State, got.DaemonID)
	}
	entries, err := srv.Transcript(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	moved := slices.ContainsFunc(entries, func(entry transcript.Entry) bool {
		body, ok := entry.Body.(transcript.TaskMoved)
		return ok && body.From == "vps" && body.To == "laptop" && body.Prompt == prompt
	})
	if !moved {
		t.Errorf("transcript has no move from vps to laptop: %+v", entries)
	}
	requireContains(t, getPage(t, srv.url+"/"), "no, lost since ")
}

func TestGivenMovedTaskWhenItsNewDaemonSendsEventsFromOneThenTheyAreHeldAfterTheOldOnesAndAckedAsItNumbersThem(t *testing.T) {
	clock := newTestClock()
	srv := startLossServer(t, clock)
	vps := connectDaemon(t, srv, "vps")
	task := runningOn(t, srv, vps, "count to three")
	laptop := connectDaemon(t, srv, "laptop")
	vps.vanish()
	clock.advance(lossTimeout)
	srv.pass(t)
	laptop.nextCommand(t, task, protocol.CommandStartTask)

	status, body := postEvents(t, srv, "laptop",
		taskControlEvent(task, 1, protocol.KindHarnessStarted, started),
		event(task, 2, `{"type":"assistant"}`))
	requireAcks(t, status, body, map[protocol.TaskID]uint64{task: 2})

	if got := storedEventCount(t, srv, task); got != 4 {
		t.Errorf("stored %d events, want the 2 from vps and the 2 from laptop", got)
	}
	status, body = doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	requireAcks(t, status, body, map[protocol.TaskID]uint64{task: 2})
	if got := taskState(t, srv, task); got.State != TaskRunning {
		t.Errorf("task is %s, want running", got.State)
	}
}

func TestGivenLostDaemonThatComesBackWhenItSendsEventsForAMovedTaskThenTheyAreRefusedAsMoved(t *testing.T) {
	clock := newTestClock()
	srv := startLossServer(t, clock)
	vps := connectDaemon(t, srv, "vps")
	task := runningOn(t, srv, vps, "count to three")
	vps.vanish()
	clock.advance(lossTimeout)
	srv.pass(t)
	connectDaemon(t, srv, "vps")

	status, body := postEvents(t, srv, "vps", event(task, 3, `{"type":"result"}`))

	var refused protocol.EventsRefused
	if status != http.StatusConflict || json.Unmarshal([]byte(body), &refused) != nil ||
		refused.Reason != protocol.RefusedTaskMoved || refused.TaskID != task {
		t.Fatalf("status = %d, body = %s; want 409 refusing task %s as moved", status, body, task)
	}
	if got := storedEventCount(t, srv, task); got != 2 {
		t.Errorf("stored %d events, want the 2 sent before the move", got)
	}
	if lostAt(t, srv, "vps") != nil {
		t.Error("vps is still lost after it reconnected")
	}
	status, body = doRequest(t, http.MethodGet, srv.url+"/v1/daemons/vps/acks", "")
	requireAcks(t, status, body, map[protocol.TaskID]uint64{})
}

func TestGivenMovedTaskWhenOnlyItsOldDaemonIsConnectedThenItsStartWaitsForAnother(t *testing.T) {
	clock := newTestClock()
	srv := startLossServer(t, clock)
	vps := connectDaemon(t, srv, "vps")
	task := runningOn(t, srv, vps, "count to three")
	vps.vanish()
	clock.advance(lossTimeout)
	srv.pass(t)
	back := reconnectDaemon(t, srv, "vps", strconv.FormatUint(lastCommandID(t, srv, "vps"), 10))

	srv.pass(t)

	back.noCommand(t, task, protocol.CommandStartTask)
	got := taskState(t, srv, task)
	if got.State != TaskQueued || got.Queue == nil || !strings.Contains(got.Queue.Reason, "ran on every connected daemon") {
		t.Errorf("task = %s, queue %+v; want queued, waiting for a daemon it did not run on", got.State, got.Queue)
	}
}

func TestGivenDaemonWhoseStreamWasOpenForLongerThanTheTimeoutWhenItReconnectsWithinTheTimeoutThenItIsNeverLost(t *testing.T) {
	clock := newTestClock()
	srv := startLossServer(t, clock)
	vps := connectDaemon(t, srv, "vps")
	task := runningOn(t, srv, vps, "count to three")
	clock.advance(3 * lossTimeout)
	srv.pass(t)

	vps.vanish()
	clock.advance(lossTimeout - time.Second)
	srv.pass(t)
	connectDaemon(t, srv, "vps")
	clock.advance(3 * lossTimeout)
	srv.pass(t)

	if at := lostAt(t, srv, "vps"); at != nil {
		t.Errorf("vps lost at %v, want never", at)
	}
	if got := taskState(t, srv, task); got.State != TaskRunning || got.DaemonID != "vps" {
		t.Errorf("task is %s on %q, want running on vps", got.State, got.DaemonID)
	}
}

func TestGivenDaemonLastSeenLongBeforeTheServerStartedWhenTheServerStartsThenTheTimeoutCountsFromTheStart(t *testing.T) {
	clock := newTestClock()
	srv := startLossServer(t, clock)
	_, err := srv.store.db.ExecContext(t.Context(), `INSERT INTO daemons (id, first_seen, last_seen) VALUES ('vps', ?1, ?1)`,
		formatTime(clock.now().Add(-24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}

	srv.pass(t)
	if at := lostAt(t, srv, "vps"); at != nil {
		t.Fatalf("vps lost at %v as the server starts, want not yet", at)
	}
	clock.advance(lossTimeout)
	srv.pass(t)
	if at := lostAt(t, srv, "vps"); at == nil || !at.Equal(clock.now()) {
		t.Errorf("vps lost at %v, want %v", at, clock.now())
	}
}

func TestGivenFinishedTaskOnALostDaemonWhenTheOwnerPromptsItThenItStartsAfreshWithThePromptInTheNote(t *testing.T) {
	clock := newTestClock()
	srv := startLossServer(t, clock)
	vps := connectDaemon(t, srv, "vps")
	task := runningOn(t, srv, vps, "count to three")
	status, body := postEvents(t, srv, "vps", taskControlEvent(task, 3, protocol.KindHarnessExited, cleanly))
	requireAcks(t, status, body, map[protocol.TaskID]uint64{task: 3})
	vps.vanish()
	laptop := connectDaemon(t, srv, "laptop")
	clock.advance(lossTimeout)
	srv.pass(t)
	if got := taskState(t, srv, task); got.State != TaskFinished {
		t.Fatalf("task with nothing queued is %s, want finished until its next turn", got.State)
	}

	status, body = doRequest(t, http.MethodPost, srv.url+"/v1/tasks/"+string(task)+"/commands", `{"kind":"prompt","payload":{"text":"now count to four"}}`)
	if status != http.StatusAccepted {
		t.Fatalf("prompt: %d %s", status, body)
	}
	srv.pass(t)

	prompt := decodeStart(t, laptop.nextCommand(t, task, protocol.CommandStartTask)).Prompt
	if !strings.HasPrefix(prompt, "count to three\n\n") || !strings.HasSuffix(prompt, "Owner's prompt 1 of 1:\n\nnow count to four") {
		t.Errorf("new start's prompt = %q, want the first prompt and a note carrying the owner's queued prompt", prompt)
	}
	laptop.noCommand(t, task, protocol.CommandPrompt)
}

func TestGivenParentAndChildOnALostDaemonWhenMovedThenTheChildFollowsItsParentsDaemonRuleAndTheParentGoesAnywhere(t *testing.T) {
	store, _ := openTestStore(t)
	taskIn(t, store, "parent", TaskRunning)
	if _, err := store.spawnTask(t.Context(), "laptop", "parent", "child", protocol.Spawn{Prompt: "help"}); err != nil {
		t.Fatal(err)
	}
	admitTurns(t, store)
	policy := roomyPolicy
	policy.DaemonTimeout = time.Minute

	if _, err := store.schedule(t.Context(), policy, time.Now().Add(time.Hour), nil, time.Time{}); err != nil {
		t.Fatal(err)
	}

	for task, want := range map[protocol.TaskID]placement{"parent": placementAny, "child": placementParent} {
		var state, placed string
		if err := store.db.QueryRowContext(t.Context(), `SELECT state, placement FROM tasks WHERE id = ?`, string(task)).Scan(&state, &placed); err != nil {
			t.Fatal(err)
		}
		if TaskState(state) != TaskQueued || placement(placed) != want {
			t.Errorf("task %s is %s, placed %s; want queued, placed %s", task, state, placed, want)
		}
	}
}

func TestGivenMovedChildWhenItsParentRunsOnADaemonWithAFreeSlotThenTheChildStartsThereAndNeverWhereItRanBefore(t *testing.T) {
	child := placed(start(1, "child", "laptop", PriorityNormal), placementParent)
	child.Ran = []protocol.DaemonID{"vps"}
	anywhere := placed(start(2, "other", "laptop", PriorityNormal), placementAny)
	anywhere.Ran = []protocol.DaemonID{"pi"}
	s := schedule{
		slots: map[protocol.DaemonID]int{"laptop": 1, "pi": 3, "vps": 3},
		turns: []pendingTurn{child, anywhere},
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{"child@laptop", "other@vps"})
}

// lastCommandID is the id of the last command issued to daemon.
func lastCommandID(t *testing.T, srv testServer, daemon protocol.DaemonID) uint64 {
	t.Helper()
	var id int64
	if err := srv.store.db.QueryRowContext(t.Context(), `SELECT coalesce(max(id), 0) FROM commands WHERE daemon_id = ?`, string(daemon)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return uint64(id)
}

func TestGivenMovedTaskWhenNotingTheMoveThenOnlyATaskWithARepositoryIsToldItsPushedCommitsSurvive(t *testing.T) {
	withRepository := movedNote("vps", true, nil, "")
	without := movedNote("vps", false, nil, "")

	if !strings.Contains(withRepository, "The commits pushed from there are on your task's branch") ||
		!strings.Contains(withRepository, "only the work that was not pushed is gone") {
		t.Errorf("note with a repository = %q", withRepository)
	}
	if strings.Contains(without, "branch") || !strings.Contains(without, "Everything you did there is gone") {
		t.Errorf("note without a repository = %q", without)
	}
}

func TestGivenTaskThatCostSomethingWhenItMovesAndItsNewSessionReportsThenItsCostIsTheSumOfBothSessions(t *testing.T) {
	clock := newTestClock()
	srv := startLossServer(t, clock)
	vps := connectDaemon(t, srv, "vps")
	task := runningOn(t, srv, vps, "count to three")
	status, body := postEvents(t, srv, "vps", event(task, 3, `{"type":"result","subtype":"success","total_cost_usd":0.5}`))
	requireAcks(t, status, body, map[protocol.TaskID]uint64{task: 3})
	laptop := connectDaemon(t, srv, "laptop")
	vps.vanish()
	clock.advance(lossTimeout)
	srv.pass(t)
	laptop.nextCommand(t, task, protocol.CommandStartTask)
	if got := taskState(t, srv, task).CostUSD; got != 0.5 {
		t.Fatalf("cost after the move = %v, want the first session's 0.5", got)
	}

	status, body = postEvents(t, srv, "laptop",
		taskControlEvent(task, 1, protocol.KindHarnessStarted, started),
		event(task, 2, `{"type":"result","subtype":"success","total_cost_usd":0.125}`),
		event(task, 3, `{"type":"result","subtype":"success","total_cost_usd":0.25}`))
	requireAcks(t, status, body, map[protocol.TaskID]uint64{task: 3})

	if got := taskState(t, srv, task).CostUSD; got != 0.75 {
		t.Errorf("cost = %v, want 0.5 from the first session and 0.25 from the second", got)
	}
}

func TestGivenTwoPromptsQueuedForAFinishedTaskWhileItsDaemonIsAwayWhenItIsLostThenTheNewStartCarriesBothInOrderAndTheMoveEntryListsThem(t *testing.T) {
	clock := newTestClock()
	srv := startLossServer(t, clock)
	vps := connectDaemon(t, srv, "vps")
	task := runningOn(t, srv, vps, "count to three")
	status, body := postEvents(t, srv, "vps", taskControlEvent(task, 3, protocol.KindHarnessExited, cleanly))
	requireAcks(t, status, body, map[protocol.TaskID]uint64{task: 3})
	vps.vanish()
	waitConnected(t, srv, "vps", false)
	for _, text := range []string{"now count to four", "then count to five"} {
		status, body := doRequest(t, http.MethodPost, srv.url+"/v1/tasks/"+string(task)+"/commands", `{"kind":"prompt","payload":{"text":"`+text+`"}}`)
		if status != http.StatusAccepted {
			t.Fatalf("prompt: %d %s", status, body)
		}
	}
	srv.pass(t)
	laptop := connectDaemon(t, srv, "laptop")
	clock.advance(lossTimeout)
	srv.pass(t)

	prompt := decodeStart(t, laptop.nextCommand(t, task, protocol.CommandStartTask)).Prompt
	want := "oldest first:\n\nOwner's prompt 1 of 2:\n\nnow count to four\n\nOwner's prompt 2 of 2:\n\nthen count to five"
	if !strings.HasPrefix(prompt, "count to three\n\n") || !strings.HasSuffix(prompt, want) {
		t.Errorf("new start's prompt = %q, want the first prompt and a note carrying both queued prompts in order", prompt)
	}
	laptop.noCommand(t, task, protocol.CommandPrompt)
	entries, err := srv.Transcript(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	listed := slices.ContainsFunc(entries, func(entry transcript.Entry) bool {
		body, ok := entry.Body.(transcript.TaskMoved)
		return ok && strings.HasSuffix(body.Prompt, want)
	})
	if !listed {
		t.Errorf("transcript's move entry does not list both queued prompts: %+v", entries)
	}
}

// refuseStream opens daemon's command stream from an epoch the server's
// lineage does not hold, which the server refuses.
func refuseStream(t *testing.T, srv testServer, daemon protocol.DaemonID) {
	t.Helper()
	status, body := openStreamOnce(t, srv, daemon, "0123456789abcdef0123456789abcdef:1")
	requireRefused(t, status, body, protocol.RefusedUnknownEpoch)
}

// keepSeen has daemon ask for its acknowledgements, which any other
// daemon would count as being seen.
func keepSeen(t *testing.T, srv testServer, daemon protocol.DaemonID) {
	t.Helper()
	if status, body := doRequest(t, http.MethodGet, srv.url+"/v1/daemons/"+string(daemon)+"/acks", ""); status != http.StatusOK {
		t.Fatalf("acks: %d %s", status, body)
	}
}

func TestGivenARefusedStreamWhenTheTimeoutPassesThoughTheDaemonKeepsCallingThenItIsLostAndItsRunningTaskMoves(t *testing.T) {
	clock := newTestClock()
	srv := startLossServer(t, clock)
	vps := connectDaemon(t, srv, "vps")
	task := runningOn(t, srv, vps, "count to three")
	vps.vanish()
	refuseStream(t, srv, "vps")

	clock.advance(lossTimeout - time.Second)
	keepSeen(t, srv, "vps")
	srv.pass(t)
	if lostAt(t, srv, "vps") != nil {
		t.Fatal("vps is lost before the timeout")
	}
	laptop := connectDaemon(t, srv, "laptop")
	clock.advance(time.Second)
	keepSeen(t, srv, "vps")
	refuseStream(t, srv, "vps")
	srv.pass(t)

	if lostAt(t, srv, "vps") == nil {
		t.Fatal("vps is not lost a timeout after its first refused stream")
	}
	laptop.nextCommand(t, task, protocol.CommandStartTask)
	keepSeen(t, srv, "vps")
	if lostAt(t, srv, "vps") == nil {
		t.Error("a call from the refused daemon made it no longer lost")
	}
}

func TestGivenALostRefusedDaemonWhenItsStreamIsAcceptedThenItIsNoLongerLostOrRefused(t *testing.T) {
	clock := newTestClock()
	srv := startLossServer(t, clock)
	connectDaemon(t, srv, "vps").vanish()
	refuseStream(t, srv, "vps")
	clock.advance(lossTimeout)
	srv.pass(t)
	if lostAt(t, srv, "vps") == nil {
		t.Fatal("vps is not lost")
	}

	connectDaemon(t, srv, "vps")

	if at := lostAt(t, srv, "vps"); at != nil {
		t.Errorf("vps is lost at %v after its stream was accepted", at)
	}
	if listed := listedRefusal(t, srv, "vps"); listed != nil {
		t.Errorf("the daemon list still shows %+v", listed)
	}
}

func TestGivenRepeatedRefusalsWhenListedThenTheRefusalCountsFromTheFirst(t *testing.T) {
	clock := newTestClock()
	srv := startLossServer(t, clock)
	connectDaemon(t, srv, "vps").vanish()
	first := clock.now()
	refuseStream(t, srv, "vps")
	clock.advance(5 * time.Minute)

	refuseStream(t, srv, "vps")

	listed := listedRefusal(t, srv, "vps")
	if listed == nil || !listed.Since.Equal(first) || !listed.At.Equal(first.Add(5*time.Minute)) {
		t.Errorf("the daemon list shows %+v, want refused since %v, last at %v", listed, first, first.Add(5*time.Minute))
	}
}
