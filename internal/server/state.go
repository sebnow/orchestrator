package server

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// TaskState is where a task is in its life, as the server last learned
// from the task's events and the commands issued to it.
type TaskState string

const (
	// TaskQueued: the task's start waits for the scheduler to admit it, so
	// no daemon knows the task yet (docs/adr/2026-10-08-scheduling.md).
	TaskQueued TaskState = "queued"
	// TaskPending: the start was issued; the harness has not started.
	TaskPending TaskState = "pending"
	TaskRunning TaskState = "running"
	// TaskAwaitingPermission: the harness asked to run a tool and waits
	// for the answer.
	TaskAwaitingPermission TaskState = "awaiting_permission"
	// TaskPausing: a pause was issued and has not taken effect yet.
	TaskPausing TaskState = "pausing"
	// TaskPaused: the owner's pause took effect and the task's process
	// exited, or a daemon restart cut its turn short; a resume or a
	// prompt starts its next process.
	TaskPaused TaskState = "paused"
	// TaskYielded: the scheduler's pause took effect and the task's
	// process exited. The scheduler resumes it when a slot is free
	// (docs/adr/2026-10-08-scheduling.md).
	TaskYielded TaskState = "yielded"
	// TaskFinished: the task's process exited with code 0 after its turn,
	// with no stop issued and no pause in effect. A prompt starts its next
	// process (docs/adr/2026-10-08-task-lifetime.md).
	TaskFinished TaskState = "finished"
	// TaskStopped: the owner stopped the task. It is terminal.
	TaskStopped TaskState = "stopped"
	// TaskFailed: the harness exited otherwise, or never started. It is
	// terminal.
	TaskFailed TaskState = "failed"
)

// Terminal reports whether the task can take no more commands.
func (s TaskState) Terminal() bool {
	return s == TaskStopped || s == TaskFailed
}

// Idle reports whether the task has no process: it has not started, or
// it is between processes.
func (s TaskState) Idle() bool {
	return s == TaskQueued || s == TaskFinished || s == TaskPaused || s == TaskYielded
}

// pauseOrigin says who asked for the pause a task is under: the owner,
// whose pause holds the task until the owner resumes it, or the
// scheduler, which yields a filler task's slot and resumes it itself.
type pauseOrigin string

const (
	pauseByOwner     pauseOrigin = "owner"
	pauseByScheduler pauseOrigin = "scheduler"
)

// afterCommand returns the state once a command of kind is issued.
//
// Admitting a queued task's start makes it pending. A prompt or a
// resume to a task between processes starts its next process: the daemon
// treats both alike, and resumes a finished task too. Either one issued
// while a pause is under way applies once the pause settles, so the task
// is running thereafter. A pause to a yielded task makes the scheduler's
// hold the owner's. A stop to a task with no process ends it at once, as
// no process is left to report it.
func (s TaskState) afterCommand(kind protocol.CommandKind) TaskState {
	switch {
	case s.Terminal():
		return s
	case kind == protocol.CommandStop && s.Idle():
		return TaskStopped
	case kind == protocol.CommandStartTask && s == TaskQueued:
		return TaskPending
	case kind == protocol.CommandPause && (s == TaskPending || s == TaskRunning || s == TaskAwaitingPermission):
		return TaskPausing
	case kind == protocol.CommandPause && s == TaskYielded:
		return TaskPaused
	case (kind == protocol.CommandResume || kind == protocol.CommandPrompt) && (s == TaskPausing || s.Idle() && s != TaskQueued):
		return TaskRunning
	case kind == protocol.CommandAnswerPermission && s == TaskAwaitingPermission:
		return TaskRunning
	}
	return s
}

// afterEvent returns the state once event is stored. stopIssued says
// whether a stop has been issued for the task; it matters only for
// harness_exited. pausedBy names who asked for the pause under way; it
// matters only for pause_settled.
//
// pause_settled takes effect only while pausing: when a resume or prompt
// was issued before the pause settled, the task resumes right after. A
// clean exit leaves a paused or yielded task so; a pause that had not
// settled when the process exited did not take effect, and the task is
// finished. An exit that says a daemon restart cut the turn short pauses
// a task with a process for the owner to resume
// (docs/adr/2026-10-08-restart-recovery.md), and leaves a task between
// processes as it is. A process started after a clean exit makes the task
// running again.
func (s TaskState) afterEvent(event protocol.Event, stopIssued bool, pausedBy pauseOrigin) TaskState {
	switch {
	case s.Terminal():
		return s
	case event.Kind == protocol.KindHarnessStarted && (s == TaskPending || s.Idle()):
		return TaskRunning
	case event.Kind == protocol.KindPermissionRequested && s == TaskRunning:
		return TaskAwaitingPermission
	case event.Kind == protocol.KindPauseSettled && s == TaskPausing && pausedBy == pauseByScheduler:
		return TaskYielded
	case event.Kind == protocol.KindPauseSettled && s == TaskPausing:
		return TaskPaused
	case event.Kind == protocol.KindHarnessExited:
		var exit protocol.HarnessExited
		decoded := json.Unmarshal(event.Payload, &exit) == nil
		restarted, _ := restartOf(exit)
		switch {
		case stopIssued:
			return TaskStopped
		case decoded && restarted && s.Idle():
			// The turn ended before the daemon restarted; only the
			// process's exit was lost.
			return s
		case decoded && restarted:
			return TaskPaused
		case !decoded || exit.ExitCode != 0 || exit.Error != "":
			return TaskFailed
		case s == TaskPaused || s == TaskYielded:
			return s
		default:
			return TaskFinished
		}
	}
	return s
}

// The errors a daemon gives in harness_exited when its restart cut the
// task's turn short and it can resume the task, continuing the recorded
// harness session or, when none was recorded, starting a new one
// (docs/adr/2026-10-08-restart-recovery.md). internal/daemon words them
// the same.
const (
	exitRestarted          = "daemon restarted during the turn"
	exitRestartedNoSession = "daemon restarted during the turn, before the harness reported a session"
)

// restartOf reports whether exit is a turn cut short by a daemon restart
// that left the task resumable, and whether resuming it starts a new
// harness session.
func restartOf(exit protocol.HarnessExited) (restarted, newSession bool) {
	if exit.ExitCode != -1 {
		return false, false
	}
	switch exit.Error {
	case exitRestarted:
		return true, false
	case exitRestartedNoSession:
		return true, true
	}
	return false, false
}

// progress is what the server keeps per task as events are stored and
// commands issued, so that listing tasks needs no transcript.
type progress struct {
	State        TaskState
	LastActivity time.Time
	// CostUSD is the highest running total the harness has reported.
	CostUSD float64
	// PausedBy names who asked for the pause while the task is pausing,
	// paused or yielded; it is empty otherwise.
	PausedBy pauseOrigin
}

// seeEvent folds a stored event into p. A task an event leaves paused
// waits for the owner, even when the scheduler had asked for the pause
// and a daemon restart cut it short.
func (p *progress) seeEvent(event protocol.Event, stopIssued bool) {
	p.State = p.State.afterEvent(event, stopIssued, p.PausedBy)
	if p.State == TaskPaused {
		p.PausedBy = pauseByOwner
	}
	p.forgetPause()
	p.see(event.Time)
	if event.Kind != protocol.KindHarnessOutput {
		return
	}
	normalise, ok := normalisers[event.Harness.Name]
	if !ok {
		return
	}
	for _, body := range normalise(event.Payload) {
		if turn, ok := body.(transcript.TurnEnded); ok && turn.TotalCostUSD > p.CostUSD {
			p.CostUSD = turn.TotalCostUSD
		}
	}
}

// seeCommand folds an issued command into p. A pause the command puts
// the task under is the owner's; the scheduler marks its own.
func (p *progress) seeCommand(command protocol.Command) {
	p.State = p.State.afterCommand(command.Kind)
	if command.Kind == protocol.CommandPause && (p.State == TaskPausing || p.State == TaskPaused) {
		p.PausedBy = pauseByOwner
	}
	p.forgetPause()
	p.see(command.Time)
}

// forgetPause clears PausedBy once the task is under no pause.
func (p *progress) forgetPause() {
	if p.State != TaskPausing && p.State != TaskPaused && p.State != TaskYielded {
		p.PausedBy = ""
	}
}

func (p *progress) see(at time.Time) {
	if at.After(p.LastActivity) {
		p.LastActivity = at.UTC()
	}
}

func loadProgress(ctx context.Context, tx *sql.Tx, task protocol.TaskID) (progress, error) {
	var state, lastActivity string
	var pausedBy sql.NullString
	var p progress
	err := tx.QueryRowContext(ctx, `SELECT state, last_activity_at, cost_usd, pause_origin FROM tasks WHERE id = ?`, string(task)).
		Scan(&state, &lastActivity, &p.CostUSD, &pausedBy)
	if err != nil {
		return progress{}, fmt.Errorf("read progress of task %q: %w", task, err)
	}
	p.State, p.PausedBy = TaskState(state), pauseOrigin(pausedBy.String)
	if p.LastActivity, err = parseTime(lastActivity); err != nil {
		return progress{}, fmt.Errorf("read progress of task %q: %w", task, err)
	}
	return p, nil
}

func saveProgress(ctx context.Context, tx *sql.Tx, task protocol.TaskID, p progress) error {
	var pausedBy any
	if p.PausedBy != "" {
		pausedBy = string(p.PausedBy)
	}
	_, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, last_activity_at = ?, cost_usd = ?, pause_origin = ? WHERE id = ?`,
		string(p.State), formatTime(p.LastActivity), p.CostUSD, pausedBy, string(task))
	if err != nil {
		return fmt.Errorf("record progress of task %q: %w", task, err)
	}
	return nil
}

// stopIssued reports whether a stop has been issued for task.
func stopIssued(ctx context.Context, tx *sql.Tx, task protocol.TaskID) (bool, error) {
	var issued bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM commands WHERE task_id = ? AND kind = ?)`,
		string(task), string(protocol.CommandStop)).Scan(&issued)
	if err != nil {
		return false, fmt.Errorf("look up stop of task %q: %w", task, err)
	}
	return issued, nil
}

// taskSummary is a task as the task list shows it.
type taskSummary struct {
	ID protocol.TaskID `json:"id"`
	// DaemonID is empty while the task waits to be placed on any daemon,
	// or on its parent's unless that is full.
	DaemonID protocol.DaemonID `json:"daemon_id"`
	// ParentID names the task that spawned this one; nil for the owner's.
	ParentID       *protocol.TaskID `json:"parent_id,omitempty"`
	State          TaskState        `json:"state"`
	Model          string           `json:"model"`
	Priority       Priority         `json:"priority"`
	Filler         bool             `json:"filler"`
	CreatedAt      time.Time        `json:"created_at"`
	LastActivityAt time.Time        `json:"last_activity_at"`
	CostUSD        float64          `json:"cost_usd"`
	// Queue is where the task's first waiting turn stands; nil when none
	// waits for the scheduler.
	Queue *queuePlace `json:"queue,omitempty"`
	// DismissedAt is when the owner dismissed the ended task from the
	// dashboard's lists; nil while it is not dismissed.
	DismissedAt *time.Time `json:"dismissed_at,omitempty"`
}

// taskDetail is one task: its summary and what it was started with.
type taskDetail struct {
	taskSummary
	Start protocol.StartTask `json:"start"`
}

const summaryColumns = `id, daemon_id, placement, parent_id, state, model, priority, filler, created_at, last_activity_at, cost_usd, dismissed_at`

// scanSummary reads summaryColumns, followed by extra destinations.
func scanSummary(row interface{ Scan(...any) error }, extra ...any) (taskSummary, error) {
	var summary taskSummary
	var id, daemon, placed, state, priority, created, lastActivity string
	var parent, dismissed sql.NullString
	dest := append([]any{&id, &daemon, &placed, &parent, &state, &summary.Model, &priority, &summary.Filler, &created, &lastActivity, &summary.CostUSD, &dismissed}, extra...)
	if err := row.Scan(dest...); err != nil {
		return taskSummary{}, err
	}
	summary.ID, summary.State, summary.Priority = protocol.TaskID(id), TaskState(state), Priority(priority)
	if placement(placed) == placementBound {
		summary.DaemonID = protocol.DaemonID(daemon)
	}
	if parent.Valid {
		parentID := protocol.TaskID(parent.String)
		summary.ParentID = &parentID
	}
	var err error
	if summary.CreatedAt, err = parseTime(created); err != nil {
		return taskSummary{}, fmt.Errorf("task %q created_at: %w", id, err)
	}
	if summary.LastActivityAt, err = parseTime(lastActivity); err != nil {
		return taskSummary{}, fmt.Errorf("task %q last_activity_at: %w", id, err)
	}
	if dismissed.Valid {
		at, err := parseTime(dismissed.String)
		if err != nil {
			return taskSummary{}, fmt.Errorf("task %q dismissed_at: %w", id, err)
		}
		summary.DismissedAt = &at
	}
	return summary, nil
}

// placeInQueue sets summary's place in the queue from places.
func (summary *taskSummary) placeInQueue(places map[protocol.TaskID]queuePlace) {
	if place, ok := places[summary.ID]; ok {
		summary.Queue = &place
	}
}

// tasks returns every task's summary, oldest first.
func (s *Store) tasks(ctx context.Context) ([]taskSummary, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	defer tx.Rollback()
	places, err := queuePlaces(ctx, tx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+summaryColumns+` FROM tasks`)
	if err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	defer rows.Close()
	summaries := []taskSummary{}
	for rows.Next() {
		summary, err := scanSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("read tasks: %w", err)
		}
		summary.placeInQueue(places)
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	// Stored times trim trailing zeros, so they do not sort as text.
	slices.SortFunc(summaries, func(a, b taskSummary) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return summaries, nil
}

// task returns one task, or errUnknownTask.
func (s *Store) task(ctx context.Context, task protocol.TaskID) (taskDetail, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return taskDetail{}, fmt.Errorf("read task %q: %w", task, err)
	}
	defer tx.Rollback()
	var detail taskDetail
	var repo, ref sql.NullString
	var acknowledge, cleanup int64
	row := tx.QueryRowContext(ctx, `
		SELECT `+summaryColumns+`, prompt, system_prompt, workspace_repo, workspace_ref, pause_acknowledge_ns, pause_cleanup_ns
		FROM tasks WHERE id = ?`, string(task))
	summary, err := scanSummary(row, &detail.Start.Prompt, &detail.Start.SystemPrompt, &repo, &ref, &acknowledge, &cleanup)
	if errors.Is(err, sql.ErrNoRows) {
		return taskDetail{}, fmt.Errorf("%w: %q", errUnknownTask, task)
	}
	if err != nil {
		return taskDetail{}, fmt.Errorf("read task %q: %w", task, err)
	}
	places, err := queuePlaces(ctx, tx)
	if err != nil {
		return taskDetail{}, err
	}
	summary.placeInQueue(places)
	detail.taskSummary = summary
	detail.Start.Model = summary.Model
	if repo.Valid {
		detail.Start.Workspace = &protocol.Workspace{Repo: repo.String, Ref: ref.String}
	}
	detail.Start.PauseLimits = protocol.PauseLimits{Acknowledge: time.Duration(acknowledge), Cleanup: time.Duration(cleanup)}
	return detail, nil
}

// taskPrompts returns every task's prompt by id, for listing tasks by
// what they were asked.
func (s *Store) taskPrompts(ctx context.Context) (map[protocol.TaskID]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, prompt FROM tasks`)
	if err != nil {
		return nil, fmt.Errorf("read task prompts: %w", err)
	}
	defer rows.Close()
	prompts := make(map[protocol.TaskID]string)
	for rows.Next() {
		var id, prompt string
		if err := rows.Scan(&id, &prompt); err != nil {
			return nil, fmt.Errorf("read task prompts: %w", err)
		}
		prompts[protocol.TaskID(id)] = prompt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read task prompts: %w", err)
	}
	return prompts, nil
}

// children returns the ids of the tasks task spawned, oldest first.
// Stored times do not sort as text; julianday compares the instants, to
// the millisecond, and the row id, which follows insertion, breaks ties.
func (s *Store) children(ctx context.Context, task protocol.TaskID) ([]protocol.TaskID, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM tasks WHERE parent_id = ? ORDER BY julianday(created_at), rowid`, string(task))
	if err != nil {
		return nil, fmt.Errorf("read children of task %q: %w", task, err)
	}
	defer rows.Close()
	var children []protocol.TaskID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read children of task %q: %w", task, err)
		}
		children = append(children, protocol.TaskID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read children of task %q: %w", task, err)
	}
	return children, nil
}

// dismissTask records that the owner dismissed task, which must be
// stopped or failed, from the dashboard's lists, and returns the task. A
// task dismissed already keeps the time of its first dismissal.
func (s *Store) dismissTask(ctx context.Context, task protocol.TaskID) (taskDetail, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return taskDetail{}, fmt.Errorf("dismiss task %q: %w", task, err)
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, string(task)).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return taskDetail{}, fmt.Errorf("%w: %q", errUnknownTask, task)
	}
	if err != nil {
		return taskDetail{}, fmt.Errorf("dismiss task %q: %w", task, err)
	}
	if !TaskState(state).Terminal() {
		return taskDetail{}, fmt.Errorf("%w: %q is %s", errNotEnded, task, state)
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET dismissed_at = coalesce(dismissed_at, ?) WHERE id = ?`,
		formatTime(time.Now().UTC()), string(task))
	if err != nil {
		return taskDetail{}, fmt.Errorf("dismiss task %q: %w", task, err)
	}
	if err := tx.Commit(); err != nil {
		return taskDetail{}, fmt.Errorf("dismiss task %q: %w", task, err)
	}
	s.publish(&effects{changed: []protocol.TaskID{task}})
	return s.task(ctx, task)
}
