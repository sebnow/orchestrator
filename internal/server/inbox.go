package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

var (
	// errNotRunning reports an agent request from a task that has no
	// process running, so no agent of it can have made the request.
	errNotRunning = errors.New("task has no process running")
	// errRefused reports an agent request the server will not carry out.
	// Its text is meant for the agent.
	errRefused = errors.New("refused")
)

// requireRunning checks that an agent request for task came from the
// daemon it is assigned to, or returns a *foreignTaskError, and that the
// task may have a process running, or returns errNotRunning. A pending
// task counts as running, because its process can call the gateway
// before the server holds its harness_started.
func requireRunning(ctx context.Context, tx *sql.Tx, daemon protocol.DaemonID, task protocol.TaskID) error {
	var owner string
	err := tx.QueryRowContext(ctx, `SELECT daemon_id FROM tasks WHERE id = ?`, string(task)).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) || err == nil && owner != string(daemon) {
		return &foreignTaskError{Task: task, Daemon: daemon}
	}
	if err != nil {
		return fmt.Errorf("look up task %q: %w", task, err)
	}
	p, err := loadProgress(ctx, tx, task)
	if err != nil {
		return err
	}
	if p.State.Terminal() || p.State.Idle() {
		return fmt.Errorf("%w: %q is %s", errNotRunning, task, p.State)
	}
	return nil
}

// spawnTask creates child, a child of parent, on parent's daemon and
// issues its start, for an agent of parent running on daemon. The child
// works in a fresh copy of parent's workspace, with parent's pause limits,
// and with parent's model unless spawn names one.
func (s *Store) spawnTask(ctx context.Context, daemon protocol.DaemonID, parent, child protocol.TaskID, spawn protocol.Spawn) (protocol.Command, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("spawn task: %w", err)
	}
	defer tx.Rollback()
	if err := requireRunning(ctx, tx, daemon, parent); err != nil {
		return protocol.Command{}, err
	}
	var model string
	var repo, ref sql.NullString
	var acknowledge, cleanup int64
	err = tx.QueryRowContext(ctx, `
		SELECT model, workspace_repo, workspace_ref, pause_acknowledge_ns, pause_cleanup_ns FROM tasks WHERE id = ?`,
		string(parent)).Scan(&model, &repo, &ref, &acknowledge, &cleanup)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("read task %q: %w", parent, err)
	}
	start := protocol.StartTask{
		Prompt:      spawn.Prompt,
		Model:       model,
		PauseLimits: protocol.PauseLimits{Acknowledge: time.Duration(acknowledge), Cleanup: time.Duration(cleanup)},
	}
	if spawn.Model != "" {
		start.Model = spawn.Model
	}
	if repo.Valid {
		start.Workspace = &protocol.Workspace{Repo: repo.String, Ref: ref.String}
	}
	fx := effects{changed: []protocol.TaskID{parent}}
	command, err := insertTask(ctx, tx, daemon, child, &parent, start, &fx)
	if err != nil {
		return protocol.Command{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.Command{}, fmt.Errorf("spawn task: %w", err)
	}
	s.publish(&fx)
	return command, nil
}

// sendMessage puts a message from the task from, whose agent runs on
// daemon, in the recipient's inbox, and delivers the inbox at once if
// the recipient is finished. It reports whether it delivered. A message
// to a task that does not exist, has ended, or is the sender is refused
// with errRefused.
func (s *Store) sendMessage(ctx context.Context, daemon protocol.DaemonID, from protocol.TaskID, send protocol.Send) (delivered bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("send message: %w", err)
	}
	defer tx.Rollback()
	if err := requireRunning(ctx, tx, daemon, from); err != nil {
		return false, err
	}
	if send.To == from {
		return false, fmt.Errorf("%w: a task cannot send a message to itself", errRefused)
	}
	recipient, err := loadProgress(ctx, tx, send.To)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: there is no task %q", errRefused, send.To)
	}
	if err != nil {
		return false, err
	}
	if recipient.State.Terminal() {
		return false, fmt.Errorf("%w: task %q has ended as %s and takes no messages", errRefused, send.To, recipient.State)
	}
	if err := insertMessage(ctx, tx, &from, nil, send.To, send.Text); err != nil {
		return false, err
	}
	fx := effects{changed: []protocol.TaskID{from}}
	if delivered, err = deliverWaiting(ctx, tx, send.To, &fx); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("send message: %w", err)
	}
	s.publish(&fx)
	return delivered, nil
}

// insertMessage puts text in to's inbox: from an agent's task, or, with
// from nil, the server's notice about another task.
func insertMessage(ctx context.Context, tx *sql.Tx, from, about *protocol.TaskID, to protocol.TaskID, text string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO messages (from_task, to_task, about_task, text, created_at) VALUES (?, ?, ?, ?, ?)`,
		nullableID(from), string(to), nullableID(about), text, formatTime(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("record message to task %q: %w", to, err)
	}
	return nil
}

// notifyParent tells the parent of child, which has just ended in state,
// that it has, unless child has no parent or the parent has ended too.
func notifyParent(ctx context.Context, tx *sql.Tx, child protocol.TaskID, state TaskState, fx *effects) error {
	var parent sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT parent_id FROM tasks WHERE id = ?`, string(child)).Scan(&parent); err != nil {
		return fmt.Errorf("look up parent of task %q: %w", child, err)
	}
	if !parent.Valid {
		return nil
	}
	recipient := protocol.TaskID(parent.String)
	p, err := loadProgress(ctx, tx, recipient)
	if err != nil {
		return err
	}
	if p.State.Terminal() {
		return nil
	}
	text := fmt.Sprintf("Your child task %s has ended as %s. It will send no more messages.", child, state)
	if err := insertMessage(ctx, tx, nil, &child, recipient, text); err != nil {
		return err
	}
	fx.changed = append(fx.changed, recipient)
	_, err = deliverWaiting(ctx, tx, recipient, fx)
	return err
}

// deliverWaiting issues every message waiting in task's inbox as one
// prompt, if task is finished and any wait. It reports whether it issued
// one. A task in any other state keeps its messages until it is finished:
// a running task's turn would otherwise be cut into, and a paused task is
// held by the owner.
func deliverWaiting(ctx context.Context, tx *sql.Tx, task protocol.TaskID, fx *effects) (bool, error) {
	p, err := loadProgress(ctx, tx, task)
	if err != nil {
		return false, err
	}
	if p.State != TaskFinished {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, from_task, about_task, text FROM messages
		WHERE to_task = ? AND delivered_command_id IS NULL ORDER BY id`, string(task))
	if err != nil {
		return false, fmt.Errorf("read inbox of task %q: %w", task, err)
	}
	var waiting []inboxMessage
	var last int64
	for rows.Next() {
		var message inboxMessage
		if err := rows.Scan(&last, &message.from, &message.about, &message.text); err != nil {
			rows.Close()
			return false, fmt.Errorf("read inbox of task %q: %w", task, err)
		}
		waiting = append(waiting, message)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read inbox of task %q: %w", task, err)
	}
	if len(waiting) == 0 {
		return false, nil
	}
	payload, err := json.Marshal(deliveryPrompt(waiting))
	if err != nil {
		return false, fmt.Errorf("encode prompt: %w", err)
	}
	var daemon string
	if err := tx.QueryRowContext(ctx, `SELECT daemon_id FROM tasks WHERE id = ?`, string(task)).Scan(&daemon); err != nil {
		return false, fmt.Errorf("look up task %q: %w", task, err)
	}
	command, err := insertCommand(ctx, tx, protocol.DaemonID(daemon), task, protocol.CommandPrompt, payload, fx)
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE messages SET delivered_command_id = ? WHERE to_task = ? AND delivered_command_id IS NULL AND id <= ?`,
		int64(command.ID), string(task), last)
	if err != nil {
		return false, fmt.Errorf("record delivery to task %q: %w", task, err)
	}
	return true, nil
}

// inboxMessage is a message waiting to be delivered. Exactly one of from
// and about is set: from for an agent's message, about for the server's
// notice that a child ended.
type inboxMessage struct {
	from, about sql.NullString
	text        string
}

// deliveryPrompt words messages, oldest first, as one prompt from the
// sender of the oldest.
func deliveryPrompt(messages []inboxMessage) protocol.Prompt {
	parts := make([]string, len(messages))
	for idx, message := range messages {
		if message.from.Valid {
			parts[idx] = "Message from task " + message.from.String + ": " + message.text
		} else {
			parts[idx] = "Notice from the orchestrator: " + message.text
		}
	}
	oldest := messages[0].from
	if !oldest.Valid {
		oldest = messages[0].about
	}
	from := protocol.TaskID(oldest.String)
	return protocol.Prompt{Text: strings.Join(parts, "\n\n"), From: &from}
}
