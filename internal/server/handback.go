package server

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// handBack sends the final reply of task's turn, which has just ended with
// task finished, to task's parent as a message from task, unless task has
// no parent or sent a message during the turn
// (docs/adr/2026-10-09-agents-and-placement.md). A parent that has ended
// is not told, as with any notice.
func handBack(ctx context.Context, tx *sql.Tx, task protocol.TaskID, fx *effects) error {
	var parent sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT parent_id FROM tasks WHERE id = ?`, string(task)).Scan(&parent); err != nil {
		return fmt.Errorf("look up parent of task %q: %w", task, err)
	}
	if !parent.Valid {
		return nil
	}
	reported, err := sentThisTurn(ctx, tx, task)
	if err != nil || reported {
		return err
	}
	to := protocol.TaskID(parent.String)
	p, err := loadProgress(ctx, tx, to)
	if err != nil {
		return err
	}
	if p.State.Terminal() {
		return nil
	}
	text, err := finalReply(ctx, tx, task)
	if err != nil {
		return err
	}
	if text == "" {
		text = fmt.Sprintf("(Task %s finished its turn without writing any text.)", task)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages (from_task, to_task, text, created_at, hand_back) VALUES (?, ?, ?, ?, 1)`,
		string(task), string(to), text, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("hand back the reply of task %q: %w", task, err)
	}
	fx.changed = append(fx.changed, task, to)
	_, err = queueDelivery(ctx, tx, to, fx)
	return err
}

// sentThisTurn reports whether task sent a message since its latest turn
// started: since the latest start, prompt or resume issued to it. Both
// times are the server's clock.
func sentThisTurn(ctx context.Context, tx *sql.Tx, task protocol.TaskID) (bool, error) {
	var started string
	err := tx.QueryRowContext(ctx, `
		SELECT time FROM commands WHERE task_id = ? AND kind IN (?, ?, ?) ORDER BY id DESC LIMIT 1`,
		string(task), string(protocol.CommandStartTask), string(protocol.CommandPrompt), string(protocol.CommandResume)).Scan(&started)
	if err != nil {
		return false, fmt.Errorf("find the start of the turn of task %q: %w", task, err)
	}
	since, err := parseTime(started)
	if err != nil {
		return false, fmt.Errorf("find the start of the turn of task %q: %w", task, err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT created_at FROM messages WHERE from_task = ?`, string(task))
	if err != nil {
		return false, fmt.Errorf("read messages of task %q: %w", task, err)
	}
	defer rows.Close()
	for rows.Next() {
		var created string
		if err := rows.Scan(&created); err != nil {
			return false, fmt.Errorf("read messages of task %q: %w", task, err)
		}
		at, err := parseTime(created)
		if err != nil {
			return false, fmt.Errorf("read messages of task %q: %w", task, err)
		}
		if !at.Before(since) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// finalReply returns the last text the agent of task wrote in its main
// conversation since its latest process started, or "" when it wrote
// none. Text that the harness's own subagents wrote does not count.
func finalReply(ctx context.Context, tx *sql.Tx, task protocol.TaskID) (string, error) {
	var started int64
	err := tx.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM events WHERE task_id = ? AND kind = ?`,
		string(task), string(protocol.KindHarnessStarted)).Scan(&started)
	if err != nil {
		return "", fmt.Errorf("find the latest process of task %q: %w", task, err)
	}
	events, err := queryEvents(ctx, tx, task, uint64(started))
	if err != nil {
		return "", err
	}
	text := ""
	for _, event := range events {
		if event.Kind != protocol.KindHarnessOutput {
			continue
		}
		for _, body := range eventBodies(event) {
			if reply, ok := body.(transcript.AgentText); ok && reply.ParentToolUseID == "" {
				text = reply.Text
			}
		}
	}
	return text, nil
}
