package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// TaskState is where a task is in its life, as the server last learned
// from the task's events and the commands issued to it.
type TaskState string

const (
	// TaskPending: the start was issued; the harness has not started.
	TaskPending TaskState = "pending"
	TaskRunning TaskState = "running"
	// TaskAwaitingPermission: the harness asked to run a tool and waits
	// for the answer.
	TaskAwaitingPermission TaskState = "awaiting_permission"
	// TaskPausing: a pause was issued and has not taken effect yet.
	TaskPausing TaskState = "pausing"
	TaskPaused  TaskState = "paused"
	// TaskFinished: the harness exited with code 0 and no stop was issued.
	TaskFinished TaskState = "finished"
	// TaskStopped: the harness exited after the owner issued a stop.
	TaskStopped TaskState = "stopped"
	// TaskFailed: the harness exited otherwise, or never started.
	TaskFailed TaskState = "failed"
)

// Ended reports whether the task's harness has exited. An ended task
// stays in its state.
func (s TaskState) Ended() bool {
	return s == TaskFinished || s == TaskStopped || s == TaskFailed
}

// afterCommand returns the state once a command of kind is issued.
//
// A prompt resumes a paused task as a resume does, because the daemon
// treats both alike, and either one issued while a pause is under way
// applies once the pause settles, so the task is running thereafter.
func (s TaskState) afterCommand(kind protocol.CommandKind) TaskState {
	switch {
	case s.Ended():
		return s
	case kind == protocol.CommandPause && (s == TaskPending || s == TaskRunning || s == TaskAwaitingPermission):
		return TaskPausing
	case (kind == protocol.CommandResume || kind == protocol.CommandPrompt) && (s == TaskPausing || s == TaskPaused):
		return TaskRunning
	case kind == protocol.CommandAnswerPermission && s == TaskAwaitingPermission:
		return TaskRunning
	}
	return s
}

// afterEvent returns the state once event is stored. stopIssued says
// whether a stop has been issued for the task; it matters only for
// harness_exited.
//
// pause_settled takes effect only while pausing: when a resume or prompt
// was issued before the pause settled, the task resumes right after.
func (s TaskState) afterEvent(event protocol.Event, stopIssued bool) TaskState {
	switch {
	case s.Ended():
		return s
	case event.Kind == protocol.KindHarnessStarted && s == TaskPending:
		return TaskRunning
	case event.Kind == protocol.KindPermissionRequested && s == TaskRunning:
		return TaskAwaitingPermission
	case event.Kind == protocol.KindPauseSettled && s == TaskPausing:
		return TaskPaused
	case event.Kind == protocol.KindHarnessExited:
		var exit protocol.HarnessExited
		decoded := json.Unmarshal(event.Payload, &exit) == nil
		switch {
		case stopIssued:
			return TaskStopped
		case decoded && exit.ExitCode == 0 && exit.Error == "":
			return TaskFinished
		default:
			return TaskFailed
		}
	}
	return s
}

// progress is what the server keeps per task as events are stored and
// commands issued, so that listing tasks needs no transcript.
type progress struct {
	State        TaskState
	LastActivity time.Time
	// CostUSD is the highest running total the harness has reported.
	CostUSD float64
}

// seeEvent folds a stored event into p.
func (p *progress) seeEvent(event protocol.Event, stopIssued bool) {
	p.State = p.State.afterEvent(event, stopIssued)
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

// seeCommand folds an issued command into p.
func (p *progress) seeCommand(command protocol.Command) {
	p.State = p.State.afterCommand(command.Kind)
	p.see(command.Time)
}

func (p *progress) see(at time.Time) {
	if at.After(p.LastActivity) {
		p.LastActivity = at.UTC()
	}
}

func loadProgress(ctx context.Context, tx *sql.Tx, task protocol.TaskID) (progress, error) {
	var state, lastActivity string
	var p progress
	err := tx.QueryRowContext(ctx, `SELECT state, last_activity_at, cost_usd FROM tasks WHERE id = ?`, string(task)).
		Scan(&state, &lastActivity, &p.CostUSD)
	if err != nil {
		return progress{}, fmt.Errorf("read progress of task %q: %w", task, err)
	}
	p.State = TaskState(state)
	if p.LastActivity, err = parseTime(lastActivity); err != nil {
		return progress{}, fmt.Errorf("read progress of task %q: %w", task, err)
	}
	return p, nil
}

func saveProgress(ctx context.Context, tx *sql.Tx, task protocol.TaskID, p progress) error {
	_, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, last_activity_at = ?, cost_usd = ? WHERE id = ?`,
		string(p.State), formatTime(p.LastActivity), p.CostUSD, string(task))
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
