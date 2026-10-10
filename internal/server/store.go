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
	"strings"
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
	// Version 5 lets the server schedule turns
	// (docs/adr/2026-10-08-scheduling.md). A task gains its priority, its
	// filler flag, how its first turn is placed, and who asked for the
	// pause under way or in effect; a daemon gains its slot count, NULL
	// for the server's default. A turn waits in turns until the scheduler
	// admits it by issuing its command, admitted_command_id; reason says
	// why it waits. Quota readings are looked up by kind.
	`ALTER TABLE tasks ADD COLUMN priority TEXT NOT NULL DEFAULT 'normal' CHECK (priority IN ('low', 'normal', 'high'));
	ALTER TABLE tasks ADD COLUMN filler INTEGER NOT NULL DEFAULT 0 CHECK (filler IN (0, 1));
	ALTER TABLE tasks ADD COLUMN placement TEXT NOT NULL DEFAULT 'bound' CHECK (placement IN ('bound', 'any', 'parent'));
	ALTER TABLE tasks ADD COLUMN pause_origin TEXT CHECK (pause_origin IN ('owner', 'scheduler'));
	ALTER TABLE daemons ADD COLUMN slots INTEGER CHECK (slots >= 0);
	CREATE TABLE turns (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL REFERENCES tasks (id),
		kind TEXT NOT NULL CHECK (kind IN ('start_task', 'prompt', 'resume', 'deliver')),
		payload TEXT,
		origin TEXT NOT NULL CHECK (origin IN ('owner', 'server', 'scheduler')),
		filler INTEGER NOT NULL CHECK (filler IN (0, 1)),
		created_at TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		admitted_command_id INTEGER REFERENCES commands (id)
	) STRICT;
	CREATE INDEX turns_by_task ON turns (task_id, admitted_command_id);
	CREATE INDEX events_by_kind ON events (kind);`,
	// Version 6 records when the owner dismissed a stopped or failed task
	// from the dashboard's lists; NULL while it is not dismissed.
	`ALTER TABLE tasks ADD COLUMN dismissed_at TEXT CHECK (dismissed_at IS NULL OR state IN ('stopped', 'failed'));`,
	// Version 7 lets the server move the tasks of a lost daemon
	// (docs/adr/2026-10-08-daemon-loss.md). A daemon gains when it was
	// declared lost, NULL while it is not. A task gains the seq that the
	// events from its latest daemon are stored after: each daemon numbers
	// a task's events from 1, so they are stored as their seq plus
	// seq_base.
	`ALTER TABLE daemons ADD COLUMN lost_at TEXT;
	ALTER TABLE tasks ADD COLUMN seq_base INTEGER NOT NULL DEFAULT 0 CHECK (seq_base >= 0);`,
	// Version 8 records which answer_permission commands the server's
	// permission policy issued rather than the owner
	// (docs/adr/2026-10-08-permission-policy.md).
	`ALTER TABLE commands ADD COLUMN by_policy INTEGER NOT NULL DEFAULT 0 CHECK (by_policy IN (0, 1));`,
	// Version 9 marks the messages the server sent for a child that ended
	// its turn without sending one: its final reply, handed back to its
	// parent (docs/adr/2026-10-09-agents-and-placement.md).
	`ALTER TABLE messages ADD COLUMN hand_back INTEGER NOT NULL DEFAULT 0 CHECK (hand_back IN (0, 1));`,
	// Version 10 keeps the owner's agent definitions, and the agent each
	// task was started as, NULL for none
	// (docs/adr/2026-10-09-agents-and-placement.md). An agent's tools are a
	// JSON array and its requires a JSON object; its pause limits are both
	// NULL when it leaves them to the task defaults.
	`CREATE TABLE agents (
		name TEXT PRIMARY KEY,
		description TEXT NOT NULL,
		system_prompt TEXT NOT NULL,
		model TEXT NOT NULL,
		tools TEXT NOT NULL,
		pause_acknowledge_ns INTEGER CHECK (pause_acknowledge_ns > 0),
		pause_cleanup_ns INTEGER CHECK (pause_cleanup_ns > 0),
		priority TEXT NOT NULL CHECK (priority IN ('low', 'normal', 'high')),
		filler INTEGER NOT NULL CHECK (filler IN (0, 1)),
		requires TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		CHECK ((pause_acknowledge_ns IS NULL) = (pause_cleanup_ns IS NULL))
	) STRICT;
	ALTER TABLE tasks ADD COLUMN agent TEXT REFERENCES agents (name);
	CREATE INDEX tasks_by_agent ON tasks (agent);`,
	// Version 11 keeps the gateway tools each task's start allows, as a
	// JSON array, NULL for every tool as before agents.
	`ALTER TABLE tasks ADD COLUMN tools TEXT;`,
	// Version 12 keeps each daemon's labels, set by the owner, and the
	// facts it last reported, each a JSON object of strings
	// (docs/adr/2026-10-09-agents-and-placement.md).
	`ALTER TABLE daemons ADD COLUMN labels TEXT NOT NULL DEFAULT '{}';
	ALTER TABLE daemons ADD COLUMN facts TEXT NOT NULL DEFAULT '{}';`,
	// Version 13 keeps the labels each task requires of its daemon, a JSON
	// object of strings.
	`ALTER TABLE tasks ADD COLUMN requires TEXT NOT NULL DEFAULT '{}';`,
	// Version 14 keeps the cost of each task's earlier harness sessions,
	// to which the current session's running total is added. A task that
	// moved before it keeps the highest total it had.
	`ALTER TABLE tasks ADD COLUMN cost_base REAL NOT NULL DEFAULT 0;`,
	// Version 15 keeps the owner's projects, the project each task belongs
	// to, NULL for none, and each task's purpose, empty for none
	// (docs/adr/2026-10-10-projects-and-lineage.md). A project's repo and
	// ref are both NULL when it has no repository, and its default_agent is
	// NULL for none.
	`CREATE TABLE projects (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		instructions TEXT NOT NULL,
		repo TEXT,
		ref TEXT,
		default_agent TEXT REFERENCES agents (name),
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		CHECK ((repo IS NULL) = (ref IS NULL))
	) STRICT;
	CREATE INDEX projects_by_default_agent ON projects (default_agent);
	ALTER TABLE tasks ADD COLUMN project TEXT REFERENCES projects (id);
	CREATE INDEX tasks_by_project ON tasks (project);
	ALTER TABLE tasks ADD COLUMN purpose TEXT NOT NULL DEFAULT '';`,
	// Version 16 keeps whether a stop issued to a task's process waits
	// for that process's exit, which leaves the task stopped. Until now
	// any stop ever issued did, since a stopped task never ran again.
	`ALTER TABLE tasks ADD COLUMN stop_pending INTEGER NOT NULL DEFAULT 0 CHECK (stop_pending IN (0, 1));
	UPDATE tasks SET stop_pending = 1
	WHERE state IN ('pending', 'running', 'awaiting_permission', 'pausing')
		AND EXISTS (SELECT 1 FROM commands c WHERE c.task_id = tasks.id AND c.kind = 'stop');`,
	// Version 17 keeps the task each task continues, its predecessor,
	// NULL for none: the owner started it from the predecessor's final
	// reply. A predecessor is not a parent.
	`ALTER TABLE tasks ADD COLUMN continues TEXT REFERENCES tasks (id);
	CREATE INDEX tasks_by_continues ON tasks (continues);`,
	// Version 18 replaces each agent's model with its models, a JSON
	// array of the acceptable models, most preferred first; an agent's
	// model becomes its only one
	// (docs/adr/2026-10-10-agent-models-and-capacity.md). A task gains
	// the models placement chooses its model from, a JSON array, NULL
	// when its model was given.
	`ALTER TABLE agents ADD COLUMN models TEXT NOT NULL DEFAULT '[]';
	UPDATE agents SET models = json_array(model) WHERE model <> '';
	ALTER TABLE agents DROP COLUMN model;
	ALTER TABLE tasks ADD COLUMN models TEXT;`,
	// Version 19 keeps each agent's effort and each task's, empty for the
	// harness's default
	// (docs/adr/2026-10-10-agent-models-and-capacity.md).
	`ALTER TABLE agents ADD COLUMN effort TEXT NOT NULL DEFAULT '' CHECK (effort IN ('', 'low', 'medium', 'high', 'max'));
	ALTER TABLE tasks ADD COLUMN effort TEXT NOT NULL DEFAULT '';`,
	// Version 20 keeps the classes of harness tools each agent's tasks
	// are restricted to, a JSON array, empty for every tool, and each
	// task's, NULL for every tool
	// (docs/adr/2026-10-10-agent-models-and-capacity.md).
	`ALTER TABLE agents ADD COLUMN tool_classes TEXT NOT NULL DEFAULT '[]';
	ALTER TABLE tasks ADD COLUMN tool_classes TEXT;`,
	// Version 21 drops each daemon's slot count, which nothing set: a
	// daemon's capacity is now its slots fact, capped by the owner's
	// slots label (docs/adr/2026-10-10-agent-models-and-capacity.md).
	`ALTER TABLE daemons DROP COLUMN slots;`,
	// Version 22 lets a command be the daemon's rather than a task's,
	// with task_id NULL: the login commands
	// (docs/adr/2026-10-10-harness-login.md). SQLite cannot drop a NOT
	// NULL constraint, so the table is rebuilt. messages and turns
	// reference commands, so this migration runs with foreign keys off
	// (noForeignKeys); the rows that reference commands are checked by
	// PRAGMA foreign_key_check once the new table has its name, and its
	// AUTOINCREMENT sequence carries on from the old one's, so that no id
	// a daemon has applied is issued again.
	`CREATE TABLE commands_new (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		daemon_id TEXT NOT NULL REFERENCES daemons (id),
		task_id TEXT REFERENCES tasks (id),
		kind TEXT NOT NULL,
		time TEXT NOT NULL,
		payload TEXT,
		by_policy INTEGER NOT NULL DEFAULT 0 CHECK (by_policy IN (0, 1)),
		CHECK ((task_id IS NULL) = (kind IN ('login', 'login_code')))
	) STRICT;
	INSERT INTO commands_new (id, daemon_id, task_id, kind, time, payload, by_policy)
		SELECT id, daemon_id, task_id, kind, time, payload, by_policy FROM commands;
	INSERT INTO sqlite_sequence (name, seq) SELECT 'commands_new', 0
		WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'commands_new');
	UPDATE sqlite_sequence SET seq = max(seq, coalesce((SELECT seq FROM sqlite_sequence WHERE name = 'commands'), 0))
		WHERE name = 'commands_new';
	DROP TABLE commands;
	ALTER TABLE commands_new RENAME TO commands;
	CREATE INDEX commands_by_daemon ON commands (daemon_id, id);`,
	// Version 23 keeps the budget each quota reading belongs to: the
	// harness and the account the sending daemon reported when the server
	// stored it, or the daemon when it reported no account
	// (docs/adr/2026-10-10-harness-login.md). All three are NULL for
	// other events. Readings stored before are the daemon's of their
	// task, as no daemon reported an account then.
	`ALTER TABLE events ADD COLUMN budget_harness TEXT;
	ALTER TABLE events ADD COLUMN budget_account TEXT;
	ALTER TABLE events ADD COLUMN budget_daemon TEXT;
	UPDATE events SET budget_harness = harness_name,
		budget_daemon = (SELECT t.daemon_id FROM tasks t WHERE t.id = events.task_id)
	WHERE kind = 'quota_observed';
	CREATE INDEX events_by_budget ON events (kind, budget_harness, budget_account, budget_daemon);`,
	// Version 24 keeps the enrolment tokens not yet used: the SHA-256 of
	// each one's secret, the daemon it enrols, which need not have been
	// seen, and when it expires, in Unix seconds
	// (docs/adr/2026-10-10-vps-provisioning.md).
	`CREATE TABLE enrolment_tokens (
		secret_sha256 TEXT PRIMARY KEY,
		daemon_id TEXT NOT NULL,
		expires_at INTEGER NOT NULL
	) STRICT;`,
	// Version 25 keeps the VPSes the server provisioned, by the daemon
	// each runs: its server's id at Hetzner, NULL until Hetzner answers,
	// its server type and location, when it was created, and its state
	// (docs/adr/2026-10-10-vps-provisioning.md).
	`CREATE TABLE vpses (
		daemon_id TEXT PRIMARY KEY,
		server_id INTEGER,
		server_type TEXT NOT NULL,
		location TEXT NOT NULL,
		created_at TEXT NOT NULL,
		state TEXT NOT NULL CHECK (state IN ('creating', 'enrolled', 'destroying', 'destroyed'))
	) STRICT;`,
	// Version 26 keeps the last backup attempt, in its one row: when it
	// was made, the copy's size in bytes, its file, empty when no copy was
	// written, where it was uploaded, empty when it was not, and why it
	// failed, empty when it did not
	// (docs/adr/2026-10-10-sqlite-backups.md).
	`CREATE TABLE backups (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		at TEXT NOT NULL,
		size INTEGER NOT NULL,
		file TEXT NOT NULL,
		uploaded_to TEXT NOT NULL,
		error TEXT NOT NULL
	) STRICT;`,
}

// schemaVersion is the version this server migrates databases to. A
// database at a later version is refused rather than guessed at.
const schemaVersion = 1 + len(migrations)

// noForeignKeys marks, by the version it takes the schema to, a migration
// that rebuilds a table other tables reference. SQLite's DROP TABLE of the
// old table counts an implicit delete against every row that references
// it, which the later rename never satisfies, so such a migration fails
// with foreign keys on even though the rebuilt table has the same rows
// (https://www.sqlite.org/lang_altertable.html, "Making Other Kinds Of
// Table Schema Changes"). PRAGMA foreign_keys cannot change inside a
// transaction, so these run on their own connection; see
// migrateWithoutForeignKeys.
var noForeignKeys = map[int]bool{22: true}

var (
	errUnknownDaemon = errors.New("unknown daemon")
	errUnknownTask   = errors.New("unknown task")
	// errTaskEnded reports a command other than a follow-up for a task
	// that is stopped or failed, which has no process to take it.
	errTaskEnded = errors.New("task has ended")
	// errDismissed reports a command or follow-up for a task the owner
	// dismissed, which takes none.
	errDismissed = errors.New("task was dismissed")
	// errNotEnded reports a dismissal of a task that is not stopped or
	// failed.
	errNotEnded = errors.New("task has not ended")
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

// movedTaskError reports an event for a task the server moved from the
// daemon that sent it, after declaring that daemon lost
// (docs/adr/2026-10-08-daemon-loss.md).
type movedTaskError struct {
	Task   protocol.TaskID
	Daemon protocol.DaemonID
}

func (e *movedTaskError) Error() string {
	return fmt.Sprintf("task %q was moved from daemon %q, which the server declared lost", e.Task, e.Daemon)
}

// Store keeps the server's record in one SQLite database.
type Store struct {
	db *sql.DB
	// now stamps when a daemon was last seen. The server sets it to the
	// scheduler's clock, which decides when a daemon is lost.
	now func() time.Time
	// published, when set, hears of every committed change that issued a
	// command or changed a task's transcript, so that open streams can be
	// woken.
	published func(effects)
	// permissions, when set, decides each permission request as it is
	// stored; when nil, every request waits for the owner.
	permissions Policy
}

// effects are what a write transaction did that open streams and the
// scheduler must hear of once it commits. A message or a child changes
// the transcript of a task other than the one the transaction was for,
// so changed lists such tasks. reschedule is set when the transaction may
// have changed what the scheduler can admit: it queued a turn, changed a
// task's state, or stored a quota reading.
type effects struct {
	issued     []protocol.Command
	changed    []protocol.TaskID
	reschedule bool
}

// publish tells the server of fx after its transaction committed.
func (s *Store) publish(fx *effects) {
	if s.published != nil && (len(fx.issued) > 0 || len(fx.changed) > 0 || fx.reschedule) {
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
	return &Store{db: db, now: time.Now}, nil
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
		target := version + 1
		if !noForeignKeys[target] {
			if _, err := tx.ExecContext(ctx, migrations[version-1]); err != nil {
				return fmt.Errorf("migrate schema to version %d: %w", target, err)
			}
			continue
		}
		// migrateWithoutForeignKeys needs its own connection, so what
		// this transaction has done so far is committed first: a failure
		// in it then leaves the version this transaction reached, rather
		// than rolling that back too.
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, version); err != nil {
			return fmt.Errorf("record schema version: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate schema to version %d: %w", target, err)
		}
		if err := migrateWithoutForeignKeys(ctx, db, migrations[version-1], target); err != nil {
			return err
		}
		if tx, err = db.BeginTx(ctx, nil); err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer tx.Rollback()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, schemaVersion); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	return tx.Commit()
}

// migrateWithoutForeignKeys runs statement, which takes the schema to
// target, on a connection of its own with foreign keys off: PRAGMA
// foreign_keys cannot change inside a transaction, and the migrations in
// noForeignKeys need it off for the whole migration, not just deferred,
// because the implicit delete of a rebuilt table's DROP TABLE is checked
// at commit regardless. The rows that reference the rebuilt table are
// checked explicitly with PRAGMA foreign_key_check before committing, so
// a migration that would otherwise leave a dangling reference fails
// instead. A failure rolls back, leaving the version unchanged.
func migrateWithoutForeignKeys(ctx context.Context, db *sql.DB, statement string, target int) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate schema to version %d: %w", target, err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("migrate schema to version %d: %w", target, err)
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate schema to version %d: %w", target, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("migrate schema to version %d: %w", target, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, target); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("migrate schema to version %d: %w", target, err)
	}
	violations, err := foreignKeyViolations(rows)
	if err != nil {
		return fmt.Errorf("migrate schema to version %d: %w", target, err)
	}
	if violations != "" {
		return fmt.Errorf("migrate schema to version %d: foreign key violations: %s", target, violations)
	}
	return tx.Commit()
}

// foreignKeyViolations reads rows, the result of PRAGMA foreign_key_check,
// and describes each row it holds; empty when it holds none.
func foreignKeyViolations(rows *sql.Rows) (string, error) {
	defer rows.Close()
	var violations []string
	for rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowID, &parent, &fkid); err != nil {
			return "", err
		}
		violations = append(violations, fmt.Sprintf("%s row %d references missing %s (foreign key %d)", table, rowID.Int64, parent, fkid))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return strings.Join(violations, "; "), nil
}

func formatTime(t time.Time) string {
	return t.Format(time.RFC3339Nano)
}

func parseTime(raw string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, raw)
}

// seeDaemon records that daemon made a request at at, creating its row on
// first sight. A daemon seen is no longer lost. A nil harness leaves the
// recorded one unchanged.
func seeDaemon(ctx context.Context, tx *sql.Tx, daemon protocol.DaemonID, at time.Time, harness *protocol.Harness) error {
	var name, version any
	if harness != nil {
		name, version = harness.Name, harness.Version
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO daemons (id, first_seen, last_seen, harness_name, harness_version)
		VALUES (?1, ?2, ?2, ?3, ?4)
		ON CONFLICT (id) DO UPDATE SET
			last_seen = excluded.last_seen,
			lost_at = NULL,
			harness_name = coalesce(excluded.harness_name, daemons.harness_name),
			harness_version = coalesce(excluded.harness_version, daemons.harness_version)`,
		string(daemon), formatTime(at.UTC()), name, version)
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
	if err := seeDaemon(ctx, tx, daemon, s.now(), nil); err != nil {
		return err
	}
	return tx.Commit()
}

// heldSeqColumn is the highest seq, as task t's latest daemon numbers
// them, up to which the server holds every event that daemon sent: the
// end of the contiguous prefix starting at 1, or 0. That is the lowest
// stored seq above t.seq_base whose successor is missing, provided
// seq_base + 1 is stored, less seq_base.
const heldSeqColumn = `CASE WHEN EXISTS (SELECT 1 FROM events WHERE task_id = t.id AND seq = t.seq_base + 1)
	THEN (SELECT min(e.seq) FROM events e WHERE e.task_id = t.id AND e.seq > t.seq_base
		AND NOT EXISTS (SELECT 1 FROM events n WHERE n.task_id = t.id AND n.seq = e.seq + 1)) - t.seq_base
	ELSE 0 END`

// appendEvents stores a daemon's batch of events in one transaction and
// returns the held seq of every task in the batch. An event already
// stored under its task and seq is not stored again; when it differs from
// the stored one it is returned in conflicts and the stored one is kept.
// A batch naming any task the server moved from daemon is refused whole
// with a *movedTaskError, and one naming any other task not assigned to
// daemon with a *foreignTaskError. Each event is stored at its seq plus
// its task's seq base.
//
// A child the batch leaves finished hands its final reply back to its
// parent unless it sent a message during the turn, a task the batch
// leaves finished has the messages waiting in its inbox queued for
// delivery, a task the batch leaves yielded has its resume
// queued, and the parent of a task the batch ends is told, as are the
// senders of the messages left undelivered in its inbox. Each permission
// request the batch stores is put to the permission policy, if any, and
// answered unless the policy leaves it to the owner.
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
	bases := make(map[protocol.TaskID]int64, len(tasks))
	for _, task := range tasks {
		var owner string
		var base int64
		err := tx.QueryRowContext(ctx, `SELECT daemon_id, seq_base FROM tasks WHERE id = ?`, string(task)).Scan(&owner, &base)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, &foreignTaskError{Task: task, Daemon: daemon}
		}
		if err != nil {
			return nil, nil, fmt.Errorf("look up task %q: %w", task, err)
		}
		p, err := loadProgress(ctx, tx, task)
		if err != nil {
			return nil, nil, err
		}
		moved, err := movedFrom(ctx, tx, task, daemon, owner, p.State)
		if err != nil {
			return nil, nil, err
		}
		if moved {
			return nil, nil, &movedTaskError{Task: task, Daemon: daemon}
		}
		// A task whose start has not been admitted is on no daemon yet.
		if owner != string(daemon) || p.State == TaskQueued {
			return nil, nil, &foreignTaskError{Task: task, Daemon: daemon}
		}
		progresses[task] = &p
		before[task] = p.State
		bases[task] = base
	}

	var harness *protocol.Harness
	if len(events) > 0 {
		harness = &events[len(events)-1].Harness
	}
	if err := seeDaemon(ctx, tx, daemon, s.now(), harness); err != nil {
		return nil, nil, err
	}

	// A quota reading belongs to the budget of the daemon's key as it is
	// now.
	key, err := queryBudgetKey(ctx, tx, daemon)
	if err != nil {
		return nil, nil, err
	}

	// requests are the permission requests the batch stored for the first
	// time, for the permission policy to decide.
	var requests []protocol.Event
	for _, event := range events {
		stored := int64(event.Seq) + bases[event.TaskID]
		var budgetHarness, budgetAccount, budgetDaemon any
		if event.Kind == protocol.KindQuotaObserved {
			budgetHarness, budgetAccount, budgetDaemon = key.Harness, nullable(key.Account), nullable(string(key.Daemon))
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO events (task_id, seq, kind, harness_name, harness_version, time, payload, budget_harness, budget_account, budget_daemon)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (task_id, seq) DO NOTHING`,
			string(event.TaskID), stored, string(event.Kind), event.Harness.Name, event.Harness.Version,
			formatTime(event.Time), string(event.Payload), budgetHarness, budgetAccount, budgetDaemon)
		if err != nil {
			return nil, nil, fmt.Errorf("store event %s/%d: %w", event.TaskID, event.Seq, err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return nil, nil, fmt.Errorf("store event %s/%d: %w", event.TaskID, event.Seq, err)
		}
		if inserted == 1 {
			if event.Kind == protocol.KindPermissionRequested {
				requests = append(requests, event)
			}
			progresses[event.TaskID].seeEvent(event)
			continue
		}
		var kind, harnessName, harnessVersion, eventTime, payload string
		err = tx.QueryRowContext(ctx, `
			SELECT kind, harness_name, harness_version, time, payload FROM events WHERE task_id = ? AND seq = ?`,
			string(event.TaskID), stored).Scan(&kind, &harnessName, &harnessVersion, &eventTime, &payload)
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
	// a parent or a sender can queue a turn for a task of this batch.
	fx := effects{reschedule: len(events) > 0}
	for _, task := range tasks {
		state := progresses[task].State
		if !before[task].Ended() && state.Ended() {
			if err := taskEnded(ctx, tx, task, state, &fx); err != nil {
				return nil, nil, err
			}
		}
		if before[task] != TaskFinished && state == TaskFinished {
			if err := handBack(ctx, tx, task, &fx); err != nil {
				return nil, nil, err
			}
		}
		if state == TaskFinished {
			if _, err := queueDelivery(ctx, tx, task, &fx); err != nil {
				return nil, nil, err
			}
		}
		if before[task] != TaskYielded && state == TaskYielded {
			if err := queueYieldedResume(ctx, tx, task, &fx); err != nil {
				return nil, nil, err
			}
		}
	}
	if err := answerByPolicy(ctx, tx, s.permissions, daemon, requests, &fx); err != nil {
		return nil, nil, err
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
// task assigned to it whose start has been admitted.
func (s *Store) heldSeqs(ctx context.Context, daemon protocol.DaemonID) (map[protocol.TaskID]uint64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("read held seqs: %w", err)
	}
	defer tx.Rollback()
	if err := seeDaemon(ctx, tx, daemon, s.now(), nil); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.id, `+heldSeqColumn+` FROM tasks t WHERE t.daemon_id = ? AND t.state <> ?`, string(daemon), string(TaskQueued))
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

// nullableID is id as a query argument: NULL when id is nil.
func nullableID(id *protocol.TaskID) any {
	if id == nil {
		return nil
	}
	return string(*id)
}

// nullable is s as a query argument: NULL when empty.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// issueCommand appends a command for task to the log of the daemon the
// task is assigned to, at once, without queueing it as a turn. A nil
// payload stores none.
//
// A task whose start has not been admitted is unknown to every daemon:
// a stop ends it without sending anything, and returns a command with no
// ID, and any other command is refused with errNotStarted. A stopped or
// failed task takes only a follow-up, which is queued as a turn, so any
// command is refused with errTaskEnded, and a dismissed task's with
// errDismissed. A pause to a yielded task takes the scheduler's hold
// over for the owner, so the scheduler's resume is dropped. A stop drops
// the task's waiting turns other than deliveries, which would otherwise
// start it again; a follow-up queued after the stop stays.
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
	if err := requireNotDismissed(ctx, tx, task); err != nil {
		return protocol.Command{}, err
	}
	p, err := loadProgress(ctx, tx, task)
	if err != nil {
		return protocol.Command{}, err
	}
	if p.State.Ended() {
		return protocol.Command{}, fmt.Errorf("%w: %q is %s", errTaskEnded, task, p.State)
	}
	var fx effects
	if kind == protocol.CommandStop {
		if err := dropWaitingTurns(ctx, tx, task, &fx); err != nil {
			return protocol.Command{}, err
		}
	}
	var command protocol.Command
	switch {
	case p.State == TaskQueued && kind == protocol.CommandStop:
		command = protocol.Command{TaskID: task, Kind: kind, Time: time.Now().UTC()}
		p.seeCommand(command)
		if err := saveProgress(ctx, tx, task, p); err != nil {
			return protocol.Command{}, err
		}
		fx.changed = append(fx.changed, task)
		fx.reschedule = true
		if err := taskEnded(ctx, tx, task, p.State, &fx); err != nil {
			return protocol.Command{}, err
		}
	case p.State == TaskQueued:
		return protocol.Command{}, fmt.Errorf("%w: %q waits for the scheduler to start it", errNotStarted, task)
	default:
		if kind == protocol.CommandPause && p.State == TaskYielded {
			if err := dropSchedulersResume(ctx, tx, task, &fx); err != nil {
				return protocol.Command{}, err
			}
		}
		if command, err = insertCommand(ctx, tx, protocol.DaemonID(daemon), task, kind, payload, &fx); err != nil {
			return protocol.Command{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return protocol.Command{}, fmt.Errorf("issue command: %w", err)
	}
	s.publish(&fx)
	return command, nil
}

// insertCommand appends a command to the log and folds it into the task's
// progress. A dismissed task takes no command. When the command stops
// the task, its parent and the senders of the messages left undelivered
// in its inbox are told.
func insertCommand(ctx context.Context, tx *sql.Tx, daemon protocol.DaemonID, task protocol.TaskID, kind protocol.CommandKind, payload json.RawMessage, fx *effects) (protocol.Command, error) {
	if err := requireNotDismissed(ctx, tx, task); err != nil {
		return protocol.Command{}, err
	}
	p, err := loadProgress(ctx, tx, task)
	if err != nil {
		return protocol.Command{}, err
	}
	before := p.State
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
	fx.reschedule = true
	if !before.Ended() && p.State.Ended() {
		if err := taskEnded(ctx, tx, task, p.State, fx); err != nil {
			return protocol.Command{}, err
		}
	}
	return command, nil
}

// yieldTask pauses task, assigned to daemon, for the scheduler, so that
// the task is yielded rather than paused once the pause settles
// (docs/adr/2026-10-08-scheduling.md).
func yieldTask(ctx context.Context, tx *sql.Tx, daemon protocol.DaemonID, task protocol.TaskID, fx *effects) (protocol.Command, error) {
	command, err := insertCommand(ctx, tx, daemon, task, protocol.CommandPause, nil, fx)
	if err != nil {
		return protocol.Command{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET pause_origin = ? WHERE id = ? AND state = ?`,
		string(pauseByScheduler), string(task), string(TaskPausing))
	if err != nil {
		return protocol.Command{}, fmt.Errorf("record the yield of task %q: %w", task, err)
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
		var kind, issued string
		var task sql.NullString
		var payload []byte
		if err := rows.Scan(&id, &task, &kind, &issued, &payload); err != nil {
			return nil, fmt.Errorf("read commands: %w", err)
		}
		at, err := parseTime(issued)
		if err != nil {
			return nil, fmt.Errorf("read command %d: %w", id, err)
		}
		commands = append(commands, protocol.Command{
			ID: uint64(id), DaemonID: daemon, TaskID: protocol.TaskID(task.String), Kind: protocol.CommandKind(kind), Time: at, Payload: payload,
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
// and the messages it sent or was sent, in id order. parent names the
// task that spawned it; nil for the owner's.
type history struct {
	parent   *protocol.TaskID
	events   []protocol.Event
	commands []protocol.Command
	// byPolicy holds the ids of the commands the permission policy issued.
	byPolicy map[uint64]bool
	// ownerStarts holds the ids of the start_task commands that admitted a
	// start the owner queued: a task's first, or a retry.
	ownerStarts map[uint64]bool
	messages    []storedMessage
}

// storedMessage is a message as the messages table keeps it. Exactly one
// of From and About is set: From for an agent's message, About for the
// server's notice that a task ended, AboutState being that task's state.
// DeliveredBy is the id of the prompt that delivered it, 0 while
// it waits.
type storedMessage struct {
	ID         uint64
	From       *protocol.TaskID
	To         protocol.TaskID
	About      *protocol.TaskID
	AboutState TaskState
	// AboutChild says About is a child of the recipient, so that the
	// notice says the child ended rather than that messages to it were
	// not delivered.
	AboutChild bool
	// HandBack says the server sent the message for From, handing back
	// From's final reply of a turn to its parent.
	HandBack    bool
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
	var h history
	var parent sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT parent_id FROM tasks WHERE id = ?`, string(task)).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return history{}, fmt.Errorf("%w: %q", errUnknownTask, task)
	}
	if err != nil {
		return history{}, fmt.Errorf("look up task %q: %w", task, err)
	}
	if parent.Valid {
		spawner := protocol.TaskID(parent.String)
		h.parent = &spawner
	}
	if h.events, err = queryEvents(ctx, tx, task, 0); err != nil {
		return history{}, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.daemon_id, c.task_id, c.kind, c.time, c.payload, c.by_policy,
			EXISTS (SELECT 1 FROM turns u WHERE u.admitted_command_id = c.id AND u.origin = ?3)
		FROM commands c
		WHERE c.task_id = ?1 OR c.kind = ?2 AND c.task_id IN (SELECT id FROM tasks WHERE parent_id = ?1)
		ORDER BY c.id`, string(task), string(protocol.CommandStartTask), string(originOwner))
	if err != nil {
		return history{}, fmt.Errorf("read commands of task %q: %w", task, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var daemon, owner, kind, issued string
		var payload []byte
		var byPolicy, byOwner bool
		if err := rows.Scan(&id, &daemon, &owner, &kind, &issued, &payload, &byPolicy, &byOwner); err != nil {
			return history{}, fmt.Errorf("read commands of task %q: %w", task, err)
		}
		if byOwner && kind == string(protocol.CommandStartTask) {
			if h.ownerStarts == nil {
				h.ownerStarts = make(map[uint64]bool)
			}
			h.ownerStarts[uint64(id)] = true
		}
		if byPolicy {
			if h.byPolicy == nil {
				h.byPolicy = make(map[uint64]bool)
			}
			h.byPolicy[uint64(id)] = true
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
		SELECT m.id, m.from_task, m.to_task, m.about_task, coalesce(a.state, ''), coalesce(a.parent_id = m.to_task, 0), m.hand_back, m.text, m.created_at, coalesce(m.delivered_command_id, 0)
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
		if err := rows.Scan(&id, &from, &to, &about, &state, &message.AboutChild, &message.HandBack, &message.Text, &created, &delivered); err != nil {
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
	// Budget is the key of the budget its turns are checked against, and
	// Quota that budget's newest usage-limit reading, observed at
	// QuotaAt; nil when there is none.
	Budget  budgetKey
	Quota   *protocol.QuotaObserved
	QuotaAt time.Time
	// InUse counts the tasks holding a slot on the daemon.
	InUse int
	// LostAt is when the server declared the daemon lost; nil while it
	// is not.
	LostAt *time.Time
	// Labels are the owner's, and Facts what the daemon last reported.
	Labels, Facts Labels
}

// daemons returns every daemon that has been seen, by id, each with the
// newest quota reading of its budget.
func (s *Store) daemons(ctx context.Context) ([]daemonSummary, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("read daemons: %w", err)
	}
	defer tx.Rollback()
	readings, err := queryReadings(ctx, tx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT d.id, d.last_seen, d.harness_name, d.harness_version, d.lost_at, d.labels, d.facts,
			(SELECT count(*) FROM tasks t WHERE t.daemon_id = d.id AND t.state IN (?1, ?2, ?3, ?4))
		FROM daemons d
		ORDER BY d.id`,
		string(TaskPending), string(TaskRunning), string(TaskAwaitingPermission), string(TaskPausing))
	if err != nil {
		return nil, fmt.Errorf("read daemons: %w", err)
	}
	defer rows.Close()
	var daemons []daemonSummary
	for rows.Next() {
		var id, lastSeen, labels, facts string
		var harnessName, harnessVersion, lostAt sql.NullString
		var inUse int
		if err := rows.Scan(&id, &lastSeen, &harnessName, &harnessVersion, &lostAt, &labels, &facts, &inUse); err != nil {
			return nil, fmt.Errorf("read daemons: %w", err)
		}
		daemon := daemonSummary{ID: protocol.DaemonID(id), InUse: inUse}
		var err error
		if daemon.Labels, err = decodeLabels(labels); err != nil {
			return nil, fmt.Errorf("read daemon %q: %w", id, err)
		}
		if daemon.Facts, err = decodeLabels(facts); err != nil {
			return nil, fmt.Errorf("read daemon %q: %w", id, err)
		}
		if daemon.LastSeen, err = parseTime(lastSeen); err != nil {
			return nil, fmt.Errorf("read daemon %q: %w", id, err)
		}
		if lostAt.Valid {
			at, err := parseTime(lostAt.String)
			if err != nil {
				return nil, fmt.Errorf("read daemon %q: %w", id, err)
			}
			daemon.LostAt = &at
		}
		if harnessName.Valid {
			daemon.Harness = &protocol.Harness{Name: harnessName.String, Version: harnessVersion.String}
		}
		daemon.Budget = budgetKeyOf(daemon.ID, daemon.Facts, daemon.Harness)
		if reading := readings[daemon.Budget]; reading != nil {
			daemon.Quota, daemon.QuotaAt = &reading.QuotaObserved, reading.At
		}
		daemons = append(daemons, daemon)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read daemons: %w", err)
	}
	return daemons, nil
}

// requireNotDismissed returns errDismissed when the owner has dismissed
// task.
func requireNotDismissed(ctx context.Context, tx *sql.Tx, task protocol.TaskID) error {
	var dismissed bool
	err := tx.QueryRowContext(ctx, `SELECT dismissed_at IS NOT NULL FROM tasks WHERE id = ?`, string(task)).Scan(&dismissed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %q", errUnknownTask, task)
	}
	if err != nil {
		return fmt.Errorf("look up task %q: %w", task, err)
	}
	if dismissed {
		return fmt.Errorf("%w: %q", errDismissed, task)
	}
	return nil
}

// dropWaitingTurns removes task's turns that wait for the scheduler, but
// for deliveries, which wait for the task to finish.
func dropWaitingTurns(ctx context.Context, tx *sql.Tx, task protocol.TaskID, fx *effects) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM turns WHERE task_id = ? AND kind <> ? AND admitted_command_id IS NULL`,
		string(task), string(turnDeliver))
	if err != nil {
		return fmt.Errorf("drop the waiting turns of task %q: %w", task, err)
	}
	if n, _ := result.RowsAffected(); n > 0 {
		fx.changed = append(fx.changed, task)
		fx.reschedule = true
	}
	return nil
}
