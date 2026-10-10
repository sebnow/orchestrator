package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// errNotQueued reports a withdrawal of a prompt that no longer waits:
// the harness has started answering it, it was dropped, or it was never
// queued.
var errNotQueued = errors.New("prompt is not queued")

// queuedPrompt is one of the owner's prompts to a task that has not
// reached the harness yet: a turn waiting for the scheduler, Turn, or a
// prompt command, Prompt, that the task's daemon holds until the running
// turn ends. Exactly one of Turn and Prompt is set. Either can be
// withdrawn.
type queuedPrompt struct {
	Turn   uint64    `json:"turn,omitempty"`
	Prompt uint64    `json:"prompt,omitempty"`
	Text   string    `json:"text"`
	Since  time.Time `json:"since"`
}

// queuedPrompts returns task's queued prompts, oldest first: those its
// daemon holds, then those waiting for the scheduler.
func queuedPrompts(ctx context.Context, tx *sql.Tx, task protocol.TaskID) ([]queuedPrompt, error) {
	held, err := heldPrompts(ctx, tx, task)
	if err != nil {
		return nil, err
	}
	queued := []queuedPrompt{}
	for _, ref := range held {
		var payload, issued string
		err := tx.QueryRowContext(ctx, `SELECT payload, time FROM commands WHERE id = ? AND task_id = ? AND kind = ?`,
			int64(ref), string(task), string(protocol.CommandPrompt)).Scan(&payload, &issued)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read prompt %d of task %q: %w", ref, task, err)
		}
		q := queuedPrompt{Prompt: ref}
		if err := fillQueued(&q, payload, issued); err != nil {
			return nil, fmt.Errorf("read prompt %d of task %q: %w", ref, task, err)
		}
		queued = append(queued, q)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, payload, created_at FROM turns
		WHERE task_id = ? AND kind = ? AND origin = ? AND admitted_command_id IS NULL ORDER BY id`,
		string(task), string(turnPrompt), string(originOwner))
	if err != nil {
		return nil, fmt.Errorf("read the waiting prompts of task %q: %w", task, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var payload, created string
		if err := rows.Scan(&id, &payload, &created); err != nil {
			return nil, fmt.Errorf("read the waiting prompts of task %q: %w", task, err)
		}
		q := queuedPrompt{Turn: uint64(id)}
		if err := fillQueued(&q, payload, created); err != nil {
			return nil, fmt.Errorf("read turn %d of task %q: %w", id, task, err)
		}
		queued = append(queued, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the waiting prompts of task %q: %w", task, err)
	}
	return queued, nil
}

func fillQueued(q *queuedPrompt, payload, at string) error {
	var prompt protocol.Prompt
	if err := json.Unmarshal([]byte(payload), &prompt); err != nil {
		return err
	}
	since, err := parseTime(at)
	if err != nil {
		return err
	}
	q.Text, q.Since = prompt.Text, since
	return nil
}

// heldPrompts returns the ids of the prompt commands task's daemon holds,
// oldest first: those it reported held and not yet released.
func heldPrompts(ctx context.Context, tx *sql.Tx, task protocol.TaskID) ([]uint64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT kind, payload FROM events WHERE task_id = ? AND kind IN (?, ?) ORDER BY seq`,
		string(task), string(protocol.KindPromptHeld), string(protocol.KindPromptReleased))
	if err != nil {
		return nil, fmt.Errorf("read the held prompts of task %q: %w", task, err)
	}
	defer rows.Close()
	var held []uint64
	for rows.Next() {
		var kind, payload string
		if err := rows.Scan(&kind, &payload); err != nil {
			return nil, fmt.Errorf("read the held prompts of task %q: %w", task, err)
		}
		var ref struct {
			Prompt uint64 `json:"prompt"`
		}
		if json.Unmarshal([]byte(payload), &ref) != nil || ref.Prompt == 0 {
			continue
		}
		if protocol.Kind(kind) == protocol.KindPromptHeld {
			held = append(held, ref.Prompt)
		} else {
			held = slices.DeleteFunc(held, func(id uint64) bool { return id == ref.Prompt })
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the held prompts of task %q: %w", task, err)
	}
	return held, nil
}

// queuedPromptsOf returns task's queued prompts, or errUnknownTask.
func (s *Store) queuedPromptsOf(ctx context.Context, task protocol.TaskID) ([]queuedPrompt, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("read the queued prompts of task %q: %w", task, err)
	}
	defer tx.Rollback()
	if err := requireTask(ctx, tx, task); err != nil {
		return nil, err
	}
	return queuedPrompts(ctx, tx, task)
}

// withdrawPrompt asks task's daemon to drop the prompt command ref, which
// the daemon holds until the running turn ends, and returns the withdraw
// command. A prompt the server does not know to be held gets
// errNotQueued. The daemon may still have sent it before the withdrawal
// arrives; its prompt_released says which.
func (s *Store) withdrawPrompt(ctx context.Context, task protocol.TaskID, ref uint64) (protocol.Command, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("withdraw prompt %d: %w", ref, err)
	}
	defer tx.Rollback()
	if err := requireTask(ctx, tx, task); err != nil {
		return protocol.Command{}, err
	}
	held, err := heldPrompts(ctx, tx, task)
	if err != nil {
		return protocol.Command{}, err
	}
	if !slices.Contains(held, ref) {
		return protocol.Command{}, fmt.Errorf("%w: prompt %d of task %q is not held by its daemon", errNotQueued, ref, task)
	}
	var daemon string
	if err := tx.QueryRowContext(ctx, `SELECT daemon_id FROM commands WHERE id = ?`, int64(ref)).Scan(&daemon); err != nil {
		return protocol.Command{}, fmt.Errorf("look up prompt %d: %w", ref, err)
	}
	payload, err := json.Marshal(protocol.Withdraw{Prompt: ref})
	if err != nil {
		return protocol.Command{}, fmt.Errorf("encode withdraw: %w", err)
	}
	var fx effects
	command, err := insertCommand(ctx, tx, protocol.DaemonID(daemon), task, protocol.CommandWithdraw, payload, &fx)
	if err != nil {
		return protocol.Command{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.Command{}, fmt.Errorf("withdraw prompt %d: %w", ref, err)
	}
	s.publish(&fx)
	return command, nil
}

// withdrawTurn removes the owner's prompt turn of task that waits for the
// scheduler. A turn that is not such a turn, or no longer waits, gets
// errNotQueued.
func (s *Store) withdrawTurn(ctx context.Context, task protocol.TaskID, turn uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("withdraw turn %d: %w", turn, err)
	}
	defer tx.Rollback()
	if err := requireTask(ctx, tx, task); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		DELETE FROM turns WHERE id = ? AND task_id = ? AND kind = ? AND origin = ? AND admitted_command_id IS NULL`,
		int64(turn), string(task), string(turnPrompt), string(originOwner))
	if err != nil {
		return fmt.Errorf("withdraw turn %d: %w", turn, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return fmt.Errorf("withdraw turn %d: %w", turn, err)
		}
		return fmt.Errorf("%w: turn %d of task %q does not wait", errNotQueued, turn, task)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("withdraw turn %d: %w", turn, err)
	}
	s.publish(&effects{changed: []protocol.TaskID{task}, reschedule: true})
	return nil
}
