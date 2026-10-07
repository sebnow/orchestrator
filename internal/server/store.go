// Package server is the orchestrator's central server: it keeps the record
// of daemons, tasks, their events and the commands issued to them, and
// serves the daemon- and owner-facing HTTP APIs
// (docs/adr/2026-10-07-client-protocol.md).
package server

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sebnow/orchestrator/internal/protocol"
)

//go:embed schema.sql
var schema string

// schemaVersion is the version schema.sql creates. A database at another
// version is refused rather than guessed at.
const schemaVersion = 1

// taskPending is the state of a task whose start has been issued.
const taskPending = "pending"

var (
	errUnknownDaemon = errors.New("unknown daemon")
	errUnknownTask   = errors.New("unknown task")
)

// foreignTaskError reports an event for a task that is not assigned to the
// daemon that sent it, including a task the server does not know.
type foreignTaskError struct {
	Task   protocol.TaskID
	Daemon protocol.DaemonID
}

func (e *foreignTaskError) Error() string {
	return fmt.Sprintf("task %q is not assigned to daemon %q", e.Task, e.Daemon)
}

// Store keeps the server's record in one SQLite database.
type Store struct {
	db *sql.DB
}

// OpenStore opens the database at path, creating it and its schema if
// needed.
func OpenStore(ctx context.Context, path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// Write transactions take the write lock when they begin, so two of
	// them never deadlock upgrading a read lock; busy_timeout makes the
	// second wait instead of failing.
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     abs,
		RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL) STRICT`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	var version int
	err = tx.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, schemaVersion); err != nil {
			return fmt.Errorf("record schema version: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read schema version: %w", err)
	case version != schemaVersion:
		return fmt.Errorf("database has schema version %d; this server knows only version %d", version, schemaVersion)
	}
	return tx.Commit()
}

func formatTime(t time.Time) string {
	return t.Format(time.RFC3339Nano)
}

func parseTime(raw string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, raw)
}

// seeDaemon records that daemon made a request now, creating its row on
// first sight. A nil harness leaves the recorded one unchanged.
func seeDaemon(ctx context.Context, tx *sql.Tx, daemon protocol.DaemonID, harness *protocol.Harness) error {
	var name, version any
	if harness != nil {
		name, version = harness.Name, harness.Version
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO daemons (id, first_seen, last_seen, harness_name, harness_version)
		VALUES (?1, ?2, ?2, ?3, ?4)
		ON CONFLICT (id) DO UPDATE SET
			last_seen = excluded.last_seen,
			harness_name = coalesce(excluded.harness_name, daemons.harness_name),
			harness_version = coalesce(excluded.harness_version, daemons.harness_version)`,
		string(daemon), formatTime(time.Now().UTC()), name, version)
	if err != nil {
		return fmt.Errorf("record daemon %q: %w", daemon, err)
	}
	return nil
}

// heldSeqColumn is the highest seq up to which the server holds every
// event of task t: the end of the contiguous prefix starting at 1, or 0.
// That is the lowest stored seq whose successor is missing, provided seq
// 1 is stored.
const heldSeqColumn = `CASE WHEN EXISTS (SELECT 1 FROM events WHERE task_id = t.id AND seq = 1)
	THEN (SELECT min(e.seq) FROM events e WHERE e.task_id = t.id
		AND NOT EXISTS (SELECT 1 FROM events n WHERE n.task_id = t.id AND n.seq = e.seq + 1))
	ELSE 0 END`

// appendEvents stores a daemon's batch of events in one transaction and
// returns the held seq of every task in the batch. An event already
// stored under its task and seq is not stored again; when it differs from
// the stored one it is returned in conflicts and the stored one is kept.
// A batch naming any task not assigned to daemon is refused whole with a
// *foreignTaskError.
func (s *Store) appendEvents(ctx context.Context, daemon protocol.DaemonID, events []protocol.Event) (held map[protocol.TaskID]uint64, conflicts []protocol.Event, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("store events: %w", err)
	}
	defer tx.Rollback()

	var tasks []protocol.TaskID
	for _, event := range events {
		if !slices.Contains(tasks, event.TaskID) {
			tasks = append(tasks, event.TaskID)
		}
	}
	for _, task := range tasks {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT daemon_id FROM tasks WHERE id = ?`, string(task)).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) || err == nil && owner != string(daemon) {
			return nil, nil, &foreignTaskError{Task: task, Daemon: daemon}
		}
		if err != nil {
			return nil, nil, fmt.Errorf("look up task %q: %w", task, err)
		}
	}

	var harness *protocol.Harness
	if len(events) > 0 {
		harness = &events[len(events)-1].Harness
	}
	if err := seeDaemon(ctx, tx, daemon, harness); err != nil {
		return nil, nil, err
	}

	for _, event := range events {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO events (task_id, seq, kind, harness_name, harness_version, time, payload)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (task_id, seq) DO NOTHING`,
			string(event.TaskID), int64(event.Seq), string(event.Kind), event.Harness.Name, event.Harness.Version,
			formatTime(event.Time), string(event.Payload))
		if err != nil {
			return nil, nil, fmt.Errorf("store event %s/%d: %w", event.TaskID, event.Seq, err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return nil, nil, fmt.Errorf("store event %s/%d: %w", event.TaskID, event.Seq, err)
		}
		if inserted == 1 {
			continue
		}
		var kind, harnessName, harnessVersion, eventTime, payload string
		err = tx.QueryRowContext(ctx, `
			SELECT kind, harness_name, harness_version, time, payload FROM events WHERE task_id = ? AND seq = ?`,
			string(event.TaskID), int64(event.Seq)).Scan(&kind, &harnessName, &harnessVersion, &eventTime, &payload)
		if err != nil {
			return nil, nil, fmt.Errorf("read stored event %s/%d: %w", event.TaskID, event.Seq, err)
		}
		same := kind == string(event.Kind) && harnessName == event.Harness.Name && harnessVersion == event.Harness.Version &&
			eventTime == formatTime(event.Time) && payload == string(event.Payload)
		if !same {
			conflicts = append(conflicts, event)
		}
	}

	held = make(map[protocol.TaskID]uint64, len(tasks))
	for _, task := range tasks {
		var seq int64
		if err := tx.QueryRowContext(ctx, `SELECT `+heldSeqColumn+` FROM tasks t WHERE t.id = ?`, string(task)).Scan(&seq); err != nil {
			return nil, nil, fmt.Errorf("read held seq of task %q: %w", task, err)
		}
		held[task] = uint64(seq)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("store events: %w", err)
	}
	return held, conflicts, nil
}

// heldSeqs records that daemon was seen and returns the held seq of every
// task assigned to it.
func (s *Store) heldSeqs(ctx context.Context, daemon protocol.DaemonID) (map[protocol.TaskID]uint64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("read held seqs: %w", err)
	}
	defer tx.Rollback()
	if err := seeDaemon(ctx, tx, daemon, nil); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.id, `+heldSeqColumn+` FROM tasks t WHERE t.daemon_id = ?`, string(daemon))
	if err != nil {
		return nil, fmt.Errorf("read held seqs: %w", err)
	}
	defer rows.Close()
	held := make(map[protocol.TaskID]uint64)
	for rows.Next() {
		var task string
		var seq int64
		if err := rows.Scan(&task, &seq); err != nil {
			return nil, fmt.Errorf("read held seqs: %w", err)
		}
		held[protocol.TaskID(task)] = uint64(seq)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read held seqs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("read held seqs: %w", err)
	}
	return held, nil
}

// createTask records a task assigned to daemon and issues its start_task
// command in the same transaction. The daemon must have been seen.
func (s *Store) createTask(ctx context.Context, daemon protocol.DaemonID, task protocol.TaskID, start protocol.StartTask) (protocol.Command, error) {
	payload, err := json.Marshal(start)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("encode start_task: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("create task: %w", err)
	}
	defer tx.Rollback()
	var known bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM daemons WHERE id = ?)`, string(daemon)).Scan(&known); err != nil {
		return protocol.Command{}, fmt.Errorf("look up daemon %q: %w", daemon, err)
	}
	if !known {
		return protocol.Command{}, fmt.Errorf("%w: %q", errUnknownDaemon, daemon)
	}
	var repo, ref any
	if start.Workspace != nil {
		repo, ref = start.Workspace.Repo, start.Workspace.Ref
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO tasks (id, daemon_id, state, created_at, prompt, system_prompt, workspace_repo, workspace_ref, model, pause_acknowledge_ns, pause_cleanup_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(task), string(daemon), taskPending, formatTime(time.Now().UTC()), start.Prompt, start.SystemPrompt, repo, ref,
		start.Model, int64(start.PauseLimits.Acknowledge), int64(start.PauseLimits.Cleanup))
	if err != nil {
		return protocol.Command{}, fmt.Errorf("create task %q: %w", task, err)
	}
	command, err := insertCommand(ctx, tx, daemon, task, protocol.CommandStartTask, payload)
	if err != nil {
		return protocol.Command{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.Command{}, fmt.Errorf("create task %q: %w", task, err)
	}
	return command, nil
}

// issueCommand appends a command for task to the log of the daemon the
// task is assigned to. A nil payload stores none.
func (s *Store) issueCommand(ctx context.Context, task protocol.TaskID, kind protocol.CommandKind, payload json.RawMessage) (protocol.Command, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("issue command: %w", err)
	}
	defer tx.Rollback()
	var daemon string
	err = tx.QueryRowContext(ctx, `SELECT daemon_id FROM tasks WHERE id = ?`, string(task)).Scan(&daemon)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.Command{}, fmt.Errorf("%w: %q", errUnknownTask, task)
	}
	if err != nil {
		return protocol.Command{}, fmt.Errorf("look up task %q: %w", task, err)
	}
	command, err := insertCommand(ctx, tx, protocol.DaemonID(daemon), task, kind, payload)
	if err != nil {
		return protocol.Command{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.Command{}, fmt.Errorf("issue command: %w", err)
	}
	return command, nil
}

func insertCommand(ctx context.Context, tx *sql.Tx, daemon protocol.DaemonID, task protocol.TaskID, kind protocol.CommandKind, payload json.RawMessage) (protocol.Command, error) {
	command := protocol.Command{DaemonID: daemon, TaskID: task, Kind: kind, Time: time.Now().UTC(), Payload: payload}
	var stored any
	if payload != nil {
		stored = string(payload)
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO commands (daemon_id, task_id, kind, time, payload) VALUES (?, ?, ?, ?, ?) RETURNING id`,
		string(daemon), string(task), string(kind), formatTime(command.Time), stored).Scan(&id)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("issue %s for task %q: %w", kind, task, err)
	}
	command.ID = uint64(id)
	return command, nil
}

// commandsAfter returns daemon's commands with an id greater than after,
// in id order.
func (s *Store) commandsAfter(ctx context.Context, daemon protocol.DaemonID, after uint64) ([]protocol.Command, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, kind, time, payload FROM commands WHERE daemon_id = ? AND id > ? ORDER BY id`,
		string(daemon), int64(after))
	if err != nil {
		return nil, fmt.Errorf("read commands: %w", err)
	}
	defer rows.Close()
	var commands []protocol.Command
	for rows.Next() {
		var id int64
		var task, kind, issued string
		var payload []byte
		if err := rows.Scan(&id, &task, &kind, &issued, &payload); err != nil {
			return nil, fmt.Errorf("read commands: %w", err)
		}
		at, err := parseTime(issued)
		if err != nil {
			return nil, fmt.Errorf("read command %d: %w", id, err)
		}
		commands = append(commands, protocol.Command{
			ID: uint64(id), DaemonID: daemon, TaskID: protocol.TaskID(task), Kind: protocol.CommandKind(kind), Time: at, Payload: payload,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read commands: %w", err)
	}
	return commands, nil
}

// eventsAfter returns task's stored events with a seq greater than after,
// in seq order.
func (s *Store) eventsAfter(ctx context.Context, task protocol.TaskID, after uint64) ([]protocol.Event, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer tx.Rollback()
	var known bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = ?)`, string(task)).Scan(&known); err != nil {
		return nil, fmt.Errorf("look up task %q: %w", task, err)
	}
	if !known {
		return nil, fmt.Errorf("%w: %q", errUnknownTask, task)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT seq, kind, harness_name, harness_version, time, payload FROM events WHERE task_id = ? AND seq > ? ORDER BY seq`,
		string(task), int64(after))
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()
	events := []protocol.Event{}
	for rows.Next() {
		var seq int64
		var kind, harnessName, harnessVersion, eventTime string
		var payload []byte
		if err := rows.Scan(&seq, &kind, &harnessName, &harnessVersion, &eventTime, &payload); err != nil {
			return nil, fmt.Errorf("read events: %w", err)
		}
		at, err := parseTime(eventTime)
		if err != nil {
			return nil, fmt.Errorf("read event %s/%d: %w", task, seq, err)
		}
		events = append(events, protocol.Event{
			TaskID:  task,
			Seq:     uint64(seq),
			Kind:    protocol.Kind(kind),
			Harness: protocol.Harness{Name: harnessName, Version: harnessVersion},
			Time:    at,
			Payload: payload,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	return events, nil
}
