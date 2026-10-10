package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// DefaultDaemonTimeout is how long a daemon may go unseen, with no command
// stream open, before the server declares it lost
// (docs/adr/2026-10-08-daemon-loss.md).
const DefaultDaemonTimeout = 10 * time.Minute

// losses are the daemons lost at a moment, and when the next of the
// others would be lost if it stays away: zero when none would.
type losses struct {
	lost  map[protocol.DaemonID]bool
	newly []protocol.DaemonID
	next  time.Time
}

// findLost declares lost, at now, each daemon that has no command stream
// open, is not among connected, and has not been seen for timeout. Time
// before upSince, when the server started, does not count, so that a
// daemon is not declared lost for the server's own absence. A daemon
// declared lost stays so until it is seen again. A timeout of zero
// declares no daemon lost.
func findLost(ctx context.Context, tx *sql.Tx, connected []protocol.DaemonID, now, upSince time.Time, timeout time.Duration) (losses, error) {
	l := losses{lost: make(map[protocol.DaemonID]bool)}
	if timeout <= 0 {
		return l, nil
	}
	isConnected := make(map[protocol.DaemonID]bool, len(connected))
	for _, daemon := range connected {
		isConnected[daemon] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, last_seen, lost_at IS NOT NULL FROM daemons`)
	if err != nil {
		return losses{}, fmt.Errorf("read daemons: %w", err)
	}
	type seen struct {
		id   protocol.DaemonID
		last time.Time
	}
	var away []seen
	for rows.Next() {
		var id, lastSeen string
		var declared bool
		if err := rows.Scan(&id, &lastSeen, &declared); err != nil {
			rows.Close()
			return losses{}, fmt.Errorf("read daemons: %w", err)
		}
		daemon := protocol.DaemonID(id)
		if isConnected[daemon] {
			continue
		}
		if declared {
			l.lost[daemon] = true
			continue
		}
		last, err := parseTime(lastSeen)
		if err != nil {
			rows.Close()
			return losses{}, fmt.Errorf("read daemon %q: %w", id, err)
		}
		away = append(away, seen{id: daemon, last: last})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return losses{}, fmt.Errorf("read daemons: %w", err)
	}
	for _, daemon := range away {
		at := daemon.last
		if upSince.After(at) {
			at = upSince
		}
		at = at.Add(timeout)
		if now.Before(at) {
			if l.next.IsZero() || at.Before(l.next) {
				l.next = at
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE daemons SET lost_at = ? WHERE id = ?`, formatTime(now.UTC()), string(daemon.id)); err != nil {
			return losses{}, fmt.Errorf("declare daemon %q lost: %w", daemon.id, err)
		}
		l.lost[daemon.id] = true
		l.newly = append(l.newly, daemon.id)
	}
	return l, nil
}

// moveLostTasks moves the tasks on lost daemons that have work to do: a
// turn under way, a prompt or resume queued, or, once finished, a
// delivery queued. A task between turns with nothing queued stays until
// its next turn is queued, so that it can carry on where it was should
// its daemon return first. A task whose start the owner bound to a lost
// daemon, and which has not started, is placed like any other instead.
// It returns the tasks moved.
func moveLostTasks(ctx context.Context, tx *sql.Tx, lost map[protocol.DaemonID]bool, now time.Time, fx *effects) ([]protocol.TaskID, error) {
	var moved []protocol.TaskID
	for daemon := range lost {
		rows, err := tx.QueryContext(ctx, `
			SELECT t.id, t.state, t.placement, t.parent_id IS NOT NULL,
				EXISTS (SELECT 1 FROM turns u WHERE u.task_id = t.id AND u.admitted_command_id IS NULL AND u.kind IN (?2, ?3)),
				EXISTS (SELECT 1 FROM turns u WHERE u.task_id = t.id AND u.admitted_command_id IS NULL AND u.kind = ?4)
			FROM tasks t WHERE t.daemon_id = ?1 AND t.dismissed_at IS NULL`,
			string(daemon), string(turnPrompt), string(turnResume), string(turnDeliver))
		if err != nil {
			return nil, fmt.Errorf("read the tasks of daemon %q: %w", daemon, err)
		}
		type candidate struct {
			task              protocol.TaskID
			state             TaskState
			placed            placement
			child             bool
			queued, delivered bool
		}
		var candidates []candidate
		for rows.Next() {
			var c candidate
			var task, state, placed string
			if err := rows.Scan(&task, &state, &placed, &c.child, &c.queued, &c.delivered); err != nil {
				rows.Close()
				return nil, fmt.Errorf("read the tasks of daemon %q: %w", daemon, err)
			}
			c.task, c.state, c.placed = protocol.TaskID(task), TaskState(state), placement(placed)
			candidates = append(candidates, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read the tasks of daemon %q: %w", daemon, err)
		}
		for _, c := range candidates {
			again := placementAny
			if c.child {
				again = placementParent
			}
			switch {
			case c.state == TaskQueued && c.placed == placementBound:
				if _, err := tx.ExecContext(ctx, `UPDATE tasks SET placement = ? WHERE id = ?`, string(again), string(c.task)); err != nil {
					return nil, fmt.Errorf("place task %q again: %w", c.task, err)
				}
				fx.changed = append(fx.changed, c.task)
				fx.reschedule = true
			case holdsSlot(c.state), c.state.Idle() && c.state != TaskQueued && c.queued, c.state == TaskFinished && c.delivered:
				if err := moveTask(ctx, tx, c.task, daemon, again, now, fx); err != nil {
					return nil, err
				}
				moved = append(moved, c.task)
			}
		}
	}
	return moved, nil
}

// moveTask takes task off its lost daemon from: its workspace and harness
// session are gone with that daemon, so its next turn is a new start,
// placed by again, carrying its first prompt and a note saying so. The
// owner's queued prompts and every queued resume give way to that start;
// the note carries every queued prompt, oldest first, or, when none is
// queued, quotes the owner's latest prompt. Queued deliveries stay
// queued.
func moveTask(ctx context.Context, tx *sql.Tx, task protocol.TaskID, from protocol.DaemonID, again placement, now time.Time, fx *effects) error {
	queued, err := queuedOwnerPrompts(ctx, tx, task)
	if err != nil {
		return err
	}
	var latest string
	if len(queued) == 0 {
		if latest, err = latestOwnerPrompt(ctx, tx, task); err != nil {
			return err
		}
	}
	var prompt, payload string
	var filler bool
	err = tx.QueryRowContext(ctx, `
		SELECT t.prompt, t.filler, c.payload FROM tasks t JOIN commands c ON c.task_id = t.id
		WHERE t.id = ? AND c.kind = ? ORDER BY c.id LIMIT 1`, string(task), string(protocol.CommandStartTask)).Scan(&prompt, &filler, &payload)
	if err != nil {
		return fmt.Errorf("read the start of task %q: %w", task, err)
	}
	var start protocol.StartTask
	if err := json.Unmarshal([]byte(payload), &start); err != nil {
		return fmt.Errorf("read the start of task %q: %w", task, err)
	}
	start.Prompt = prompt + "\n\n" + movedNote(from, start.Workspace != nil, queued, latest)
	restart, err := json.Marshal(start)
	if err != nil {
		return fmt.Errorf("encode the new start of task %q: %w", task, err)
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM turns WHERE task_id = ? AND kind IN (?, ?) AND admitted_command_id IS NULL`,
		string(task), string(turnPrompt), string(turnResume))
	if err != nil {
		return fmt.Errorf("drop the queued turns of task %q: %w", task, err)
	}
	p, err := loadProgress(ctx, tx, task)
	if err != nil {
		return err
	}
	p.State, p.PausedBy = TaskQueued, ""
	p.newSession()
	p.see(now)
	if err := saveProgress(ctx, tx, task, p); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET placement = ? WHERE id = ?`, string(again), string(task)); err != nil {
		return fmt.Errorf("place task %q again: %w", task, err)
	}
	_, err = insertTurn(ctx, tx, task, turnStart, restart, originServer, filler, fx)
	return err
}

// queuedOwnerPrompts returns the texts of the owner's prompts queued for
// task and not yet admitted, oldest first.
func queuedOwnerPrompts(ctx context.Context, tx *sql.Tx, task protocol.TaskID) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT payload FROM turns WHERE task_id = ? AND kind = ? AND admitted_command_id IS NULL ORDER BY id`,
		string(task), string(turnPrompt))
	if err != nil {
		return nil, fmt.Errorf("read the queued prompts of task %q: %w", task, err)
	}
	defer rows.Close()
	var queued []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("read the queued prompts of task %q: %w", task, err)
		}
		var prompt protocol.Prompt
		if json.Unmarshal([]byte(payload), &prompt) == nil {
			queued = append(queued, prompt.Text)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the queued prompts of task %q: %w", task, err)
	}
	return queued, nil
}

// latestOwnerPrompt returns the text of the owner's newest prompt issued
// to task; "" when there is none.
func latestOwnerPrompt(ctx context.Context, tx *sql.Tx, task protocol.TaskID) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM commands WHERE task_id = ? AND kind = ? ORDER BY id DESC`,
		string(task), string(protocol.CommandPrompt))
	if err != nil {
		return "", fmt.Errorf("read the prompts of task %q: %w", task, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return "", fmt.Errorf("read the prompts of task %q: %w", task, err)
		}
		var prompt protocol.Prompt
		if json.Unmarshal(raw, &prompt) == nil && prompt.From == nil {
			return prompt.Text, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read the prompts of task %q: %w", task, err)
	}
	return "", nil
}

// movedNote tells the agent of a task moved off the lost daemon from what
// of its earlier work is gone. It carries each of the owner's queued
// prompts, oldest first and labelled, or, when none is queued, quotes
// latest, the owner's latest prompt, when that is not empty. For a task
// with a repository, the commits the lost daemon pushed survive on the
// task's branch, which the new daemon checks out
// (docs/adr/2026-10-08-work-delivery.md).
func movedNote(from protocol.DaemonID, repository bool, queued []string, latest string) string {
	note := "Note from the orchestrator: you worked on this task before on daemon " + string(from) +
		", which has been lost. Everything you did there is gone, your workspace and your conversation alike, " +
		"so you are starting again in a fresh workspace."
	if repository {
		note = "Note from the orchestrator: you worked on this task before on daemon " + string(from) +
			", which has been lost, and you are starting again in a fresh clone and a new conversation. " +
			"The commits pushed from there are on your task's branch, which your clone has checked out; " +
			"only the work that was not pushed is gone, along with your earlier conversation."
	}
	switch {
	case len(queued) > 0:
		note += " The owner sent you these prompts, which you had not been given yet, oldest first:"
		for idx, text := range queued {
			note += "\n\nOwner's prompt " + strconv.Itoa(idx+1) + " of " + strconv.Itoa(len(queued)) + ":\n\n" + text
		}
	case latest != "":
		note += " The owner's latest prompt to you was:\n\n" + latest
	}
	return note
}

// movedFrom reports whether the server moved task, now on owner in state,
// from daemon: daemon was sent the task's start, and the task has since
// gone back to waiting for a start or been started elsewhere.
func movedFrom(ctx context.Context, tx *sql.Tx, task protocol.TaskID, daemon protocol.DaemonID, owner string, state TaskState) (bool, error) {
	if owner == string(daemon) && state != TaskQueued {
		return false, nil
	}
	var started bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM commands WHERE task_id = ? AND daemon_id = ? AND kind = ?)`,
		string(task), string(daemon), string(protocol.CommandStartTask)).Scan(&started)
	if err != nil {
		return false, fmt.Errorf("look up the starts of task %q: %w", task, err)
	}
	return started, nil
}
