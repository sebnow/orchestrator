package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func controlEvent(kind protocol.Kind, payload string) protocol.Event {
	return protocol.Event{Kind: kind, Payload: json.RawMessage(payload)}
}

func TestGivenEachStateWhenACommandIsIssuedThenTheStateFollowsTheTable(t *testing.T) {
	for _, tc := range []struct {
		from TaskState
		kind protocol.CommandKind
		want TaskState
	}{
		{TaskPending, protocol.CommandStartTask, TaskPending},
		{TaskPending, protocol.CommandPause, TaskPausing},
		{TaskRunning, protocol.CommandPause, TaskPausing},
		{TaskAwaitingPermission, protocol.CommandPause, TaskPausing},
		{TaskPaused, protocol.CommandPause, TaskPaused},
		{TaskPausing, protocol.CommandResume, TaskRunning},
		{TaskPaused, protocol.CommandResume, TaskRunning},
		{TaskRunning, protocol.CommandResume, TaskRunning},
		{TaskPaused, protocol.CommandPrompt, TaskRunning},
		{TaskPausing, protocol.CommandPrompt, TaskRunning},
		{TaskAwaitingPermission, protocol.CommandPrompt, TaskAwaitingPermission},
		{TaskAwaitingPermission, protocol.CommandAnswerPermission, TaskRunning},
		{TaskRunning, protocol.CommandAnswerPermission, TaskRunning},
		{TaskRunning, protocol.CommandInterrupt, TaskRunning},
		{TaskPausing, protocol.CommandInterrupt, TaskPausing},
		{TaskRunning, protocol.CommandStop, TaskRunning},
		{TaskFinished, protocol.CommandPrompt, TaskRunning},
		{TaskFinished, protocol.CommandResume, TaskRunning},
		{TaskQueued, protocol.CommandStartTask, TaskPending},
		{TaskQueued, protocol.CommandStop, TaskStopped},
		{TaskQueued, protocol.CommandPause, TaskQueued},
		{TaskFinished, protocol.CommandPause, TaskFinished},
		{TaskFinished, protocol.CommandInterrupt, TaskFinished},
		{TaskFinished, protocol.CommandStop, TaskStopped},
		{TaskPaused, protocol.CommandStop, TaskStopped},
		{TaskStopped, protocol.CommandPrompt, TaskRunning},
		{TaskFailed, protocol.CommandPrompt, TaskRunning},
		{TaskStopped, protocol.CommandResume, TaskRunning},
		{TaskFailed, protocol.CommandResume, TaskRunning},
		{TaskFailed, protocol.CommandPause, TaskFailed},
		{TaskFailed, protocol.CommandStop, TaskStopped},
		{TaskStopped, protocol.CommandInterrupt, TaskStopped},
	} {
		if got := tc.from.afterCommand(tc.kind); got != tc.want {
			t.Errorf("%s + %s command = %s, want %s", tc.from, tc.kind, got, tc.want)
		}
	}
}

func TestGivenEachStateWhenAnEventIsStoredThenTheStateFollowsTheTable(t *testing.T) {
	started := controlEvent(protocol.KindHarnessStarted, `{"pid":1}`)
	permission := controlEvent(protocol.KindPermissionRequested, `{"request_id":"r1","tool":"Bash","input":{}}`)
	settled := controlEvent(protocol.KindPauseSettled, `{"interrupted":false}`)
	exitedCleanly := controlEvent(protocol.KindHarnessExited, `{"exit_code":0}`)
	exitedNonZero := controlEvent(protocol.KindHarnessExited, `{"exit_code":1}`)
	restarted := controlEvent(protocol.KindHarnessExited, `{"exit_code":-1,"error":"daemon restarted"}`)
	cutShort := controlEvent(protocol.KindHarnessExited, `{"exit_code":-1,"error":"daemon restarted during the turn"}`)
	cutShortNoSession := controlEvent(protocol.KindHarnessExited, `{"exit_code":-1,"error":"daemon restarted during the turn, before the harness reported a session"}`)
	cutShortWithCode := controlEvent(protocol.KindHarnessExited, `{"exit_code":1,"error":"daemon restarted during the turn"}`)
	stoppedMidTurn := controlEvent(protocol.KindHarnessExited, `{"exit_code":-1,"error":"daemon stopped during the turn"}`)
	stoppedNoSession := controlEvent(protocol.KindHarnessExited, `{"exit_code":-1,"error":"daemon stopped during the turn, before the harness reported a session"}`)
	stoppedWithCode := controlEvent(protocol.KindHarnessExited, `{"exit_code":1,"error":"daemon stopped during the turn"}`)
	neverStarted := controlEvent(protocol.KindHarnessExited, `{"exit_code":-1,"error":"clone https://example.com/r.git: exit status 128"}`)
	zeroWithError := controlEvent(protocol.KindHarnessExited, `{"exit_code":0,"error":"read harness output: broken pipe"}`)
	output := controlEvent(protocol.KindHarnessOutput, `{"type":"assistant"}`)
	for _, tc := range []struct {
		name       string
		from       TaskState
		event      protocol.Event
		stopIssued bool
		want       TaskState
	}{
		{"start", TaskPending, started, false, TaskRunning},
		{"start while a pause waits", TaskPausing, started, false, TaskPausing},
		{"permission asked", TaskRunning, permission, false, TaskAwaitingPermission},
		{"permission asked while pausing", TaskPausing, permission, false, TaskPausing},
		{"pause settles", TaskPausing, settled, false, TaskPaused},
		{"pause settles after a resume", TaskRunning, settled, false, TaskRunning},
		{"harness output", TaskRunning, output, false, TaskRunning},
		{"clean exit", TaskRunning, exitedCleanly, false, TaskFinished},
		{"clean exit while paused", TaskPaused, exitedCleanly, false, TaskPaused},
		{"clean exit before the pause settled", TaskPausing, exitedCleanly, false, TaskFinished},
		{"failed exit while paused", TaskPaused, exitedNonZero, false, TaskFailed},
		{"next process of a finished task", TaskFinished, started, false, TaskRunning},
		{"next process of a paused task", TaskPaused, started, false, TaskRunning},
		{"exit after stop", TaskRunning, exitedCleanly, true, TaskStopped},
		{"killed after stop", TaskPausing, restarted, true, TaskStopped},
		{"non-zero exit", TaskRunning, exitedNonZero, false, TaskFailed},
		{"restart of a daemon that cannot resume", TaskAwaitingPermission, restarted, false, TaskFailed},
		{"restart cut the turn short", TaskRunning, cutShort, false, TaskPaused},
		{"restart cut a permission request short", TaskAwaitingPermission, cutShort, false, TaskPaused},
		{"restart cut a pause short", TaskPausing, cutShort, false, TaskPaused},
		{"restart cut the start short", TaskPending, cutShortNoSession, false, TaskPaused},
		{"restart after a stop", TaskRunning, cutShort, true, TaskStopped},
		{"restart text with an exit code", TaskRunning, cutShortWithCode, false, TaskFailed},
		{"restart after the yield settled", TaskYielded, cutShort, false, TaskYielded},
		{"restart after the pause settled", TaskPaused, cutShort, false, TaskPaused},
		{"restart after a clean exit was journaled", TaskFinished, cutShort, false, TaskFinished},
		{"shutdown cut the turn short", TaskRunning, stoppedMidTurn, false, TaskPaused},
		{"shutdown cut a permission request short", TaskAwaitingPermission, stoppedMidTurn, false, TaskPaused},
		{"shutdown cut a pause short", TaskPausing, stoppedMidTurn, false, TaskPaused},
		{"shutdown cut the first turn short before a session", TaskRunning, stoppedNoSession, false, TaskPaused},
		{"shutdown after a stop", TaskRunning, stoppedMidTurn, true, TaskStopped},
		{"shutdown text with an exit code", TaskRunning, stoppedWithCode, false, TaskFailed},
		{"clone failed", TaskPending, neverStarted, false, TaskFailed},
		{"exit 0 with an error", TaskRunning, zeroWithError, false, TaskFailed},
		{"undecodable exit", TaskRunning, controlEvent(protocol.KindHarnessExited, `"gone"`), false, TaskFailed},
		{"exit after a stop took effect", TaskStopped, exitedNonZero, false, TaskStopped},
		{"next process of a failed task", TaskFailed, started, false, TaskRunning},
		{"next process of a stopped task", TaskStopped, started, false, TaskRunning},
		{"failed exit of a resumed failed task", TaskRunning, exitedNonZero, false, TaskFailed},
	} {
		if got := tc.from.afterEvent(tc.event, tc.stopIssued, pauseByOwner); got != tc.want {
			t.Errorf("%s: %s + %s = %s, want %s", tc.name, tc.from, tc.event.Kind, got, tc.want)
		}
	}
}

func readProgress(t *testing.T, store *Store, task protocol.TaskID) progress {
	t.Helper()
	tx, err := store.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	p, err := loadProgress(t.Context(), tx, task)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// lifecycle drives a stored task through events and commands, checking
// its state after each step.
type lifecycle struct {
	t     *testing.T
	store *Store
	task  protocol.TaskID
	seq   uint64
	// held keeps the scheduler from admitting turns after each step.
	held bool
}

func (l *lifecycle) event(kind protocol.Kind, payload string, want TaskState) {
	l.t.Helper()
	l.seq++
	e := event(l.task, l.seq, payload)
	e.Kind = kind
	e.Time = time.Now()
	if _, _, err := l.store.appendEvents(l.t.Context(), "laptop", []protocol.Event{e}); err != nil {
		l.t.Fatal(err)
	}
	if !l.held {
		admitTurns(l.t, l.store)
	}
	if got := readProgress(l.t, l.store, l.task).State; got != want {
		l.t.Fatalf("after %s event: state = %s, want %s", kind, got, want)
	}
}

func (l *lifecycle) command(kind protocol.CommandKind, payload string, want TaskState) {
	l.t.Helper()
	var raw json.RawMessage
	if payload != "" {
		raw = json.RawMessage(payload)
	}
	if _, err := l.store.issueCommand(l.t.Context(), l.task, kind, raw); err != nil {
		l.t.Fatal(err)
	}
	if !l.held {
		admitTurns(l.t, l.store)
	}
	if got := readProgress(l.t, l.store, l.task).State; got != want {
		l.t.Fatalf("after %s command: state = %s, want %s", kind, got, want)
	}
}

func newLifecycle(t *testing.T, store *Store, task protocol.TaskID) *lifecycle {
	t.Helper()
	seedTask(t, store, "laptop", task)
	if got := readProgress(t, store, task).State; got != TaskPending {
		t.Fatalf("new task state = %s, want pending", got)
	}
	return &lifecycle{t: t, store: store, task: task}
}

func TestGivenTaskWhenItRunsThroughEveryTransitionAndExitsCleanlyThenItIsFinishedAndAPromptResumesIt(t *testing.T) {
	store, _ := openTestStore(t)
	l := newLifecycle(t, store, "task-1")

	l.event(protocol.KindHarnessStarted, `{"pid":1,"model":"haiku","workdir":"/w"}`, TaskRunning)
	l.event(protocol.KindPermissionRequested, `{"request_id":"r1","tool":"Bash","input":{}}`, TaskAwaitingPermission)
	l.command(protocol.CommandAnswerPermission, `{"request_id":"r1","allow":true}`, TaskRunning)
	l.command(protocol.CommandPause, "", TaskPausing)
	l.event(protocol.KindPauseAcknowledged, `{"note":"after step 1"}`, TaskPausing)
	l.command(protocol.CommandInterrupt, "", TaskPausing)
	l.event(protocol.KindPauseSettled, `{"interrupted":false}`, TaskPaused)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskPaused)
	l.command(protocol.CommandResume, "", TaskRunning)
	l.event(protocol.KindHarnessStarted, `{"pid":2}`, TaskRunning)
	l.command(protocol.CommandInterrupt, "", TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskFinished)
	l.command(protocol.CommandPrompt, `{"text":"more"}`, TaskRunning)
	l.event(protocol.KindHarnessStarted, `{"pid":3}`, TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskFinished)
}

func TestGivenResumeIssuedBeforeTheOldProcessExitReachesTheServerWhenEventsArriveThenTheTaskEndsUpRunning(t *testing.T) {
	store, _ := openTestStore(t)
	l := newLifecycle(t, store, "task-1")

	l.event(protocol.KindHarnessStarted, `{"pid":1}`, TaskRunning)
	l.command(protocol.CommandPause, "", TaskPausing)
	l.event(protocol.KindPauseSettled, `{"interrupted":false}`, TaskPaused)
	l.command(protocol.CommandResume, "", TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskFinished)
	l.event(protocol.KindHarnessStarted, `{"pid":2}`, TaskRunning)
}

func TestGivenTaskBetweenProcessesWhenStoppedThenItIsStoppedAtOnceAndTakesNoMoreCommands(t *testing.T) {
	store, _ := openTestStore(t)
	l := newLifecycle(t, store, "task-1")
	l.event(protocol.KindHarnessStarted, `{"pid":1}`, TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskFinished)

	l.command(protocol.CommandStop, "", TaskStopped)

	_, err := store.issueCommand(t.Context(), "task-1", protocol.CommandPrompt, json.RawMessage(`{"text":"more"}`))
	if !errors.Is(err, errTaskEnded) {
		t.Errorf("prompt to a stopped task: %v, want errTaskEnded", err)
	}
}

func TestGivenStopIssuedWhenTheHarnessExitsThenTheTaskIsStopped(t *testing.T) {
	store, _ := openTestStore(t)
	l := newLifecycle(t, store, "task-1")

	l.event(protocol.KindHarnessStarted, `{"pid":1}`, TaskRunning)
	l.command(protocol.CommandStop, "", TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskStopped)
}

func TestGivenRunningTaskWhenADaemonThatCannotResumeItRestartsThenTheTaskFailed(t *testing.T) {
	store, _ := openTestStore(t)
	l := newLifecycle(t, store, "task-1")

	l.event(protocol.KindHarnessStarted, `{"pid":1}`, TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":-1,"error":"daemon restarted"}`, TaskFailed)
}

func TestGivenRunningTaskWhenADaemonRestartCutsItsTurnShortThenItIsPausedForTheOwnerAndAResumeRunsIt(t *testing.T) {
	store, _ := openTestStore(t)
	l := newLifecycle(t, store, "task-1")

	l.event(protocol.KindHarnessStarted, `{"pid":1}`, TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":-1,"error":"`+exitRestarted+`"}`, TaskPaused)
	if p := readProgress(t, store, "task-1"); p.PausedBy != pauseByOwner {
		t.Errorf("paused by %q, want the owner", p.PausedBy)
	}
	l.command(protocol.CommandResume, "", TaskRunning)
	l.event(protocol.KindHarnessStarted, `{"pid":2}`, TaskRunning)
}

func TestGivenYieldUnderWayWhenADaemonRestartCutsTheTurnShortThenTheTaskWaitsForTheOwner(t *testing.T) {
	p := progress{State: TaskPausing, PausedBy: pauseByScheduler}

	p.seeEvent(controlEvent(protocol.KindHarnessExited, `{"exit_code":-1,"error":"`+exitRestarted+`"}`))

	if p.State != TaskPaused || p.PausedBy != pauseByOwner {
		t.Errorf("progress = %+v, want paused by the owner", p)
	}
}

func TestGivenEventsAndCommandsWhenStoredThenLastActivityIsTheLatestAndCostTheHighestRunningTotal(t *testing.T) {
	store, _ := openTestStore(t)
	seedTask(t, store, "laptop", "task-1")
	future := time.Now().Add(time.Hour).Truncate(time.Second)
	result := func(seq uint64, cost string, at time.Time) protocol.Event {
		e := event("task-1", seq, `{"type":"result","subtype":"success","total_cost_usd":`+cost+`}`)
		e.Time = at
		return e
	}
	// The second batch repeats seq 2, which must not count twice, and
	// carries a lower total stamped earlier, which must not win.
	batches := [][]protocol.Event{
		{result(1, "0.02", future), result(2, "0.03", future.Add(time.Second))},
		{result(2, "0.03", future.Add(time.Second)), result(3, "0.01", future.Add(-time.Minute))},
	}
	for _, batch := range batches {
		if _, _, err := store.appendEvents(t.Context(), "laptop", batch); err != nil {
			t.Fatal(err)
		}
	}

	p := readProgress(t, store, "task-1")
	if p.CostUSD != 0.03 || !p.LastActivity.Equal(future.Add(time.Second)) {
		t.Errorf("progress = %+v, want cost 0.03 and last activity %s", p, future.Add(time.Second))
	}

	command, err := store.issueCommand(t.Context(), "task-1", protocol.CommandPause, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := readProgress(t, store, "task-1"); !p.LastActivity.Equal(future.Add(time.Second)) {
		t.Errorf("a command issued at %s moved last activity back to %s", command.Time, p.LastActivity)
	}
}

// createVersionOneDatabase creates a database as schema version 1 left
// it, holding one daemon and one pending task.
func createVersionOneDatabase(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL) STRICT`,
		schema,
		`INSERT INTO schema_version (version) VALUES (1)`,
		`INSERT INTO daemons (id, first_seen, last_seen) VALUES ('laptop', '2026-10-07T10:00:00Z', '2026-10-07T10:00:00Z')`,
		`INSERT INTO tasks (id, daemon_id, state, created_at, prompt, system_prompt, model, pause_acknowledge_ns, pause_cleanup_ns)
			VALUES ('old', 'laptop', 'pending', '2026-10-07T10:00:01.5Z', 'p', '', 'haiku', 60000000000, 300000000000)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func TestGivenVersionOneDatabaseWhenOpeningStoreThenItIsMigratedAndItsTasksKeepProgressing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.db")
	createVersionOneDatabase(t, path)

	store, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var version int
	if err := store.db.QueryRowContext(t.Context(), `SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || schemaVersion != 24 {
		t.Errorf("schema version = %d (server knows %d), want 24", version, schemaVersion)
	}
	if old := readTask(t, store, "old"); old.Project != "" || old.Purpose != "" {
		t.Errorf("migrated task project %q, purpose %q; want none", old.Project, old.Purpose)
	}
	if has, err := store.HasOwnerToken(t.Context()); err != nil || has {
		t.Errorf("migrated HasOwnerToken = %v, %v; want false", has, err)
	}
	created := time.Date(2026, 10, 7, 10, 0, 1, 500_000_000, time.UTC)
	if p := readProgress(t, store, "old"); p != (progress{State: TaskPending, LastActivity: created}) {
		t.Errorf("migrated progress = %+v, want pending, last active at creation, no cost", p)
	}

	started := event("old", 1, `{"pid":1}`)
	started.Kind = protocol.KindHarnessStarted
	started.Time = created.Add(time.Minute)
	if _, _, err := store.appendEvents(t.Context(), "laptop", []protocol.Event{started}); err != nil {
		t.Fatal(err)
	}
	if p := readProgress(t, store, "old"); p.State != TaskRunning || !p.LastActivity.Equal(started.Time) {
		t.Errorf("progress after start = %+v", p)
	}
}

func TestGivenMigratedDatabaseWhenComparedWithANewOneThenTheTasksTablesHaveTheSameColumns(t *testing.T) {
	migratedPath := filepath.Join(t.TempDir(), "migrated.db")
	createVersionOneDatabase(t, migratedPath)
	migrated, err := OpenStore(t.Context(), migratedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	fresh, _ := openTestStore(t)

	columns := func(store *Store) []string {
		rows, err := store.db.QueryContext(t.Context(), `SELECT name || ' ' || type || ' ' || "notnull" FROM pragma_table_info('tasks') ORDER BY cid`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var column string
			if err := rows.Scan(&column); err != nil {
				t.Fatal(err)
			}
			out = append(out, column)
		}
		return out
	}
	requireJSONEqual(t, columns(migrated), columns(fresh))
}

func TestGivenEachStateWhenTheSchedulersPauseAppliesThenTheStateFollowsTheTable(t *testing.T) {
	settled := controlEvent(protocol.KindPauseSettled, `{"interrupted":false}`)
	exitedCleanly := controlEvent(protocol.KindHarnessExited, `{"exit_code":0}`)
	started := controlEvent(protocol.KindHarnessStarted, `{"pid":1}`)
	for _, tc := range []struct {
		name  string
		from  TaskState
		event protocol.Event
		want  TaskState
	}{
		{"pause settles", TaskPausing, settled, TaskYielded},
		{"clean exit while yielded", TaskYielded, exitedCleanly, TaskYielded},
		{"next process of a yielded task", TaskYielded, started, TaskRunning},
		{"clean exit before the pause settled", TaskPausing, exitedCleanly, TaskFinished},
	} {
		if got := tc.from.afterEvent(tc.event, false, pauseByScheduler); got != tc.want {
			t.Errorf("%s: %s + %s = %s, want %s", tc.name, tc.from, tc.event.Kind, got, tc.want)
		}
	}
	for _, tc := range []struct {
		kind protocol.CommandKind
		want TaskState
	}{
		{protocol.CommandResume, TaskRunning},
		{protocol.CommandPrompt, TaskRunning},
		{protocol.CommandPause, TaskPaused},
		{protocol.CommandStop, TaskStopped},
		{protocol.CommandInterrupt, TaskYielded},
	} {
		if got := TaskYielded.afterCommand(tc.kind); got != tc.want {
			t.Errorf("yielded + %s command = %s, want %s", tc.kind, got, tc.want)
		}
	}
}

// yield pauses the lifecycle's task for the scheduler.
func (l *lifecycle) yield(want TaskState) {
	l.t.Helper()
	tx, err := l.store.db.BeginTx(l.t.Context(), nil)
	if err != nil {
		l.t.Fatal(err)
	}
	defer tx.Rollback()
	var fx effects
	if _, err := yieldTask(l.t.Context(), tx, "laptop", l.task, &fx); err != nil {
		l.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		l.t.Fatal(err)
	}
	if got := readProgress(l.t, l.store, l.task).State; got != want {
		l.t.Fatalf("after the scheduler's pause: state = %s, want %s", got, want)
	}
}

func TestGivenRunningTaskWhenTheSchedulerPausesItThenItIsYieldedOnceThePauseSettlesAndAResumeRunsIt(t *testing.T) {
	store, _ := openTestStore(t)
	l := newLifecycle(t, store, "task-1")
	l.held = true
	l.event(protocol.KindHarnessStarted, `{"pid":1}`, TaskRunning)

	l.yield(TaskPausing)
	if p := readProgress(t, store, "task-1"); p.PausedBy != pauseByScheduler {
		t.Errorf("paused by %q, want the scheduler", p.PausedBy)
	}
	l.event(protocol.KindPauseAcknowledged, `{"note":"after step 1"}`, TaskPausing)
	l.event(protocol.KindPauseSettled, `{"interrupted":false}`, TaskYielded)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskYielded)
	l.command(protocol.CommandResume, "", TaskRunning)
	if p := readProgress(t, store, "task-1"); p.PausedBy != "" {
		t.Errorf("paused by %q after the resume, want nobody", p.PausedBy)
	}
	l.event(protocol.KindHarnessStarted, `{"pid":2}`, TaskRunning)
	l.event(protocol.KindHarnessExited, `{"exit_code":0}`, TaskFinished)
}

func TestGivenTheSchedulersPauseWhenTheOwnerPausesTooThenTheTaskEndsUpPausedForTheOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drive func(l *lifecycle)
	}{
		{"while pausing", func(l *lifecycle) {
			l.command(protocol.CommandPause, "", TaskPausing)
			l.event(protocol.KindPauseSettled, `{"interrupted":false}`, TaskPaused)
		}},
		{"once yielded", func(l *lifecycle) {
			l.event(protocol.KindPauseSettled, `{"interrupted":false}`, TaskYielded)
			l.command(protocol.CommandPause, "", TaskPaused)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := openTestStore(t)
			l := newLifecycle(t, store, "task-1")
			l.event(protocol.KindHarnessStarted, `{"pid":1}`, TaskRunning)
			l.yield(TaskPausing)
			l.held = true

			tc.drive(l)

			if p := readProgress(t, store, "task-1"); p.PausedBy != pauseByOwner {
				t.Errorf("paused by %q, want the owner", p.PausedBy)
			}
		})
	}
}

func TestGivenTurnCutShortWhenResumingStartsANewSessionThenItsCostAddsToTheEarlierSessionsAndOtherwiseItDoesNot(t *testing.T) {
	for name, tc := range map[string]struct {
		exitError string
		want      float64
	}{
		"new session":       {exitError: exitRestartedNoSession, want: 0.75},
		"session continues": {exitError: exitRestarted, want: 0.5},
	} {
		t.Run(name, func(t *testing.T) {
			store, _ := openTestStore(t)
			seedTask(t, store, "laptop", "task-1")
			exited := `{"exit_code":-1,"error":"` + tc.exitError + `"}`
			events := []protocol.Event{
				taskControlEvent("task-1", 1, protocol.KindHarnessStarted, started),
				event("task-1", 2, `{"type":"result","subtype":"success","total_cost_usd":0.5}`),
				taskControlEvent("task-1", 3, protocol.KindHarnessExited, exited),
				taskControlEvent("task-1", 4, protocol.KindHarnessStarted, started),
				event("task-1", 5, `{"type":"result","subtype":"success","total_cost_usd":0.25}`),
			}
			if _, _, err := store.appendEvents(t.Context(), "laptop", events); err != nil {
				t.Fatal(err)
			}

			if p := readProgress(t, store, "task-1"); p.CostUSD != tc.want {
				t.Errorf("cost = %v, want %v", p.CostUSD, tc.want)
			}
		})
	}
}

func TestGivenCommandsBeforeVersion22WhenMigratedThenTheyStayTheirIdsCarryOnAndADaemonsCommandHasNoTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.db")
	createVersionOneDatabase(t, path)
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	// Command 5 was issued and is gone, so no id up to 5 may be issued
	// again.
	for _, statement := range []string{
		`INSERT INTO commands (id, daemon_id, task_id, kind, time) VALUES (1, 'laptop', 'old', 'pause', '2026-10-07T10:00:02Z')`,
		`INSERT INTO commands (id, daemon_id, task_id, kind, time) VALUES (5, 'laptop', 'old', 'resume', '2026-10-07T10:00:03Z')`,
		`DELETE FROM commands WHERE id = 5`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	db.Close()

	store, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	kept, err := store.commandsAfter(t.Context(), "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].ID != 1 || kept[0].TaskID != "old" || kept[0].Kind != protocol.CommandPause {
		t.Errorf("commands = %+v, want pause 1 for old", kept)
	}
	login, err := store.issueDaemonCommand(t.Context(), "laptop", protocol.CommandLogin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if login.ID != 6 {
		t.Errorf("the next command's id = %d, want 6", login.ID)
	}
	all, err := store.commandsAfter(t.Context(), "laptop", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].TaskID != "" || all[0].Kind != protocol.CommandLogin {
		t.Errorf("commands after 1 = %+v, want the login, with no task", all)
	}
	rows, err := store.db.QueryContext(t.Context(), `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("the migrated database has foreign key violations")
	}
}

// TestGivenCommandsWithChildRowsBeforeVersion22WhenMigratedThenForeignKeysHoldAndNothingIsLost
// reproduces the owner's database, whose migration to version 22 failed
// with "FOREIGN KEY constraint failed": messages and turns referenced
// commands that version 22's rebuild of the commands table dropped and
// recreated, which SQLite counts as a violation with foreign keys on
// (https://www.sqlite.org/lang_altertable.html).
func TestGivenCommandsWithChildRowsBeforeVersion22WhenMigratedThenForeignKeysHoldAndNothingIsLost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.db")
	createVersionOneDatabase(t, path)
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	// migrations[:20] takes the schema from version 1 to version 21, the
	// last version before the commands table is rebuilt.
	for _, statement := range append(migrations[:20:20],
		`INSERT INTO commands (id, daemon_id, task_id, kind, time) VALUES (1, 'laptop', 'old', 'prompt', '2026-10-07T10:00:02Z')`,
		`INSERT INTO commands (id, daemon_id, task_id, kind, time) VALUES (2, 'laptop', 'old', 'prompt', '2026-10-07T10:00:03Z')`,
		`INSERT INTO turns (task_id, kind, origin, filler, created_at, admitted_command_id) VALUES ('old', 'prompt', 'owner', 0, '2026-10-07T10:00:02Z', 1)`,
		`INSERT INTO messages (from_task, to_task, text, created_at, delivered_command_id) VALUES ('old', 'old', 'hi', '2026-10-07T10:00:03Z', 2)`,
		`UPDATE schema_version SET version = 21`) {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	db.Close()

	store, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var notNull int
	if err := store.db.QueryRowContext(t.Context(), `SELECT "notnull" FROM pragma_table_info('commands') WHERE name = 'task_id'`).Scan(&notNull); err != nil {
		t.Fatal(err)
	}
	if notNull != 0 {
		t.Errorf(`commands.task_id "notnull" = %d, want 0 (nullable)`, notNull)
	}

	for _, id := range []int64{1, 2} {
		var taskID string
		if err := store.db.QueryRowContext(t.Context(), `SELECT task_id FROM commands WHERE id = ?`, id).Scan(&taskID); err != nil {
			t.Errorf("command %d: %v", id, err)
		} else if taskID != "old" {
			t.Errorf("command %d task_id = %q, want %q", id, taskID, "old")
		}
	}

	var turnAdmitted int64
	if err := store.db.QueryRowContext(t.Context(), `SELECT admitted_command_id FROM turns WHERE task_id = 'old'`).Scan(&turnAdmitted); err != nil {
		t.Errorf("turn: %v", err)
	} else if turnAdmitted != 1 {
		t.Errorf("turn's admitted_command_id = %d, want 1", turnAdmitted)
	}

	var messageDelivered int64
	if err := store.db.QueryRowContext(t.Context(), `SELECT delivered_command_id FROM messages WHERE to_task = 'old'`).Scan(&messageDelivered); err != nil {
		t.Errorf("message: %v", err)
	} else if messageDelivered != 2 {
		t.Errorf("message's delivered_command_id = %d, want 2", messageDelivered)
	}

	rows, err := store.db.QueryContext(t.Context(), `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Error("the migrated database has foreign key violations")
	}
	rows.Close()

	var seq int64
	if err := store.db.QueryRowContext(t.Context(), `SELECT seq FROM sqlite_sequence WHERE name = 'commands'`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq < 2 {
		t.Errorf("sqlite_sequence for commands = %d, want at least 2", seq)
	}
}

// TestGivenTheOwnersCopiedDatabaseWhenOpenedThenItMigratesWithoutLosingTasks
// guards against the regression this bug was found in: opening a copy of
// the owner's real database, at schema version 15, failed to migrate past
// version 22 with child rows in messages and turns. It is skipped when no
// copy is available to test against.
func TestGivenTheOwnersCopiedDatabaseWhenOpenedThenItMigratesWithoutLosingTasks(t *testing.T) {
	const source = "/tmp/server-copy.db"
	data, err := os.ReadFile(source)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("no owner database copy at " + source)
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "server-copy.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	before, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		t.Fatal(err)
	}
	var wantTasks int
	if err := before.QueryRowContext(t.Context(), `SELECT count(*) FROM tasks`).Scan(&wantTasks); err != nil {
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var gotTasks int
	if err := store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM tasks`).Scan(&gotTasks); err != nil {
		t.Fatal(err)
	}
	if gotTasks != wantTasks {
		t.Errorf("task count after migration = %d, want %d", gotTasks, wantTasks)
	}
}
