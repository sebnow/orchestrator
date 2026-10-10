package server

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
	// TaskStopped: the owner stopped the task. A follow-up starts its next
	// process, as for a finished task; only dismissal ends it for good.
	TaskStopped TaskState = "stopped"
	// TaskFailed: the harness exited otherwise, or never started. A
	// follow-up starts its next process, or, when its harness never
	// started, starts it afresh; only dismissal ends it for good.
	TaskFailed TaskState = "failed"
)

// Ended reports whether the task's work ended without finishing: it was
// stopped or failed. Such a task waits for the owner, who may follow it
// up or dismiss it; agents' messages to it are refused.
func (s TaskState) Ended() bool {
	return s == TaskStopped || s == TaskFailed
}

// Idle reports whether the task has no process: it has not started, or
// it is between processes.
func (s TaskState) Idle() bool {
	return s == TaskQueued || s == TaskFinished || s == TaskPaused || s == TaskYielded || s.Ended()
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
// treats both alike, and resumes a finished, stopped or failed task too.
// Either one issued while a pause is under way applies once the pause
// settles, so the task is running thereafter. A pause to a yielded task
// makes the scheduler's hold the owner's. A stop to a task with no
// process stops it at once, as no process is left to report it.
func (s TaskState) afterCommand(kind protocol.CommandKind) TaskState {
	switch {
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
// whether a stop has been issued to the task's running process; it
// matters only for harness_exited. pausedBy names who asked for the pause
// under way; it matters only for pause_settled.
//
// pause_settled takes effect only while pausing: when a resume or prompt
// was issued before the pause settled, the task resumes right after. A
// clean exit leaves a paused or yielded task so; a pause that had not
// settled when the process exited did not take effect, and the task is
// finished. An exit that says the turn was cut short, by the daemon
// restarting or shutting down or by the owner's interrupt, pauses a task
// with a process for the owner to resume
// (docs/adr/2026-10-08-shutdown-recovery.md), and leaves
// a task between processes as it is. The exit a daemon reports when it
// stops a task with no process leaves the task stopped. A process
// started after any exit makes the task running again.
func (s TaskState) afterEvent(event protocol.Event, stopIssued bool, pausedBy pauseOrigin) TaskState {
	switch {
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
		by, _ := cutShortOf(exit)
		cutShort := decoded && by != ""
		switch {
		case stopIssued, s == TaskStopped:
			return TaskStopped
		case cutShort && s.Idle():
			// The turn ended before the daemon restarted; only the
			// process's exit was lost.
			return s
		case cutShort:
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

// The errors a daemon gives in harness_exited when it cut the task's turn
// short and can resume the task, continuing the recorded harness session
// or, when none was recorded, starting a new one: by restarting after it
// died during the turn, or by shutting down during it
// (docs/adr/2026-10-08-shutdown-recovery.md). internal/daemon words them
// the same.
const (
	exitRestarted          = "daemon restarted during the turn"
	exitRestartedNoSession = "daemon restarted during the turn, before the harness reported a session"
	exitStopped            = "daemon stopped during the turn"
	exitStoppedNoSession   = "daemon stopped during the turn, before the harness reported a session"
)

// The errors a daemon gives in harness_exited when the owner's interrupt
// cut the task's turn short and the task can be resumed, continuing its
// harness session or starting a new one
// (docs/design/2026-10-08-interrupt-findings.md). internal/daemon words
// them the same.
const (
	exitInterrupted          = "interrupted by the owner"
	exitInterruptedNoSession = "interrupted by the owner, before the harness reported a session"
)

// The beginnings of the errors a daemon gives in harness_exited, with
// ExitCode -1, for a process that never started: its workspace could not
// be prepared, or its harness could not be started. The cause follows.
// internal/daemon words them the same.
const (
	startFailedWorkspace = "workspace could not be prepared: "
	startFailedHarness   = "harness could not be started: "
)

// startFailed reports whether exit says the process never started.
func startFailed(exit protocol.HarnessExited) bool {
	return exit.ExitCode == -1 && (strings.HasPrefix(exit.Error, startFailedWorkspace) || strings.HasPrefix(exit.Error, startFailedHarness))
}

// failureOf words why a process that ended with exit failed: what never
// started and why, for a process that never started, or else the
// harness's exit code and error.
func failureOf(exit protocol.HarnessExited) string {
	switch {
	case startFailed(exit):
		return exit.Error
	case exit.Error != "":
		return "harness exited with code " + strconv.Itoa(exit.ExitCode) + ": " + exit.Error
	}
	return "harness exited with code " + strconv.Itoa(exit.ExitCode)
}

// cutShortOf reports whether exit is a turn cut short and left resumable,
// by the daemon or by the owner's interrupt: by is "restarted", "stopped"
// or "interrupted" when it is, and empty otherwise. newSession says whether resuming starts a new harness
// session.
func cutShortOf(exit protocol.HarnessExited) (by string, newSession bool) {
	if exit.ExitCode != -1 {
		return "", false
	}
	switch exit.Error {
	case exitRestarted:
		return "restarted", false
	case exitRestartedNoSession:
		return "restarted", true
	case exitStopped:
		return "stopped", false
	case exitStoppedNoSession:
		return "stopped", true
	case exitInterrupted:
		return "interrupted", false
	case exitInterruptedNoSession:
		return "interrupted", true
	}
	return "", false
}

// progress is what the server keeps per task as events are stored and
// commands issued, so that listing tasks needs no transcript.
type progress struct {
	State        TaskState
	LastActivity time.Time
	// CostUSD is what the task has cost over all its harness sessions:
	// CostBase plus the highest running total the current session has
	// reported.
	CostUSD float64
	// CostBase is the sum of the final totals of the task's earlier
	// harness sessions. A harness's running total covers one session and
	// starts again from zero in the next, so CostBase is set to CostUSD
	// whenever the task's next turn starts a new session: when the task
	// moves off a lost daemon (docs/adr/2026-10-08-daemon-loss.md), and
	// when a turn cut short before the harness reported a session leaves
	// its resume to start one.
	CostBase float64
	// PausedBy names who asked for the pause while the task is pausing,
	// paused or yielded; it is empty otherwise.
	PausedBy pauseOrigin
	// StopPending says a stop was issued to the task's process and its
	// exit has not been stored yet; that exit leaves the task stopped.
	StopPending bool
}

// seeEvent folds a stored event into p. A task an event leaves paused
// waits for the owner, even when the scheduler had asked for the pause
// and a daemon restart cut it short.
func (p *progress) seeEvent(event protocol.Event) {
	p.State = p.State.afterEvent(event, p.StopPending, p.PausedBy)
	if event.Kind == protocol.KindHarnessExited {
		p.StopPending = false
	}
	if p.State == TaskPaused {
		p.PausedBy = pauseByOwner
	}
	p.forgetPause()
	p.see(event.Time)
	if event.Kind == protocol.KindHarnessExited {
		var exit protocol.HarnessExited
		if json.Unmarshal(event.Payload, &exit) == nil {
			if _, newSession := cutShortOf(exit); newSession {
				p.newSession()
			}
		}
		return
	}
	if event.Kind != protocol.KindHarnessOutput {
		return
	}
	normalise, ok := normalisers[event.Harness.Name]
	if !ok {
		return
	}
	for _, body := range normalise(event.Payload) {
		if turn, ok := body.(transcript.TurnEnded); ok && p.CostBase+turn.TotalCostUSD > p.CostUSD {
			p.CostUSD = p.CostBase + turn.TotalCostUSD
		}
	}
}

// newSession records that the task's next turn starts a new harness
// session, whose running total starts from zero.
func (p *progress) newSession() {
	p.CostBase = p.CostUSD
}

// seeCommand folds an issued command into p. A pause the command puts
// the task under is the owner's; the scheduler marks its own. A stop to a
// task with a process leaves the stop pending until the process exits. A
// prompt that steers a task awaiting permission interrupts the turn that
// asked, so the task runs on with nothing to answer.
func (p *progress) seeCommand(command protocol.Command) {
	if command.Kind == protocol.CommandStop && !p.State.Idle() {
		p.StopPending = true
	}
	p.State = p.State.afterCommand(command.Kind)
	if command.Kind == protocol.CommandPrompt && p.State == TaskAwaitingPermission {
		var prompt protocol.Prompt
		if json.Unmarshal(command.Payload, &prompt) == nil && prompt.Steer {
			p.State = TaskRunning
		}
	}
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
	err := tx.QueryRowContext(ctx, `SELECT state, last_activity_at, cost_usd, cost_base, pause_origin, stop_pending FROM tasks WHERE id = ?`, string(task)).
		Scan(&state, &lastActivity, &p.CostUSD, &p.CostBase, &pausedBy, &p.StopPending)
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
	_, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, last_activity_at = ?, cost_usd = ?, cost_base = ?, pause_origin = ?, stop_pending = ? WHERE id = ?`,
		string(p.State), formatTime(p.LastActivity), p.CostUSD, p.CostBase, pausedBy, p.StopPending, string(task))
	if err != nil {
		return fmt.Errorf("record progress of task %q: %w", task, err)
	}
	return nil
}

// taskSummary is a task as the task list shows it.
type taskSummary struct {
	ID protocol.TaskID `json:"id"`
	// DaemonID is empty while the task waits to be placed on any daemon,
	// or on its parent's unless that is full.
	DaemonID protocol.DaemonID `json:"daemon_id"`
	// ParentID names the task that spawned this one; nil for the owner's.
	ParentID *protocol.TaskID `json:"parent_id,omitempty"`
	// Agent names the agent the task was started as; empty for none.
	Agent string `json:"agent,omitempty"`
	// Project is the id of the project the task belongs to; empty for
	// none (docs/adr/2026-10-10-projects-and-lineage.md).
	Project string `json:"project,omitempty"`
	// Purpose says why the task exists, as its spawner or the owner gave
	// it; empty for none.
	Purpose string `json:"purpose,omitempty"`
	// Requires are the labels the task's daemon must have.
	Requires       Labels    `json:"requires,omitempty"`
	State          TaskState `json:"state"`
	Model          string    `json:"model"`
	Priority       Priority  `json:"priority"`
	Filler         bool      `json:"filler"`
	CreatedAt      time.Time `json:"created_at"`
	LastActivityAt time.Time `json:"last_activity_at"`
	CostUSD        float64   `json:"cost_usd"`
	// Queue is where the task's first waiting turn stands; nil when none
	// waits for the scheduler.
	Queue *queuePlace `json:"queue,omitempty"`
	// DismissedAt is when the owner dismissed the ended task from the
	// dashboard's lists; nil while it is not dismissed.
	DismissedAt *time.Time `json:"dismissed_at,omitempty"`
	// Branch is the latest branch_pushed the task's daemon reported: where
	// the task's work was delivered (docs/adr/2026-10-08-work-delivery.md).
	// nil until a daemon reports one.
	Branch *protocol.BranchPushed `json:"branch,omitempty"`
}

// taskDetail is one task: its summary and what it was started with.
// HasSession says its latest start got as far as starting its harness, so
// that a follow-up continues its session; a stopped or failed task
// without one is started afresh instead. Failure says why a failed task
// failed, as failureOf words its latest exit; it is empty otherwise.
// Queued are the owner's prompts that have not reached the harness yet.
type taskDetail struct {
	taskSummary
	Start      protocol.StartTask `json:"start"`
	HasSession bool               `json:"has_session"`
	Failure    string             `json:"failure,omitempty"`
	Queued     []queuedPrompt     `json:"queued"`
}

const summaryColumns = `id, daemon_id, placement, parent_id, coalesce(agent, ''), coalesce(project, ''), purpose, requires, state, model, priority, filler, created_at, last_activity_at, cost_usd, dismissed_at`

// scanSummary reads summaryColumns, followed by extra destinations.
func scanSummary(row interface{ Scan(...any) error }, extra ...any) (taskSummary, error) {
	var summary taskSummary
	var id, daemon, placed, requires, state, priority, created, lastActivity string
	var parent, dismissed sql.NullString
	dest := append([]any{&id, &daemon, &placed, &parent, &summary.Agent, &summary.Project, &summary.Purpose, &requires, &state, &summary.Model, &priority, &summary.Filler, &created, &lastActivity, &summary.CostUSD, &dismissed}, extra...)
	if err := row.Scan(dest...); err != nil {
		return taskSummary{}, err
	}
	summary.ID, summary.State, summary.Priority = protocol.TaskID(id), TaskState(state), Priority(priority)
	var err error
	if summary.Requires, err = decodeLabels(requires); err != nil {
		return taskSummary{}, fmt.Errorf("task %q requires: %w", id, err)
	}
	if placement(placed) == placementBound {
		summary.DaemonID = protocol.DaemonID(daemon)
	}
	if parent.Valid {
		parentID := protocol.TaskID(parent.String)
		summary.ParentID = &parentID
	}
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
	branches, err := latestBranches(ctx, tx, "")
	if err != nil {
		return nil, err
	}
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
		summary.Branch = branches[summary.ID]
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
	return readTaskDetail(ctx, tx, task)
}

// readTaskDetail reads one task in tx, or returns errUnknownTask.
func readTaskDetail(ctx context.Context, tx *sql.Tx, task protocol.TaskID) (taskDetail, error) {
	var detail taskDetail
	var repo, ref, tools sql.NullString
	var acknowledge, cleanup int64
	row := tx.QueryRowContext(ctx, `
		SELECT `+summaryColumns+`, prompt, system_prompt, workspace_repo, workspace_ref, pause_acknowledge_ns, pause_cleanup_ns, tools
		FROM tasks WHERE id = ?`, string(task))
	summary, err := scanSummary(row, &detail.Start.Prompt, &detail.Start.SystemPrompt, &repo, &ref, &acknowledge, &cleanup, &tools)
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
	branches, err := latestBranches(ctx, tx, task)
	if err != nil {
		return taskDetail{}, err
	}
	summary.Branch = branches[task]
	summary.placeInQueue(places)
	detail.taskSummary = summary
	detail.Start.Model = summary.Model
	if repo.Valid {
		detail.Start.Workspace = &protocol.Workspace{Repo: repo.String, Ref: ref.String}
	}
	detail.Start.PauseLimits = protocol.PauseLimits{Acknowledge: time.Duration(acknowledge), Cleanup: time.Duration(cleanup)}
	if tools.Valid {
		if err := json.Unmarshal([]byte(tools.String), &detail.Start.Tools); err != nil {
			return taskDetail{}, fmt.Errorf("read tools of task %q: %w", task, err)
		}
	}
	if detail.HasSession, err = hasSession(ctx, tx, task); err != nil {
		return taskDetail{}, err
	}
	if detail.Queued, err = queuedPrompts(ctx, tx, task); err != nil {
		return taskDetail{}, err
	}
	if detail.State == TaskFailed {
		var payload string
		err := tx.QueryRowContext(ctx, `SELECT payload FROM events WHERE task_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
			string(task), string(protocol.KindHarnessExited)).Scan(&payload)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return taskDetail{}, fmt.Errorf("read the exit of task %q: %w", task, err)
		}
		var exit protocol.HarnessExited
		if json.Unmarshal([]byte(payload), &exit) == nil {
			detail.Failure = failureOf(exit)
		}
	}
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

// childSummary is a task's child as its parent's page lists it. Report
// is the latest message the child sent its parent, its report, handed
// back or sent; empty when it has sent none. Branch is the latest
// branch_pushed the child's daemon reported; nil when none.
type childSummary struct {
	ID      protocol.TaskID
	Purpose string
	Agent   string
	State   TaskState
	Report  string
	Branch  *protocol.BranchPushed
}

// children returns the tasks task spawned, oldest first. Stored times do
// not sort as text; julianday compares the instants, to the millisecond,
// and the row id, which follows insertion, breaks ties.
func (s *Store) children(ctx context.Context, task protocol.TaskID) ([]childSummary, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("read children of task %q: %w", task, err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id, t.purpose, coalesce(t.agent, ''), t.state,
			coalesce((SELECT m.text FROM messages m WHERE m.from_task = t.id AND m.to_task = ?1 ORDER BY m.id DESC LIMIT 1), '')
		FROM tasks t WHERE t.parent_id = ?1 ORDER BY julianday(t.created_at), t.rowid`, string(task))
	if err != nil {
		return nil, fmt.Errorf("read children of task %q: %w", task, err)
	}
	defer rows.Close()
	var children []childSummary
	for rows.Next() {
		var child childSummary
		var id, state string
		if err := rows.Scan(&id, &child.Purpose, &child.Agent, &state, &child.Report); err != nil {
			return nil, fmt.Errorf("read children of task %q: %w", task, err)
		}
		child.ID, child.State = protocol.TaskID(id), TaskState(state)
		children = append(children, child)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read children of task %q: %w", task, err)
	}
	rows.Close()
	for idx := range children {
		branches, err := latestBranches(ctx, tx, children[idx].ID)
		if err != nil {
			return nil, err
		}
		children[idx].Branch = branches[children[idx].ID]
	}
	return children, nil
}

// dismissTask records that the owner dismissed task, which must be
// stopped or failed, from the dashboard's lists, and returns the task. A
// task dismissed already keeps the time of its first dismissal. A
// dismissed task takes no more follow-ups, so its waiting turns are
// dropped, and the daemon its latest start went to is told to discard
// what it keeps of the task.
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
	if !TaskState(state).Ended() {
		return taskDetail{}, fmt.Errorf("%w: %q is %s", errNotEnded, task, state)
	}
	fx := effects{changed: []protocol.TaskID{task}}
	var daemon sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT t.daemon_id FROM tasks t
		WHERE t.id = ?1 AND t.dismissed_at IS NULL
			AND EXISTS (SELECT 1 FROM commands c WHERE c.task_id = t.id AND c.daemon_id = t.daemon_id AND c.kind = ?2)`,
		string(task), string(protocol.CommandStartTask)).Scan(&daemon)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return taskDetail{}, fmt.Errorf("dismiss task %q: %w", task, err)
	}
	if daemon.Valid {
		if _, err := insertCommand(ctx, tx, protocol.DaemonID(daemon.String), task, protocol.CommandDiscard, nil, &fx); err != nil {
			return taskDetail{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM turns WHERE task_id = ? AND admitted_command_id IS NULL`, string(task)); err != nil {
		return taskDetail{}, fmt.Errorf("dismiss task %q: %w", task, err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET dismissed_at = coalesce(dismissed_at, ?) WHERE id = ?`,
		formatTime(time.Now().UTC()), string(task))
	if err != nil {
		return taskDetail{}, fmt.Errorf("dismiss task %q: %w", task, err)
	}
	if err := tx.Commit(); err != nil {
		return taskDetail{}, fmt.Errorf("dismiss task %q: %w", task, err)
	}
	s.publish(&fx)
	return s.task(ctx, task)
}

// latestBranches returns the latest branch_pushed of task, or of every
// task when task is empty, by task. An event whose payload does not
// decode is passed over.
func latestBranches(ctx context.Context, tx *sql.Tx, task protocol.TaskID) (map[protocol.TaskID]*protocol.BranchPushed, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT e.task_id, e.payload FROM events e
		WHERE e.kind = ?1 AND (?2 = '' OR e.task_id = ?2)
			AND e.seq = (SELECT max(seq) FROM events WHERE task_id = e.task_id AND kind = ?1)`,
		string(protocol.KindBranchPushed), string(task))
	if err != nil {
		return nil, fmt.Errorf("read pushed branches: %w", err)
	}
	defer rows.Close()
	branches := make(map[protocol.TaskID]*protocol.BranchPushed)
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, fmt.Errorf("read pushed branches: %w", err)
		}
		var pushed protocol.BranchPushed
		if json.Unmarshal([]byte(payload), &pushed) == nil {
			branches[protocol.TaskID(id)] = &pushed
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pushed branches: %w", err)
	}
	return branches, nil
}
