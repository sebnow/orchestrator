# Client protocol

- Status: Accepted
- Decided: 2026-10-07
- Source: owner decision, working session on 2026-10-07

## Context

Each client daemon dials out to the server, all daemons speak one
protocol to it, and the connection must work without a VPN or private
network ([client connectivity](2026-10-07-client-connectivity.md)).
Daemons run on cloud VPSes, which are ephemeral, and on the laptop of the
owner, the single person who runs the server and the client machines. The
daemon runs the harness (the Claude Code CLI) and exchanges stream-json
(newline-delimited JSON) with it on stdin and stdout
([harness adapter](2026-10-07-harness-adapter.md)).

The protocol carries what these decisions require:

- the harness's output as it is produced, kept across a dropped connection
  ([connection loss](2026-10-07-connection-loss.md)) and stored centrally
  as the record ([transcripts](2026-10-07-transcripts.md),
  [server storage](2026-10-07-server-storage.md));
- tasks and their settings
  ([task interface](2026-10-07-task-interface.md));
- permission prompts, which the harness asks through
  `--permission-prompt-tool` and waits on
  ([human steering](2026-10-07-human-steering.md));
- cooperative pause, its acknowledgement and stop note (the agent's report
  of where it stopped), and the emergency interrupt
  ([graceful pause](2026-10-07-graceful-pause.md));
- agents' requests to spawn agents and to use their inboxes
  ([agent messaging](2026-10-07-agent-messaging.md)).

[Harness independence](2026-10-07-harness-independence.md) requires the
protocol, task model and server not to depend on Claude Code specifics,
and keeps Claude-shaped types inside the daemon.

## Decision

### Transport

HTTPS with JSON. The daemon POSTs batches of events to the server and
receives commands over one long-lived server-sent events (SSE) stream per
daemon. The daemon opens both connections, so it dials out as client
connectivity requires.

gRPC was considered and rejected: the owner found no reason strong enough
to adopt it. A duplex WebSocket was rejected because SSE event ids give
replay of commands and sequenced POST batches give replay of events, both
described below, and HTTP/2 can multiplex the two over one TCP connection; a
WebSocket would need its own framing, ids and replay in both directions.

This record does not decide authentication.
[Client connectivity](2026-10-07-client-connectivity.md) records a
preference for certificate-based authentication; mutual TLS is the
preferred form.

### Ordering and replay

The daemon gives every event it sends a sequence number that increases
monotonically per task, and writes the event to an append-only journal on
disk before sending it. When the daemon connects or reconnects, the server
reports the highest sequence number it holds for each of that daemon's
tasks, and the daemon replays every later event.

An event is durable once the server acknowledges it. The server's copy is
the record; the journal lives on a VPS that may be discarded. The journal
exists so that a dropped connection or a daemon restart loses no event the
server has not yet acknowledged.

### Events, daemon to server

- Harness output: each raw line the harness writes, for Claude Code a
  stream-json line, sent as an opaque payload tagged with the harness name
  and version.
- Control events that originate in the daemon: harness started, harness
  exited, permission requested, pause acknowledged (with the stop note),
  and quota observed.

The server stores the raw lines as the transcript of record and normalises
them into harness-neutral event types. The task model, the web interface,
and the scheduler proposed in
[budget and pause](../design/2026-10-07-budget-and-pause.md) consume those
types.

One Go package in the monorepo
([repository and toolchain](2026-10-07-repository-and-toolchain.md))
parses stream-json, and both the daemon and the server use it. The daemon
uses it to drive the harness: it detects the end of a turn from `result`,
confirms from the echoed `user_message_uuid` that a turn picked up the
pause message, and reads quota figures from `rate_limit_event`. The server
uses it to normalise.

Normalisation runs on the server rather than in the daemon for two
reasons:

- The server can run the normaliser again over stored raw history when the
  normaliser improves or a bug in it is found.
- The wire protocol stays small. The harness-neutral event types are Go
  types, which are cheaper to change than a wire format.

### Commands, server to daemon

Sent over the SSE stream: start task, send follow-up prompt, pause,
resume, interrupt, stop, and answer permission. Start task carries the
task id, prompt, system prompt, workspace specification, model settings,
and the per-task pause limits that graceful pause defines.

Stop ends the task: the daemon interrupts anything running and terminates
the harness process, and the task is recorded as stopped by the owner.
Interrupt is the emergency path that graceful pause defines; it leaves the
task's session alive.

Each command is an SSE event with an `id:` field. On reconnect the daemon
sends the standard SSE `Last-Event-ID` header, and the server resends every
command after that id. The server resends from its per-daemon command log,
the SQLite record of who issued which command and when. Delivery is at
least once; the daemon ignores a command whose id it has already applied.

The daemon does not acknowledge commands. A command's effect arrives as an
event: start task as harness started, pause as pause acknowledged or as
the harness output showing an interrupt, stop as harness exited, and
answer permission as the transcript continuing.

### Permission requests

A permission request carries the task id, the tool name, and the tool
input as structured JSON. The request is itself a sequenced event in the
task's stream, so the server knows which events precede it.

The server answers by policy or hands the request to the owner. Policy is
evaluated on the server; the policy engine is not decided. By default a
request waits indefinitely for an answer.

### Agent requests

Requests from agents to read and write inboxes and to spawn agents also go
through the daemon to the server over this protocol. This record does not
define their shape.

## Consequences

- The transcript of record is the harness's own output (for Claude Code,
  stream-json lines). The neutral events can be rebuilt from it.
- Claude-shaped types live in a harness package that both the daemon and
  the server use. This amends the consequences of
  harness independence. The protocol stays harness-neutral because it
  treats harness lines as opaque payloads tagged with their harness.
  Substituting a harness means adding a second such package, selected by
  the tag.
- The server binary contains Claude Code-specific parsing. Server code
  outside the harness package must use only the neutral types.
- The daemon's harness control relies on undocumented or unconfirmed parts
  of stream-json: `rate_limit_event` is undocumented
  ([harness integration](2026-10-07-harness-integration.md)), and the
  `user_message_uuid` echo is **UNVERIFIED**
  ([graceful pause](2026-10-07-graceful-pause.md)).
- A task waiting on a permission answer keeps its harness process alive
  until the answer arrives. **UNVERIFIED:** that Claude Code applies no
  timeout of its own to `--permission-prompt-tool`; waiting indefinitely
  relies on it.
- At-least-once delivery requires a command's id to stay the same across
  resends, and the daemon to deduplicate by it.
- A daemon restart differs from a dropped connection: the harness exits
  when its stdin closes ([harness integration](2026-10-07-harness-integration.md)),
  so the daemon's tasks end with it. Recovery of those tasks is not
  decided here.
