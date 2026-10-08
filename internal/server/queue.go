package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// Priority orders a task's turns against other tasks' turns
// (docs/adr/2026-10-08-scheduling.md).
type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
)

var errInvalidPriority = errors.New("priority must be low, normal or high")

// ParsePriority reads a priority; an empty one is normal.
func ParsePriority(raw string) (Priority, error) {
	switch Priority(raw) {
	case "":
		return PriorityNormal, nil
	case PriorityLow, PriorityNormal, PriorityHigh:
		return Priority(raw), nil
	}
	return "", fmt.Errorf("%w, not %q", errInvalidPriority, raw)
}

// rank orders priorities, the highest greatest.
func (p Priority) rank() int {
	switch p {
	case PriorityHigh:
		return 2
	case PriorityNormal:
		return 1
	}
	return 0
}

// placement is how a task's first turn picks its daemon: on the task's
// daemon (bound), on the connected daemon with the most free slots (any),
// or on the task's daemon, its parent's, unless that has no free slot
// (parent). Admitting the first turn binds the task to its daemon.
type placement string

const (
	placementBound  placement = "bound"
	placementAny    placement = "any"
	placementParent placement = "parent"
)

// turnKind is what admitting a turn issues: the command of the same
// kind, or, for turnDeliver, a prompt carrying the messages waiting in
// the task's inbox at that moment.
type turnKind string

const (
	turnStart   turnKind = "start_task"
	turnPrompt  turnKind = "prompt"
	turnResume  turnKind = "resume"
	turnDeliver turnKind = "deliver"
)

// turnOrigin is who queued a turn: the owner; the server, for a child's
// start and a delivery; or the scheduler, for the resume of a task it
// yielded.
type turnOrigin string

const (
	originOwner     turnOrigin = "owner"
	originServer    turnOrigin = "server"
	originScheduler turnOrigin = "scheduler"
)

// queuedTurn is a turn as the owner API reports it once queued.
type queuedTurn struct {
	ID        uint64          `json:"id"`
	TaskID    protocol.TaskID `json:"task_id"`
	Kind      turnKind        `json:"kind"`
	CreatedAt time.Time       `json:"created_at"`
}

var (
	// errNoDaemon reports a task placed on any daemon while none has
	// ever connected.
	errNoDaemon = errors.New("no daemon has connected yet")
	// errNotStarted reports a command that needs the task's daemon to
	// know the task, for a task whose start has not been admitted.
	errNotStarted = errors.New("task has not started")
)

// insertTurn queues a turn of kind for task.
func insertTurn(ctx context.Context, tx *sql.Tx, task protocol.TaskID, kind turnKind, payload json.RawMessage, origin turnOrigin, filler bool, fx *effects) (queuedTurn, error) {
	var stored any
	if payload != nil {
		stored = string(payload)
	}
	turn := queuedTurn{TaskID: task, Kind: kind, CreatedAt: time.Now().UTC()}
	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO turns (task_id, kind, payload, origin, filler, created_at) VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
		string(task), string(kind), stored, string(origin), filler, formatTime(turn.CreatedAt)).Scan(&id)
	if err != nil {
		return queuedTurn{}, fmt.Errorf("queue %s for task %q: %w", kind, task, err)
	}
	turn.ID = uint64(id)
	fx.changed = append(fx.changed, task)
	fx.reschedule = true
	return turn, nil
}

// newTask is a task to record and the turn that starts it.
type newTask struct {
	ID protocol.TaskID
	// Parent is the task that spawned this one; nil for the owner's.
	Parent *protocol.TaskID
	// Daemon is where the first turn goes, or prefers to go for
	// placementParent; it is ignored for placementAny.
	Daemon    protocol.DaemonID
	Placement placement
	Priority  Priority
	Filler    bool
	Start     protocol.StartTask
	Origin    turnOrigin
}

// createTask records task, queued, with its start as a pending turn. Its
// daemon must have been seen; a task placed on any daemon needs one
// daemon to have been seen.
func (s *Store) createTask(ctx context.Context, task newTask) (queuedTurn, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return queuedTurn{}, fmt.Errorf("create task: %w", err)
	}
	defer tx.Rollback()
	if task.Placement == placementAny {
		// The task needs a daemon_id before it is placed; any seen daemon
		// does, and admission replaces it.
		var daemon string
		err := tx.QueryRowContext(ctx, `SELECT id FROM daemons ORDER BY id LIMIT 1`).Scan(&daemon)
		if errors.Is(err, sql.ErrNoRows) {
			return queuedTurn{}, errNoDaemon
		}
		if err != nil {
			return queuedTurn{}, fmt.Errorf("look up daemons: %w", err)
		}
		task.Daemon = protocol.DaemonID(daemon)
	} else {
		var known bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM daemons WHERE id = ?)`, string(task.Daemon)).Scan(&known); err != nil {
			return queuedTurn{}, fmt.Errorf("look up daemon %q: %w", task.Daemon, err)
		}
		if !known {
			return queuedTurn{}, fmt.Errorf("%w: %q", errUnknownDaemon, task.Daemon)
		}
	}
	var fx effects
	turn, err := insertTask(ctx, tx, task, &fx)
	if err != nil {
		return queuedTurn{}, err
	}
	if err := tx.Commit(); err != nil {
		return queuedTurn{}, fmt.Errorf("create task %q: %w", task.ID, err)
	}
	s.publish(&fx)
	return turn, nil
}

// insertTask records task, queued, and queues its start.
func insertTask(ctx context.Context, tx *sql.Tx, task newTask, fx *effects) (queuedTurn, error) {
	payload, err := json.Marshal(task.Start)
	if err != nil {
		return queuedTurn{}, fmt.Errorf("encode start_task: %w", err)
	}
	start := task.Start
	var repo, ref any
	if start.Workspace != nil {
		repo, ref = start.Workspace.Repo, start.Workspace.Ref
	}
	created := formatTime(time.Now().UTC())
	_, err = tx.ExecContext(ctx, `
		INSERT INTO tasks (id, daemon_id, parent_id, state, created_at, last_activity_at, prompt, system_prompt, workspace_repo, workspace_ref, model,
			pause_acknowledge_ns, pause_cleanup_ns, priority, filler, placement)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(task.ID), string(task.Daemon), nullableID(task.Parent), string(TaskQueued), created, created, start.Prompt, start.SystemPrompt, repo, ref,
		start.Model, int64(start.PauseLimits.Acknowledge), int64(start.PauseLimits.Cleanup), string(task.Priority), task.Filler, string(task.Placement))
	if err != nil {
		return queuedTurn{}, fmt.Errorf("create task %q: %w", task.ID, err)
	}
	return insertTurn(ctx, tx, task.ID, turnStart, payload, task.Origin, task.Filler, fx)
}

// queueCommand queues the owner's prompt or resume for task as a turn.
//
// A resume of a task the scheduler yielded, or is yielding, is the
// owner's ordinary turn, not filler: it takes over the resume the
// scheduler queued, if there is one.
func (s *Store) queueCommand(ctx context.Context, task protocol.TaskID, kind turnKind, payload json.RawMessage) (queuedTurn, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return queuedTurn{}, fmt.Errorf("queue %s: %w", kind, err)
	}
	defer tx.Rollback()
	if err := requireTask(ctx, tx, task); err != nil {
		return queuedTurn{}, err
	}
	p, err := loadProgress(ctx, tx, task)
	if err != nil {
		return queuedTurn{}, err
	}
	if p.State.Terminal() {
		return queuedTurn{}, fmt.Errorf("%w: %q is %s", errTaskEnded, task, p.State)
	}
	var filler bool
	if err := tx.QueryRowContext(ctx, `SELECT filler FROM tasks WHERE id = ?`, string(task)).Scan(&filler); err != nil {
		return queuedTurn{}, fmt.Errorf("read task %q: %w", task, err)
	}
	var fx effects
	var turn queuedTurn
	yielded := p.State == TaskYielded || p.State == TaskPausing && p.PausedBy == pauseByScheduler
	if kind == turnResume && yielded {
		turn, err = claimSchedulersResume(ctx, tx, task, &fx)
		if err == nil && turn.ID == 0 {
			turn, err = insertTurn(ctx, tx, task, kind, payload, originOwner, false, &fx)
		}
	} else {
		turn, err = insertTurn(ctx, tx, task, kind, payload, originOwner, filler, &fx)
	}
	if err != nil {
		return queuedTurn{}, err
	}
	if err := tx.Commit(); err != nil {
		return queuedTurn{}, fmt.Errorf("queue %s: %w", kind, err)
	}
	s.publish(&fx)
	return turn, nil
}

// claimSchedulersResume makes the resume the scheduler queued for task,
// if one waits, the owner's ordinary turn, and returns it; it returns a
// zero turn when none waits.
func claimSchedulersResume(ctx context.Context, tx *sql.Tx, task protocol.TaskID, fx *effects) (queuedTurn, error) {
	var id int64
	var created string
	err := tx.QueryRowContext(ctx, `
		UPDATE turns SET origin = ?, filler = 0
		WHERE task_id = ? AND origin = ? AND kind = ? AND admitted_command_id IS NULL
		RETURNING id, created_at`,
		string(originOwner), string(task), string(originScheduler), string(turnResume)).Scan(&id, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return queuedTurn{}, nil
	}
	if err != nil {
		return queuedTurn{}, fmt.Errorf("take over the scheduler's resume of task %q: %w", task, err)
	}
	at, err := parseTime(created)
	if err != nil {
		return queuedTurn{}, fmt.Errorf("read turn %d: %w", id, err)
	}
	fx.changed = append(fx.changed, task)
	fx.reschedule = true
	return queuedTurn{ID: uint64(id), TaskID: task, Kind: turnResume, CreatedAt: at}, nil
}

// queueYieldedResume queues the scheduler's resume of task, which has
// just been yielded, unless a turn other than a delivery already waits
// for it.
func queueYieldedResume(ctx context.Context, tx *sql.Tx, task protocol.TaskID, fx *effects) error {
	var waiting bool
	var filler bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM turns WHERE task_id = ?1 AND kind <> ?2 AND admitted_command_id IS NULL), filler
		FROM tasks WHERE id = ?1`, string(task), string(turnDeliver)).Scan(&waiting, &filler)
	if err != nil {
		return fmt.Errorf("look up the turns of task %q: %w", task, err)
	}
	if waiting {
		return nil
	}
	_, err = insertTurn(ctx, tx, task, turnResume, nil, originScheduler, filler, fx)
	return err
}

// dropSchedulersResume removes the resume the scheduler queued for task,
// once the owner's pause has taken over the scheduler's hold.
func dropSchedulersResume(ctx context.Context, tx *sql.Tx, task protocol.TaskID, fx *effects) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM turns WHERE task_id = ? AND origin = ? AND admitted_command_id IS NULL`,
		string(task), string(originScheduler))
	if err != nil {
		return fmt.Errorf("drop the scheduler's resume of task %q: %w", task, err)
	}
	fx.reschedule = true
	return nil
}
