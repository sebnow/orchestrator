-- Times are RFC 3339 strings with nanoseconds. Payloads are JSON text.

CREATE TABLE daemons (
	id TEXT PRIMARY KEY,
	first_seen TEXT NOT NULL,
	last_seen TEXT NOT NULL,
	-- The harness named by the latest event the daemon sent; NULL until
	-- it sends one.
	harness_name TEXT,
	harness_version TEXT
) STRICT;

CREATE TABLE tasks (
	id TEXT PRIMARY KEY,
	daemon_id TEXT NOT NULL REFERENCES daemons (id),
	state TEXT NOT NULL,
	created_at TEXT NOT NULL,
	prompt TEXT NOT NULL,
	system_prompt TEXT NOT NULL,
	-- Both NULL when the task names no workspace.
	workspace_repo TEXT,
	workspace_ref TEXT,
	model TEXT NOT NULL,
	pause_acknowledge_ns INTEGER NOT NULL,
	pause_cleanup_ns INTEGER NOT NULL,
	CHECK ((workspace_repo IS NULL) = (workspace_ref IS NULL))
) STRICT;

CREATE INDEX tasks_by_daemon ON tasks (daemon_id);

CREATE TABLE events (
	task_id TEXT NOT NULL REFERENCES tasks (id),
	seq INTEGER NOT NULL CHECK (seq > 0),
	kind TEXT NOT NULL,
	harness_name TEXT NOT NULL,
	harness_version TEXT NOT NULL,
	time TEXT NOT NULL,
	payload TEXT NOT NULL,
	PRIMARY KEY (task_id, seq)
) STRICT;

-- The per-daemon command log: who was sent which command and when. The id
-- is the SSE event id, so AUTOINCREMENT keeps ids from being reused.
CREATE TABLE commands (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	daemon_id TEXT NOT NULL REFERENCES daemons (id),
	task_id TEXT NOT NULL REFERENCES tasks (id),
	kind TEXT NOT NULL,
	time TEXT NOT NULL,
	-- NULL for commands that carry no payload.
	payload TEXT
) STRICT;

CREATE INDEX commands_by_daemon ON commands (daemon_id, id);
