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

// migrations[n] takes the schema from version n+1 to version n+2. A new
// database gets schema.sql, which is version 1, and then every migration,
// so new and migrated databases have the same shape.
var migrations = [...]string{
	// Version 2 keeps what the task list shows without reading events.
	`ALTER TABLE tasks ADD COLUMN last_activity_at TEXT NOT NULL DEFAULT '';
	UPDATE tasks SET last_activity_at = created_at;
	ALTER TABLE tasks ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0;`,
	// Version 3 keeps the hash of the owner's token, and the hash and
	// expiry, in Unix seconds, of each login session
	// (docs/adr/2026-10-08-owner-authentication.md).
	`CREATE TABLE settings (name TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
	CREATE TABLE sessions (id_sha256 TEXT PRIMARY KEY, expires_at INTEGER NOT NULL) STRICT;`,
	// Version 4 records each task's parent and the messages agents send
	// each other (docs/adr/2026-10-08-inbox-delivery.md). A message's
	// from_task is NULL for the server's notice that a child ended, and
	// about_task names that child; delivered_command_id is the prompt that
	// delivered it, NULL while it waits in the recipient's inbox.
	`ALTER TABLE tasks ADD COLUMN parent_id TEXT REFERENCES tasks (id);
	CREATE INDEX tasks_by_parent ON tasks (parent_id);
	CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		from_task TEXT REFERENCES tasks (id),
		to_task TEXT NOT NULL REFERENCES tasks (id),
		about_task TEXT REFERENCES tasks (id),
		text TEXT NOT NULL,
		created_at TEXT NOT NULL,
		delivered_command_id INTEGER REFERENCES commands (id),
		CHECK ((from_task IS NULL) <> (about_task IS NULL))
	) STRICT;
	CREATE INDEX messages_by_recipient ON messages (to_task, delivered_command_id);
	CREATE INDEX messages_by_sender ON messages (from_task);`,
}

// schemaVersion is the version this server migrates databases to. A
// database at a later version is refused rather than guessed at.
const schemaVersion = 1 + len(migrations)

var (
	errUnknownDaemon = errors.New("unknown daemon")
	errUnknownTask   = errors.New("unknown task")
	// errTaskEnded reports a command for a task that is stopped or failed.
	errTaskEnded = errors.New("task has ended")
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
	// published, when set, hears of every committed change that issued a
	// command or changed a task's transcript, so that open streams can be
	// woken.
	published func(effects)
}

// effects are what a write transaction did that open streams must hear
// of once it commits. A message or a child changes the transcript of a
// task other than the one the transaction was for, so changed lists such
// tasks.
type effects struct {
	issued  []protocol.Command
	changed []protocol.TaskID
}

// publish tells the server of fx after its transaction committed.
func (s *Store) publish(fx *effects) {
	if s.published != nil && (len(fx.issued) > 0 || len(fx.changed) > 0) {
		s.published(*fx)
	}
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (1)`); err != nil {
			return fmt.Errorf("record schema version: %w", err)
		}
		version = 1
	case err != nil:
		return fmt.Errorf("read schema version: %w", err)
	case version < 1 || version > schemaVersion:
		return fmt.Errorf("database has schema version %d; this server knows versions 1 to %d", version, schemaVersion)
	}
	if version == schemaVersion {
		return tx.Commit()
	}
	for ; version < schemaVersion; version++ {
		if _, err := tx.ExecContext(ctx, migrations[version-1]); err != nil {
			return fmt.Errorf("migrate schema to version %d: %w", version+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, schemaVersion); err != nil {
		return fmt.Errorf("record schema version: %w", err)
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

// recordSeen records that daemon made a request now.
func (s *Store) recordSeen(ctx context.Context, daemon protocol.DaemonID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record daemon %q: %w", daemon, err)
	}
	defer tx.Rollback()
	if err := seeDaemon(ctx, tx, daemon, nil); err != nil {
		return err
	}
	return tx.Commit()
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
//
// A task the batch leaves finished is sent the messages waiting in its
// inbox, and the parent of a task the batch ends is told.
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
	progresses := make(map[protocol.TaskID]*progress, len(tasks))
	before := make(map[protocol.TaskID]TaskState, len(tasks))
	for _, task := range tasks {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT daemon_id FROM tasks WHERE id = ?`, string(task)).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) || err == nil && owner != string(daemon) {
			return nil, nil, &foreignTaskError{Task: task, Daemon: daemon}
		}
		if err != nil {
			return nil, nil, fmt.Errorf("look up task %q: %w", task, err)
		}
		p, err := loadProgress(ctx, tx, task)
		if err != nil {
			return nil, nil, err
		}
		progresses[task] = &p
		before[task] = p.State
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
			stopped := false
			if event.Kind == protocol.KindHarnessExited {
				if stopped, err = stopIssued(ctx, tx, event.TaskID); err != nil {
					return nil, nil, err
				}
			}
			progresses[event.TaskID].seeEvent(event, stopped)
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

	for task, p := range progresses {
		if err := saveProgress(ctx, tx, task, *p); err != nil {
			return nil, nil, err
		}
	}
	// Each step below reads the progress it needs afresh, because telling
	// a parent can issue a command to a task of this batch.
	var fx effects
	for _, task := range tasks {
		state := progresses[task].State
		if !before[task].Terminal() && state.Terminal() {
			if err := notifyParent(ctx, tx, task, state, &fx); err != nil {
				return nil, nil, err
			}
		}
		if state == TaskFinished {
			if _, err := deliverWaiting(ctx, tx, task, &fx); err != nil {
				return nil, nil, err
			}
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
	s.publish(&fx)
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
	var fx effects
	command, err := insertTask(ctx, tx, daemon, task, nil, start, &fx)
	if err != nil {
		return protocol.Command{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.Command{}, fmt.Errorf("create task %q: %w", task, err)
	}
	s.publish(&fx)
	return command, nil
}

// insertTask records a task assigned to daemon, a child of parent when
// that is set, and issues its start_task command.
func insertTask(ctx context.Context, tx *sql.Tx, daemon protocol.DaemonID, task protocol.TaskID, parent *protocol.TaskID, start protocol.StartTask, fx *effects) (protocol.Command, error) {
	payload, err := json.Marshal(start)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("encode start_task: %w", err)
	}
	var repo, ref any
	if start.Workspace != nil {
		repo, ref = start.Workspace.Repo, start.Workspace.Ref
	}
	created := formatTime(time.Now().UTC())
	_, err = tx.ExecContext(ctx, `
		INSERT INTO tasks (id, daemon_id, parent_id, state, created_at, last_activity_at, prompt, system_prompt, workspace_repo, workspace_ref, model, pause_acknowledge_ns, pause_cleanup_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(task), string(daemon), nullableID(parent), string(TaskPending), created, created, start.Prompt, start.SystemPrompt, repo, ref,
		start.Model, int64(start.PauseLimits.Acknowledge), int64(start.PauseLimits.Cleanup))
	if err != nil {
		return protocol.Command{}, fmt.Errorf("create task %q: %w", task, err)
	}
	return insertCommand(ctx, tx, daemon, task, protocol.CommandStartTask, payload, fx)
}

// nullableID is id as a query argument: NULL when id is nil.
func nullableID(id *protocol.TaskID) any {
	if id == nil {
		return nil
	}
	return string(*id)
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
	var fx effects
	command, err := insertCommand(ctx, tx, protocol.DaemonID(daemon), task, kind, payload, &fx)
	if err != nil {
		return protocol.Command{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.Command{}, fmt.Errorf("issue command: %w", err)
	}
	s.publish(&fx)
	return command, nil
}

// insertCommand appends a command to the log and folds it into the task's
// progress. A task that is stopped or failed takes no command. When the
// command ends the task, its parent is told.
func insertCommand(ctx context.Context, tx *sql.Tx, daemon protocol.DaemonID, task protocol.TaskID, kind protocol.CommandKind, payload json.RawMessage, fx *effects) (protocol.Command, error) {
	p, err := loadProgress(ctx, tx, task)
	if err != nil {
		return protocol.Command{}, err
	}
	if p.State.Terminal() {
		return protocol.Command{}, fmt.Errorf("%w: %q is %s", errTaskEnded, task, p.State)
	}
	command := protocol.Command{DaemonID: daemon, TaskID: task, Kind: kind, Time: time.Now().UTC(), Payload: payload}
	var stored any
	if payload != nil {
		stored = string(payload)
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO commands (daemon_id, task_id, kind, time, payload) VALUES (?, ?, ?, ?, ?) RETURNING id`,
		string(daemon), string(task), string(kind), formatTime(command.Time), stored).Scan(&id)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("issue %s for task %q: %w", kind, task, err)
	}
	command.ID = uint64(id)
	p.seeCommand(command)
	if err := saveProgress(ctx, tx, task, p); err != nil {
		return protocol.Command{}, err
	}
	fx.issued = append(fx.issued, command)
	if p.State.Terminal() {
		if err := notifyParent(ctx, tx, task, p.State, fx); err != nil {
			return protocol.Command{}, err
		}
	}
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
	if err := requireTask(ctx, tx, task); err != nil {
		return nil, err
	}
	return queryEvents(ctx, tx, task, after)
}

// history is everything recorded about a task: its events in seq order;
// its commands, and the start_task of each of its children, in id order;
// and the messages it sent or was sent, in id order.
type history struct {
	events   []protocol.Event
	commands []protocol.Command
	messages []storedMessage
}

// storedMessage is a message as the messages table keeps it. Exactly one
// of From and About is set: From for an agent's message, About for the
// server's notice that a child ended, AboutState being that child's
// state. DeliveredBy is the id of the prompt that delivered it, 0 while
// it waits.
type storedMessage struct {
	ID          uint64
	From        *protocol.TaskID
	To          protocol.TaskID
	About       *protocol.TaskID
	AboutState  TaskState
	Text        string
	CreatedAt   time.Time
	DeliveredBy uint64
}

// taskHistory returns task's history, read in one transaction.
func (s *Store) taskHistory(ctx context.Context, task protocol.TaskID) (history, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return history{}, fmt.Errorf("read task history: %w", err)
	}
	defer tx.Rollback()
	if err := requireTask(ctx, tx, task); err != nil {
		return history{}, err
	}
	var h history
	if h.events, err = queryEvents(ctx, tx, task, 0); err != nil {
		return history{}, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, daemon_id, task_id, kind, time, payload FROM commands
		WHERE task_id = ?1 OR kind = ?2 AND task_id IN (SELECT id FROM tasks WHERE parent_id = ?1)
		ORDER BY id`, string(task), string(protocol.CommandStartTask))
	if err != nil {
		return history{}, fmt.Errorf("read commands of task %q: %w", task, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var daemon, owner, kind, issued string
		var payload []byte
		if err := rows.Scan(&id, &daemon, &owner, &kind, &issued, &payload); err != nil {
			return history{}, fmt.Errorf("read commands of task %q: %w", task, err)
		}
		at, err := parseTime(issued)
		if err != nil {
			return history{}, fmt.Errorf("read command %d: %w", id, err)
		}
		h.commands = append(h.commands, protocol.Command{
			ID: uint64(id), DaemonID: protocol.DaemonID(daemon), TaskID: protocol.TaskID(owner), Kind: protocol.CommandKind(kind), Time: at, Payload: payload,
		})
	}
	if err := rows.Err(); err != nil {
		return history{}, fmt.Errorf("read commands of task %q: %w", task, err)
	}
	rows.Close()
	if h.messages, err = queryMessages(ctx, tx, task); err != nil {
		return history{}, err
	}
	return h, nil
}

// queryMessages returns the messages task sent or was sent, in id order.
func queryMessages(ctx context.Context, tx *sql.Tx, task protocol.TaskID) ([]storedMessage, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.from_task, m.to_task, m.about_task, coalesce(a.state, ''), m.text, m.created_at, coalesce(m.delivered_command_id, 0)
		FROM messages m LEFT JOIN tasks a ON a.id = m.about_task
		WHERE m.from_task = ?1 OR m.to_task = ?1 ORDER BY m.id`, string(task))
	if err != nil {
		return nil, fmt.Errorf("read messages of task %q: %w", task, err)
	}
	defer rows.Close()
	var messages []storedMessage
	for rows.Next() {
		var message storedMessage
		var id, delivered int64
		var from, about sql.NullString
		var to, state, created string
		if err := rows.Scan(&id, &from, &to, &about, &state, &message.Text, &created, &delivered); err != nil {
			return nil, fmt.Errorf("read messages of task %q: %w", task, err)
		}
		message.ID, message.To, message.AboutState, message.DeliveredBy = uint64(id), protocol.TaskID(to), TaskState(state), uint64(delivered)
		if from.Valid {
			sender := protocol.TaskID(from.String)
			message.From = &sender
		}
		if about.Valid {
			child := protocol.TaskID(about.String)
			message.About = &child
		}
		if message.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("read message %d: %w", id, err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read messages of task %q: %w", task, err)
	}
	return messages, nil
}

// requireTask returns errUnknownTask when task is not recorded.
func requireTask(ctx context.Context, tx *sql.Tx, task protocol.TaskID) error {
	var known bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = ?)`, string(task)).Scan(&known); err != nil {
		return fmt.Errorf("look up task %q: %w", task, err)
	}
	if !known {
		return fmt.Errorf("%w: %q", errUnknownTask, task)
	}
	return nil
}

// queryEvents returns task's events with a seq greater than after, in seq
// order.
func queryEvents(ctx context.Context, tx *sql.Tx, task protocol.TaskID, after uint64) ([]protocol.Event, error) {
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

// daemonSummary is a daemon as the daemon list shows it.
type daemonSummary struct {
	ID       protocol.DaemonID
	LastSeen time.Time
	// Harness is the one named by the latest event the daemon sent; nil
	// until it sends one.
	Harness *protocol.Harness
	// Quota is the newest usage-limit reading among the daemon's events,
	// observed at QuotaAt; nil when there is none.
	Quota   *protocol.QuotaObserved
	QuotaAt time.Time
}

// daemons returns every daemon that has been seen, by id, each with its
// newest quota reading.
//
// Stored times keep the daemon's zone offset and trim trailing zeros, so
// they do not sort as text; julianday compares the instants, to the
// millisecond, and the row id, which follows storage order, breaks ties.
func (s *Store) daemons(ctx context.Context) ([]daemonSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH readings AS (
			SELECT t.daemon_id, e.time, e.payload,
				row_number() OVER (PARTITION BY t.daemon_id ORDER BY julianday(e.time) DESC, e.rowid DESC) AS newest
			FROM events e JOIN tasks t ON t.id = e.task_id
			WHERE e.kind = ?)
		SELECT d.id, d.last_seen, d.harness_name, d.harness_version, r.time, r.payload
		FROM daemons d LEFT JOIN readings r ON r.daemon_id = d.id AND r.newest = 1
		ORDER BY d.id`, string(protocol.KindQuotaObserved))
	if err != nil {
		return nil, fmt.Errorf("read daemons: %w", err)
	}
	defer rows.Close()
	var daemons []daemonSummary
	for rows.Next() {
		var id, lastSeen string
		var harnessName, harnessVersion, quotaTime, quotaPayload sql.NullString
		if err := rows.Scan(&id, &lastSeen, &harnessName, &harnessVersion, &quotaTime, &quotaPayload); err != nil {
			return nil, fmt.Errorf("read daemons: %w", err)
		}
		daemon := daemonSummary{ID: protocol.DaemonID(id)}
		if daemon.LastSeen, err = parseTime(lastSeen); err != nil {
			return nil, fmt.Errorf("read daemon %q: %w", id, err)
		}
		if harnessName.Valid {
			daemon.Harness = &protocol.Harness{Name: harnessName.String, Version: harnessVersion.String}
		}
		if quotaPayload.Valid {
			// A reading that does not decode is left out rather than
			// failing the whole list; the raw event stays readable.
			var quota protocol.QuotaObserved
			at, err := parseTime(quotaTime.String)
			if err == nil && json.Unmarshal([]byte(quotaPayload.String), &quota) == nil {
				daemon.Quota, daemon.QuotaAt = &quota, at
			}
		}
		daemons = append(daemons, daemon)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read daemons: %w", err)
	}
	return daemons, nil
}
