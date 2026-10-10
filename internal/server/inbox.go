package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// spawnTask creates child, a child of parent, and queues its start, for
// an agent of parent running on daemon. The start goes to parent's
// daemon unless that has no free slot when it is admitted
// (docs/adr/2026-10-08-scheduling.md). The child works in a fresh copy of
// parent's workspace, belongs to parent's project, and has spawn's
// purpose, if any, at the top of its prompt
// (docs/adr/2026-10-10-projects-and-lineage.md). Started as the agent spawn names, it has the
// agent's priority and filler flag, the agent's model and pause limits,
// or else parent's (docs/adr/2026-10-09-agents-and-placement.md), and
// those of the agent's tools that parent may call; started as none, it
// has parent's tools and settings
// (docs/design/2026-10-09-nostr-direction.md, Tool inheritance). A model
// spawn names wins over both, and so do the labels spawn requires, if it
// gives them.
// A parent not allowed spawn_task, or a spawn naming no agent there is,
// is refused with errRefused.
func (s *Store) spawnTask(ctx context.Context, daemon protocol.DaemonID, parent, child protocol.TaskID, spawn protocol.Spawn) (queuedTurn, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return queuedTurn{}, fmt.Errorf("spawn task: %w", err)
	}
	defer tx.Rollback()
	if err := requireRunning(ctx, tx, daemon, parent); err != nil {
		return queuedTurn{}, err
	}
	parentTools, err := taskTools(ctx, tx, parent)
	if err != nil {
		return queuedTurn{}, err
	}
	if err := requireTool(parent, parentTools, protocol.ToolSpawnTask); err != nil {
		return queuedTurn{}, err
	}
	var model, priority, project string
	var repo, ref sql.NullString
	var acknowledge, cleanup int64
	var filler bool
	err = tx.QueryRowContext(ctx, `
		SELECT model, workspace_repo, workspace_ref, pause_acknowledge_ns, pause_cleanup_ns, priority, filler, coalesce(project, '') FROM tasks WHERE id = ?`,
		string(parent)).Scan(&model, &repo, &ref, &acknowledge, &cleanup, &priority, &filler, &project)
	if err != nil {
		return queuedTurn{}, fmt.Errorf("read task %q: %w", parent, err)
	}
	purpose := oneLine(spawn.Purpose)
	start := protocol.StartTask{
		Prompt:      withPurpose(purpose, spawn.Prompt),
		Model:       model,
		PauseLimits: protocol.PauseLimits{Acknowledge: time.Duration(acknowledge), Cleanup: time.Duration(cleanup)},
		Tools:       parentTools,
	}
	var agentPrompt string
	requires := Labels{}
	if spawn.Agent != "" {
		a, err := queryAgent(ctx, tx, spawn.Agent)
		if errors.Is(err, errUnknownAgent) {
			return queuedTurn{}, fmt.Errorf("%w: there is no agent %q", errRefused, spawn.Agent)
		}
		if err != nil {
			return queuedTurn{}, err
		}
		if a.Model != "" {
			start.Model = a.Model
		}
		if a.PauseLimits != nil {
			start.PauseLimits = *a.PauseLimits
		}
		start.Tools, priority, filler, agentPrompt, requires = narrowTools(a.Tools, parentTools), string(a.Priority), a.Filler, a.SystemPrompt, a.Requires
	}
	if spawn.Requires != nil {
		requires = Labels(spawn.Requires)
		if err := requires.Validate(); err != nil {
			return queuedTurn{}, fmt.Errorf("%w: requires: %v", errRefused, err)
		}
	}
	if spawn.Model != "" {
		start.Model = spawn.Model
	}
	agents, err := queryAgents(ctx, tx)
	if err != nil {
		return queuedTurn{}, err
	}
	var instructions string
	if project != "" {
		p, err := queryProject(ctx, tx, project)
		if err != nil {
			return queuedTurn{}, err
		}
		instructions = p.Instructions
	}
	start.SystemPrompt = systemPrompt(promptParts{Parent: &parent, Tools: start.Tools, Agents: agents, Agent: agentPrompt, Project: instructions})
	if repo.Valid {
		start.Workspace = &protocol.Workspace{Repo: repo.String, Ref: ref.String}
	}
	fx := effects{changed: []protocol.TaskID{parent}}
	turn, err := insertTask(ctx, tx, newTask{
		ID: child, Parent: &parent, Daemon: daemon, Placement: placementParent, Agent: spawn.Agent, Project: project, Purpose: purpose, Requires: requires,
		Priority: Priority(priority), Filler: filler, Start: start, Origin: originServer,
	}, &fx)
	if err != nil {
		return queuedTurn{}, err
	}
	if err := tx.Commit(); err != nil {
		return queuedTurn{}, fmt.Errorf("spawn task: %w", err)
	}
	s.publish(&fx)
	return turn, nil
}

// taskTools reads the gateway tools task's start allows, nil for every
// one of them.
func taskTools(ctx context.Context, tx *sql.Tx, task protocol.TaskID) ([]string, error) {
	var tools sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT tools FROM tasks WHERE id = ?`, string(task)).Scan(&tools); err != nil {
		return nil, fmt.Errorf("read tools of task %q: %w", task, err)
	}
	if !tools.Valid {
		return nil, nil
	}
	allowed := []string{}
	if err := json.Unmarshal([]byte(tools.String), &allowed); err != nil {
		return nil, fmt.Errorf("read tools of task %q: %w", task, err)
	}
	return allowed, nil
}

// narrowTools returns those of tools that limit allows, in tools' order,
// never nil; a nil limit allows every tool.
func narrowTools(tools, limit []string) []string {
	if limit == nil {
		return tools
	}
	narrowed := []string{}
	for _, tool := range tools {
		if slices.Contains(limit, tool) {
			narrowed = append(narrowed, tool)
		}
	}
	return narrowed
}

// requireTool refuses, with errRefused, a request for tool from task when
// allowed, the tools its start allows, does not hold tool. The daemon
// does not offer such a tool, so this guards against a daemon that does.
func requireTool(task protocol.TaskID, allowed []string, tool string) error {
	if allowed != nil && !slices.Contains(allowed, tool) {
		return fmt.Errorf("%w: task %s is not allowed %s", errRefused, task, tool)
	}
	return nil
}

// sendMessage puts a message from the task from, whose agent runs on
// daemon, in the recipient's inbox, and queues the inbox's delivery if
// the recipient is finished. It reports whether the message is to be the
// recipient's next turn. A message to a task that does not exist, has
// ended, or is the sender is refused with errRefused.
func (s *Store) sendMessage(ctx context.Context, daemon protocol.DaemonID, from protocol.TaskID, send protocol.Send) (delivered bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("send message: %w", err)
	}
	defer tx.Rollback()
	if err := requireRunning(ctx, tx, daemon, from); err != nil {
		return false, err
	}
	fromTools, err := taskTools(ctx, tx, from)
	if err != nil {
		return false, err
	}
	if err := requireTool(from, fromTools, protocol.ToolSendMessage); err != nil {
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
	if delivered, err = queueDelivery(ctx, tx, send.To, &fx); err != nil {
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

// taskEnded tells the tasks that wait on task, which has just ended in
// state, that it has: its parent, which will hear from it no more, and
// every task whose messages wait undelivered in its inbox, which never
// will be delivered. A parent that sent such messages hears of both in
// one notice.
func taskEnded(ctx context.Context, tx *sql.Tx, task protocol.TaskID, state TaskState, fx *effects) error {
	var parent sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT parent_id FROM tasks WHERE id = ?`, string(task)).Scan(&parent); err != nil {
		return fmt.Errorf("look up parent of task %q: %w", task, err)
	}
	senders, err := undeliveredSenders(ctx, tx, task)
	if err != nil {
		return err
	}
	for _, sender := range senders {
		if parent.Valid && sender.task == protocol.TaskID(parent.String) {
			continue
		}
		text := fmt.Sprintf("Task %s has ended as %s before your message reached it, so it was not delivered.", task, state)
		if sender.count > 1 {
			text = fmt.Sprintf("Task %s has ended as %s before your %d messages reached it, so they were not delivered.", task, state, sender.count)
		}
		if err := notify(ctx, tx, sender.task, task, text, fx); err != nil {
			return err
		}
	}
	if !parent.Valid {
		return nil
	}
	text := fmt.Sprintf("Your child task %s has ended as %s. It will send no more messages.", task, state)
	for _, sender := range senders {
		switch {
		case sender.task != protocol.TaskID(parent.String):
		case sender.count == 1:
			text += " Your message to it was not delivered."
		default:
			text += fmt.Sprintf(" Your %d messages to it were not delivered.", sender.count)
		}
	}
	return notify(ctx, tx, protocol.TaskID(parent.String), task, text, fx)
}

// undeliveredSender is a task with count messages waiting undelivered in
// another task's inbox.
type undeliveredSender struct {
	task  protocol.TaskID
	count int
}

// undeliveredSenders returns the tasks with messages waiting in task's
// inbox, in the order of their oldest message there.
func undeliveredSenders(ctx context.Context, tx *sql.Tx, task protocol.TaskID) ([]undeliveredSender, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT from_task, count(*) FROM messages
		WHERE to_task = ? AND delivered_command_id IS NULL AND from_task IS NOT NULL
		GROUP BY from_task ORDER BY min(id)`, string(task))
	if err != nil {
		return nil, fmt.Errorf("read inbox of task %q: %w", task, err)
	}
	defer rows.Close()
	var senders []undeliveredSender
	for rows.Next() {
		var sender string
		var count int
		if err := rows.Scan(&sender, &count); err != nil {
			return nil, fmt.Errorf("read inbox of task %q: %w", task, err)
		}
		senders = append(senders, undeliveredSender{task: protocol.TaskID(sender), count: count})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read inbox of task %q: %w", task, err)
	}
	return senders, nil
}

// notify puts the server's notice text about the task about in
// recipient's inbox and queues its delivery, so that recipient hears it
// as its next prompt, unless recipient has ended.
func notify(ctx context.Context, tx *sql.Tx, recipient, about protocol.TaskID, text string, fx *effects) error {
	p, err := loadProgress(ctx, tx, recipient)
	if err != nil {
		return err
	}
	if p.State.Terminal() {
		return nil
	}
	if err := insertMessage(ctx, tx, nil, &about, recipient, text); err != nil {
		return err
	}
	fx.changed = append(fx.changed, recipient)
	_, err = queueDelivery(ctx, tx, recipient, fx)
	return err
}

// queueDelivery queues the delivery of the messages waiting in task's
// inbox as a turn, if task is finished and any wait, unless a delivery
// is queued already; that one carries every message waiting when it is
// admitted. It reports whether a delivery is queued. A task in any other
// state keeps its messages until it is finished: a running task's turn
// would otherwise be cut into, and a paused task is held by the owner.
func queueDelivery(ctx context.Context, tx *sql.Tx, task protocol.TaskID, fx *effects) (bool, error) {
	p, err := loadProgress(ctx, tx, task)
	if err != nil {
		return false, err
	}
	if p.State != TaskFinished {
		return false, nil
	}
	var waiting, queued, filler bool
	err = tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM messages WHERE to_task = ?1 AND delivered_command_id IS NULL),
			EXISTS (SELECT 1 FROM turns WHERE task_id = ?1 AND kind = ?2 AND admitted_command_id IS NULL),
			filler
		FROM tasks WHERE id = ?1`, string(task), string(turnDeliver)).Scan(&waiting, &queued, &filler)
	if err != nil {
		return false, fmt.Errorf("read inbox of task %q: %w", task, err)
	}
	if !waiting {
		return false, nil
	}
	if !queued {
		if _, err := insertTurn(ctx, tx, task, turnDeliver, nil, originServer, filler, fx); err != nil {
			return false, err
		}
	}
	return true, nil
}

// deliverWaiting issues every message waiting in task's inbox, assigned
// to daemon, as one prompt, and returns it; it returns a command with no
// ID when none waits.
func deliverWaiting(ctx context.Context, tx *sql.Tx, daemon protocol.DaemonID, task protocol.TaskID, fx *effects) (protocol.Command, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, from_task, about_task, text, hand_back FROM messages
		WHERE to_task = ? AND delivered_command_id IS NULL ORDER BY id`, string(task))
	if err != nil {
		return protocol.Command{}, fmt.Errorf("read inbox of task %q: %w", task, err)
	}
	var waiting []inboxMessage
	var last int64
	for rows.Next() {
		var message inboxMessage
		if err := rows.Scan(&last, &message.from, &message.about, &message.text, &message.handBack); err != nil {
			rows.Close()
			return protocol.Command{}, fmt.Errorf("read inbox of task %q: %w", task, err)
		}
		waiting = append(waiting, message)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return protocol.Command{}, fmt.Errorf("read inbox of task %q: %w", task, err)
	}
	if len(waiting) == 0 {
		return protocol.Command{}, nil
	}
	payload, err := json.Marshal(deliveryPrompt(waiting))
	if err != nil {
		return protocol.Command{}, fmt.Errorf("encode prompt: %w", err)
	}
	command, err := insertCommand(ctx, tx, daemon, task, protocol.CommandPrompt, payload, fx)
	if err != nil {
		return protocol.Command{}, err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE messages SET delivered_command_id = ? WHERE to_task = ? AND delivered_command_id IS NULL AND id <= ?`,
		int64(command.ID), string(task), last)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("record delivery to task %q: %w", task, err)
	}
	return command, nil
}

// inboxMessage is a message waiting to be delivered. Exactly one of from
// and about is set: from for an agent's message, about for the server's
// notice that a task ended. handBack marks a child's final reply that the
// server sent for it.
type inboxMessage struct {
	from, about sql.NullString
	text        string
	handBack    bool
}

// deliveryPrompt words messages, oldest first, as one prompt from the
// sender of the oldest.
func deliveryPrompt(messages []inboxMessage) protocol.Prompt {
	parts := make([]string, len(messages))
	for idx, message := range messages {
		switch {
		case message.from.Valid && message.handBack:
			parts[idx] = "Report from child task " + message.from.String + ", its final reply as it ended its turn: " + message.text
		case message.from.Valid:
			parts[idx] = "Message from task " + message.from.String + ": " + message.text
		default:
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
