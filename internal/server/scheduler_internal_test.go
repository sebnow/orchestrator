package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// schedNow is the scheduler's clock in the tests of decide.
var schedNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

var testPolicy = SchedulePolicy{SlotsPerDaemon: 2, FillerThreshold: 0.5, LowThreshold: 0.85}

// fiveHourAt is a reading with the five-hour window used to utilization,
// resetting in an hour.
func fiveHourAt(utilization float64) *quotaReading {
	return &quotaReading{
		QuotaObserved: protocol.QuotaObserved{Status: protocol.QuotaAllowed, Windows: []protocol.QuotaWindow{
			{Name: fiveHourWindow, Utilization: utilization, ResetsAt: schedNow.Add(time.Hour)},
			{Name: "seven_day", Utilization: 0.2, ResetsAt: schedNow.Add(72 * time.Hour)},
		}},
		At: schedNow.Add(-time.Minute),
	}
}

// start is the first turn of a new task bound to daemon.
func start(id uint64, task protocol.TaskID, daemon protocol.DaemonID, priority Priority) pendingTurn {
	return pendingTurn{ID: id, Task: task, Kind: turnStart, State: TaskQueued, Priority: priority, Daemon: daemon, Placement: placementBound}
}

func filler(turn pendingTurn) pendingTurn {
	turn.Filler = true
	return turn
}

func placed(turn pendingTurn, how placement) pendingTurn {
	turn.Placement = how
	return turn
}

// later is a turn of kind for a task in state on daemon.
func later(id uint64, task protocol.TaskID, kind turnKind, state TaskState, daemon protocol.DaemonID) pendingTurn {
	return pendingTurn{ID: id, Task: task, Kind: kind, State: state, Priority: PriorityNormal, Daemon: daemon, Placement: placementBound}
}

func running(task protocol.TaskID, daemon protocol.DaemonID, isFiller bool, admitted uint64) slotHolder {
	return slotHolder{Task: task, Daemon: daemon, State: TaskRunning, Filler: isFiller, Admitted: admitted}
}

// admitted lists the turns d admits as "turn@daemon", in order.
func admitted(d decisions) []string {
	out := []string{}
	for _, a := range d.admit {
		out = append(out, string(a.turn.Task)+"@"+string(a.daemon))
	}
	return out
}

func yielded(d decisions) []protocol.TaskID {
	out := []protocol.TaskID{}
	for _, holder := range d.yield {
		out = append(out, holder.Task)
	}
	return out
}

func requireStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func requireReason(t *testing.T, d decisions, turn uint64, want string) {
	t.Helper()
	if got := d.reasons[turn]; !strings.Contains(got, want) {
		t.Errorf("turn %d waits because %q, want it to mention %q", turn, got, want)
	}
}

func TestGivenTurnsOfEachPriorityWhenSlotsAreShortThenTheHighestAndOldestAreAdmittedFirst(t *testing.T) {
	s := schedule{
		slots: map[protocol.DaemonID]int{"laptop": 2},
		turns: []pendingTurn{
			start(1, "low", "laptop", PriorityLow),
			start(2, "normal-old", "laptop", PriorityNormal),
			start(3, "high", "laptop", PriorityHigh),
			start(4, "normal-new", "laptop", PriorityNormal),
		},
		reading: fiveHourAt(0.1),
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{"high@laptop", "normal-old@laptop"})
	requireReason(t, d, 4, "slots: daemon laptop has no free slot")
	requireReason(t, d, 1, "slots: daemon laptop has no free slot")
}

func TestGivenTwoDaemonsWhenTurnsArePlacedThenEachDaemonTakesNoMoreThanItsFreeSlots(t *testing.T) {
	s := schedule{
		slots:   map[protocol.DaemonID]int{"laptop": 1, "vps": 2},
		holders: []slotHolder{running("busy", "laptop", false, 1)},
		turns: []pendingTurn{
			placed(start(1, "a", "laptop", PriorityNormal), placementAny),
			placed(start(2, "b", "laptop", PriorityNormal), placementAny),
			placed(start(3, "c", "laptop", PriorityNormal), placementAny),
			start(4, "on-laptop", "laptop", PriorityNormal),
		},
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{"a@vps", "b@vps"})
	requireReason(t, d, 3, "slots: no connected daemon has a free slot")
	requireReason(t, d, 4, "slots: daemon laptop has no free slot")
}

func TestGivenFirstTurnsWhenPlacedThenTheNamedDaemonTheParentsOrTheFreestIsChosen(t *testing.T) {
	s := schedule{
		slots:   map[protocol.DaemonID]int{"a": 2, "b": 2, "c": 3},
		holders: []slotHolder{running("x", "c", false, 1), running("y", "c", false, 2)},
		turns: []pendingTurn{
			start(1, "named", "b", PriorityNormal),
			placed(start(2, "child", "a", PriorityNormal), placementParent),
			placed(start(3, "anywhere", "c", PriorityNormal), placementAny),
		},
	}

	d := decide(s, testPolicy, schedNow)

	// After the first two, a has 1 free, b 1 and c 1: the tie goes to the
	// lowest id.
	requireStrings(t, "admitted", admitted(d), []string{"named@b", "child@a", "anywhere@a"})
}

func TestGivenFullParentDaemonWhenAChildStartsThenItIsPlacedElsewhere(t *testing.T) {
	s := schedule{
		slots:   map[protocol.DaemonID]int{"laptop": 1, "vps": 1},
		holders: []slotHolder{running("parent", "laptop", false, 1)},
		turns:   []pendingTurn{placed(start(1, "child", "laptop", PriorityNormal), placementParent)},
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{"child@vps"})
}

func TestGivenLaterTurnsWhenAnotherDaemonIsFreerThenTheyStayOnTheTasksDaemon(t *testing.T) {
	s := schedule{
		slots:   map[protocol.DaemonID]int{"laptop": 2, "vps": 5},
		holders: []slotHolder{running("busy", "laptop", false, 1)},
		turns: []pendingTurn{
			later(1, "finished", turnPrompt, TaskFinished, "laptop"),
			later(2, "paused", turnResume, TaskPaused, "laptop"),
			later(3, "running", turnPrompt, TaskRunning, "laptop"),
		},
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{"finished@laptop", "running@laptop"})
	requireReason(t, d, 2, "slots: daemon laptop has no free slot")
}

func TestGivenDisconnectedDaemonWhenItsTurnsWaitThenTheySayWhy(t *testing.T) {
	d := decide(schedule{
		slots: map[protocol.DaemonID]int{"vps": 2},
		turns: []pendingTurn{start(1, "a", "laptop", PriorityNormal), later(2, "b", turnPrompt, TaskFinished, "laptop")},
	}, testPolicy, schedNow)
	requireStrings(t, "admitted", admitted(d), []string{})
	requireReason(t, d, 1, "daemon laptop is not connected")
	requireReason(t, d, 2, "daemon laptop is not connected")

	d = decide(schedule{turns: []pendingTurn{placed(start(1, "a", "laptop", PriorityNormal), placementAny)}}, testPolicy, schedNow)
	requireReason(t, d, 1, "no daemon is connected")
}

func TestGivenNormalTurnWaitingForASlotWhenFillerCouldRunElsewhereThenTheFillerIsHeld(t *testing.T) {
	s := schedule{
		slots:   map[protocol.DaemonID]int{"laptop": 1, "vps": 1},
		holders: []slotHolder{running("busy", "laptop", false, 1)},
		turns: []pendingTurn{
			filler(start(1, "filler", "vps", PriorityHigh)),
			start(2, "normal", "laptop", PriorityNormal),
		},
		reading: fiveHourAt(0.1),
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{})
	requireReason(t, d, 1, "priority: non-filler turns are waiting for a slot")

	s.holders = nil
	d = decide(s, testPolicy, schedNow)
	requireStrings(t, "admitted once nothing waits", admitted(d), []string{"normal@laptop", "filler@vps"})
}

func TestGivenFiveHourUtilizationWhenFillerAndLowTurnsWaitThenEachNeedsItsThreshold(t *testing.T) {
	turns := []pendingTurn{
		filler(start(1, "filler", "laptop", PriorityNormal)),
		start(2, "low", "laptop", PriorityLow),
		start(3, "normal", "laptop", PriorityNormal),
		start(4, "high", "laptop", PriorityHigh),
	}
	for _, tc := range []struct {
		utilization float64
		want        []string
	}{
		{0.49, []string{"high@laptop", "normal@laptop", "low@laptop", "filler@laptop"}},
		{0.5, []string{"high@laptop", "normal@laptop", "low@laptop"}},
		{0.84, []string{"high@laptop", "normal@laptop", "low@laptop"}},
		{0.85, []string{"high@laptop", "normal@laptop"}},
		{0.99, []string{"high@laptop", "normal@laptop"}},
	} {
		s := schedule{slots: map[protocol.DaemonID]int{"laptop": 10}, turns: turns, reading: fiveHourAt(tc.utilization)}
		d := decide(s, testPolicy, schedNow)
		requireStrings(t, "admitted at "+percent(tc.utilization), admitted(d), tc.want)
	}

	d := decide(schedule{slots: map[protocol.DaemonID]int{"laptop": 10}, turns: turns, reading: fiveHourAt(0.9)}, testPolicy, schedNow)
	requireReason(t, d, 1, "budget: the five-hour window is 90% used; filler runs below 50%")
	requireReason(t, d, 2, "budget: the five-hour window is 90% used; low priority runs below 85%")
}

func TestGivenRejectedReadingWhenItsEarliestWindowResetsThenTurnsAreHeldUntilThenAndReleasedAfter(t *testing.T) {
	reset := schedNow.Add(30 * time.Minute)
	s := schedule{
		slots: map[protocol.DaemonID]int{"laptop": 10},
		turns: []pendingTurn{
			start(1, "high", "laptop", PriorityHigh),
			later(2, "running", turnPrompt, TaskRunning, "laptop"),
			filler(start(3, "filler", "laptop", PriorityNormal)),
		},
		reading: &quotaReading{QuotaObserved: protocol.QuotaObserved{Status: protocol.QuotaRejected, Windows: []protocol.QuotaWindow{
			{Name: fiveHourWindow, Utilization: 1, ResetsAt: reset},
			{Name: "seven_day", Utilization: 0.5, ResetsAt: reset.Add(48 * time.Hour)},
		}}, At: schedNow.Add(-time.Minute)},
	}

	held := decide(s, testPolicy, schedNow)
	released := decide(s, testPolicy, reset.Add(time.Second))

	requireStrings(t, "admitted while rejected", admitted(held), []string{})
	for turn := range uint64(3) {
		requireReason(t, held, turn+1, "rejected by the account's usage limits until 2026-10-08 12:30 UTC")
	}
	if !held.wake.Equal(reset) {
		t.Errorf("wake = %s, want the reset at %s", held.wake, reset)
	}
	requireStrings(t, "admitted after the reset", admitted(released), []string{"high@laptop", "running@laptop"})
	requireReason(t, released, 3, "budget: filler needs a current reading")
}

func TestGivenNoReadingOrAStaleOneWhenTurnsWaitThenAllButFillerAreAdmitted(t *testing.T) {
	stale := fiveHourAt(0.1)
	stale.Windows[0].ResetsAt = schedNow.Add(-time.Minute)
	for name, reading := range map[string]*quotaReading{"none": nil, "stale": stale} {
		s := schedule{
			slots: map[protocol.DaemonID]int{"laptop": 10},
			turns: []pendingTurn{
				start(1, "low", "laptop", PriorityLow),
				filler(start(2, "filler", "laptop", PriorityHigh)),
			},
			reading: reading,
		}

		d := decide(s, testPolicy, schedNow)

		requireStrings(t, name+": admitted", admitted(d), []string{"low@laptop"})
		requireReason(t, d, 2, "budget: filler needs a current reading of the five-hour window")
	}
}

func TestGivenFullDaemonWithRunningFillerWhenANormalTurnWaitsThenTheNewestFillerIsYieldedOnce(t *testing.T) {
	s := schedule{
		slots: map[protocol.DaemonID]int{"laptop": 2, "vps": 1},
		holders: []slotHolder{
			running("older-filler", "laptop", true, 5),
			running("newer-filler", "laptop", true, 9),
			running("vps-filler", "vps", true, 12),
		},
		turns:   []pendingTurn{start(1, "normal", "laptop", PriorityNormal)},
		reading: fiveHourAt(0.1),
	}

	d := decide(s, testPolicy, schedNow)

	if got := yielded(d); !reflect.DeepEqual(got, []protocol.TaskID{"newer-filler"}) {
		t.Errorf("yielded %v, want the newest filler on laptop", got)
	}

	s.holders[1].State, s.holders[1].PausedBy = TaskPausing, pauseByScheduler
	d = decide(s, testPolicy, schedNow)
	if got := yielded(d); len(got) != 0 {
		t.Errorf("yielded %v while a yield is under way, want none", got)
	}
}

func TestGivenNoFillerRunningWhenANormalTurnWaitsThenNothingIsYielded(t *testing.T) {
	s := schedule{
		slots:   map[protocol.DaemonID]int{"laptop": 1},
		holders: []slotHolder{running("normal", "laptop", false, 1)},
		turns: []pendingTurn{
			start(1, "waiting", "laptop", PriorityHigh),
			filler(start(2, "filler", "laptop", PriorityNormal)),
		},
		reading: fiveHourAt(0.1),
	}

	if got := yielded(decide(s, testPolicy, schedNow)); len(got) != 0 {
		t.Errorf("yielded %v, want none", got)
	}
}

func TestGivenUnplacedTurnWithEveryDaemonFullWhenItWaitsThenItYieldsWhereAFillerRuns(t *testing.T) {
	s := schedule{
		slots:   map[protocol.DaemonID]int{"laptop": 1, "vps": 1},
		holders: []slotHolder{running("normal", "laptop", false, 3), running("filler", "vps", true, 1)},
		turns:   []pendingTurn{placed(start(1, "anywhere", "laptop", PriorityNormal), placementAny)},
	}

	if got := yielded(decide(s, testPolicy, schedNow)); !reflect.DeepEqual(got, []protocol.TaskID{"filler"}) {
		t.Errorf("yielded %v, want the filler on vps", got)
	}
}

func TestGivenTaskWithSeveralTurnsWhenScheduledThenTheyGoOneAtATimeAndADeliveryWaitsForTheTurnToEnd(t *testing.T) {
	s := schedule{
		slots: map[protocol.DaemonID]int{"laptop": 10},
		turns: []pendingTurn{
			later(1, "task", turnDeliver, TaskRunning, "laptop"),
			later(2, "task", turnPrompt, TaskRunning, "laptop"),
			later(3, "task", turnPrompt, TaskRunning, "laptop"),
			later(4, "paused", turnDeliver, TaskPaused, "laptop"),
			later(5, "yielding", turnPrompt, TaskPausing, "laptop"),
		},
	}
	s.turns[4].PausedBy = pauseByScheduler

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{"task@laptop"})
	if d.admit[0].turn.ID != 2 {
		t.Errorf("admitted turn %d, want the oldest prompt, 2", d.admit[0].turn.ID)
	}
	requireReason(t, d, 1, "waits for the task's turn to end")
	requireReason(t, d, 3, "waits behind the task's earlier turn")
	requireReason(t, d, 4, "waits for the owner to resume the task")
	requireReason(t, d, 5, "waits for the scheduler's pause of the task to settle")
}

// schedulerAt runs a scheduler pass over store at now, with laptop the
// only daemon connected and one slot, and returns the commands it issued.
func schedulerAt(t *testing.T, store *Store, now time.Time) []protocol.Command {
	t.Helper()
	before, err := store.commandsAfter(t.Context(), "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	policy := SchedulePolicy{SlotsPerDaemon: 1, FillerThreshold: 0.5, LowThreshold: 0.85}
	if _, err := store.schedule(t.Context(), policy, now, []protocol.DaemonID{"laptop"}); err != nil {
		t.Fatal(err)
	}
	var last uint64
	if len(before) > 0 {
		last = before[len(before)-1].ID
	}
	issued, err := store.commandsAfter(t.Context(), "laptop", last)
	if err != nil {
		t.Fatal(err)
	}
	return issued
}

func kinds(commands []protocol.Command) []string {
	out := []string{}
	for _, command := range commands {
		out = append(out, string(command.TaskID)+":"+string(command.Kind))
	}
	return out
}

// quotaEvent reports the five-hour window used to utilization, resetting
// an hour after now.
func quotaEvent(utilization float64, now time.Time) (protocol.Kind, string) {
	payload, _ := json.Marshal(protocol.QuotaObserved{Status: protocol.QuotaAllowed, Windows: []protocol.QuotaWindow{
		{Name: fiveHourWindow, Utilization: utilization, ResetsAt: now.Add(time.Hour)},
	}})
	return protocol.KindQuotaObserved, string(payload)
}

// readingFromVPS has a task on the daemon vps report the five-hour
// window used to utilization at now.
func readingFromVPS(t *testing.T, store *Store, now time.Time, utilization float64) {
	t.Helper()
	queueTask(t, store, ownersTask("reporter", "vps"))
	if _, err := store.schedule(t.Context(), roomyPolicy, now, []protocol.DaemonID{"vps"}); err != nil {
		t.Fatal(err)
	}
	kind, payload := quotaEvent(utilization, now)
	e := event("reporter", 1, payload)
	e.Kind, e.Time = kind, now
	if _, _, err := store.appendEvents(t.Context(), "vps", []protocol.Event{e}); err != nil {
		t.Fatal(err)
	}
}

// heldTask queues task on laptop with priority and filler, and returns
// its lifecycle, which leaves admission to the test.
func heldTask(t *testing.T, store *Store, task protocol.TaskID, priority Priority, isFiller bool) *lifecycle {
	t.Helper()
	spec := ownersTask(task, "laptop")
	spec.Priority, spec.Filler = priority, isFiller
	queueTask(t, store, spec)
	return &lifecycle{t: t, store: store, task: task, held: true}
}

func TestGivenRunningFillerOnAFullDaemonWhenANormalTaskArrivesThenTheFillerYieldsAndResumesOnceTheSlotIsFree(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now()
	fill := heldTask(t, store, "filler", PriorityNormal, true)
	requireStrings(t, "issued with no reading", kinds(schedulerAt(t, store, now)), []string{})
	readingFromVPS(t, store, now, 0.1)
	requireStrings(t, "issued with a reading", kinds(schedulerAt(t, store, now)), []string{"filler:start_task"})
	fill.event(protocol.KindHarnessStarted, started, TaskRunning)

	normal := heldTask(t, store, "normal", PriorityNormal, false)
	requireStrings(t, "issued once the normal task waits", kinds(schedulerAt(t, store, now)), []string{"filler:pause"})
	requireStrings(t, "issued while the yield settles", kinds(schedulerAt(t, store, now)), []string{})
	fill.event(protocol.KindPauseAcknowledged, `{"note":"after step 1"}`, TaskPausing)
	fill.event(protocol.KindPauseSettled, settled, TaskYielded)
	requireStrings(t, "issued once the filler yielded", kinds(schedulerAt(t, store, now)), []string{"normal:start_task"})
	fill.event(protocol.KindHarnessExited, cleanly, TaskYielded)
	requireStrings(t, "issued while the normal task runs", kinds(schedulerAt(t, store, now)), []string{})

	normal.event(protocol.KindHarnessStarted, started, TaskRunning)
	normal.event(protocol.KindHarnessExited, cleanly, TaskFinished)
	requireStrings(t, "issued once the slot is free", kinds(schedulerAt(t, store, now)), []string{"filler:resume"})
	if state := readProgress(t, store, "filler").State; state != TaskRunning {
		t.Errorf("filler state = %s, want running", state)
	}
}

func TestGivenYieldedFillerWhenTheOwnerResumesItThenItsResumeIsOrdinaryAndRunsDespiteTheBudget(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now()
	readingFromVPS(t, store, now, 0.1)
	fill := heldTask(t, store, "filler", PriorityNormal, true)
	if _, err := store.schedule(t.Context(), roomyPolicy, now, []protocol.DaemonID{"laptop"}); err != nil {
		t.Fatal(err)
	}
	fill.event(protocol.KindHarnessStarted, started, TaskRunning)
	fill.yield(TaskPausing)
	kind, payload := quotaEvent(0.9, now)
	fill.event(kind, payload, TaskPausing)
	fill.event(protocol.KindPauseSettled, settled, TaskYielded)
	fill.event(protocol.KindHarnessExited, cleanly, TaskYielded)
	requireStrings(t, "issued while the budget holds filler", kinds(schedulerAt(t, store, now)), []string{})

	turn, err := store.queueCommand(t.Context(), "filler", turnResume, nil)
	if err != nil {
		t.Fatal(err)
	}

	requireStrings(t, "issued after the owner's resume", kinds(schedulerAt(t, store, now)), []string{"filler:resume"})
	var count int
	if err := store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM turns WHERE task_id = 'filler' AND kind = 'resume'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 || turn.Kind != turnResume {
		t.Errorf("%d resume turns, owner's turn %+v; want the scheduler's resume taken over", count, turn)
	}
}

func TestGivenFinishedRecipientOnAFullDaemonWhenMessagesArriveThenOnePromptCarriesThemOnceASlotIsFree(t *testing.T) {
	store, _ := openTestStore(t)
	now := time.Now()
	recipient := heldTask(t, store, "recipient", PriorityNormal, false)
	schedulerAt(t, store, now)
	recipient.drive(TaskFinished)
	sender := heldTask(t, store, "sender", PriorityNormal, false)
	schedulerAt(t, store, now)
	sender.drive(TaskRunning)

	first, err := store.sendMessage(t.Context(), "laptop", "sender", protocol.Send{To: "recipient", Text: "one"})
	if err != nil {
		t.Fatal(err)
	}
	requireStrings(t, "issued while the sender holds the slot", kinds(schedulerAt(t, store, now)), []string{})
	second, err := store.sendMessage(t.Context(), "laptop", "sender", protocol.Send{To: "recipient", Text: "two"})
	if err != nil {
		t.Fatal(err)
	}
	sender.event(protocol.KindHarnessExited, cleanly, TaskFinished)

	issued := schedulerAt(t, store, now)
	requireStrings(t, "issued once the slot is free", kinds(issued), []string{"recipient:prompt"})
	if !first || !second {
		t.Errorf("delivered = %v, %v; want both queued for the recipient's next turn", first, second)
	}
	requirePrompts(t, prompts(t, store, "recipient"), []protocol.Prompt{{
		Text: "Message from task sender: one\n\nMessage from task sender: two", From: fromTask("sender"),
	}})
}

func TestGivenChildThatFailsWhenItsParentIsFinishedThenTheNoticeIsQueuedAsATurn(t *testing.T) {
	store, _ := openTestStore(t)
	parent := taskIn(t, store, "parent", TaskRunning)
	child := spawned(t, store, "parent", "child")
	parent.event(protocol.KindHarnessExited, cleanly, TaskFinished)
	child.held = true

	child.drive(TaskFailed)

	var kind, origin string
	if err := store.db.QueryRowContext(t.Context(), `SELECT kind, origin FROM turns WHERE task_id = 'parent' AND admitted_command_id IS NULL`).Scan(&kind, &origin); err != nil {
		t.Fatalf("no waiting turn for the parent: %v", err)
	}
	if kind != string(turnDeliver) || origin != string(originServer) {
		t.Errorf("parent's turn is a %s from %s, want the server's delivery", kind, origin)
	}
	if got := prompts(t, store, "parent"); len(got) != 0 {
		t.Errorf("prompts before admission = %+v", got)
	}
}
