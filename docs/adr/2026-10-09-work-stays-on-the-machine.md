---
status: accepted
date: 2026-10-09
source: >-
  owner decision, 2026-10-08 (the daemon must not send the work to the
  server; delivery is a pushed branch); owner decision, 2026-10-09
  (subagents Claude Code would run in its cloud are to be refused)
---

# Work stays on the daemon's machine

## Context

The daemon runs a task's harness on the owner's machine or VPS, with a
clone of the task's repository. The server holds the record of events
and the transcript ([transcripts](2026-10-07-transcripts.md), [client
protocol](2026-10-07-client-protocol.md)).

Two earlier decisions already follow one rule that no record states:
[work delivery](2026-10-08-work-delivery.md) rejected uploading the
work to the server, and on 2026-10-09 the owner had cloud-run subagents
refused (`docs/design/2026-10-09-remote-subagents.md`).

Upcoming decisions on credentials and on a Nostr relay need the rule
stated once.

## Decision

- The task's workspace, its repository contents and anything derived
  from them leave the daemon's machine only as a push to the task's own
  repository remote, by the daemon, as work delivery records.
- No part of a task's execution runs on a machine the owner does not
  operate: the harness, its tools and its subagents run on the daemon's
  machine. A harness feature that would run work elsewhere is refused
  (for Claude Code: `Agent`/`Task` with `isolation: remote`, denied by
  the server before any policy).
- The model API the harness talks to is the one exception, inherent to
  the harness, and is not the orchestrator's channel; the transcript
  the server stores is the harness's output as the transcripts record
  decided, not the workspace.
- Credentials a task uses are held on the daemon's machine, outside the
  agent's process; they are never sent to the server. (Mechanism
  undecided; ssh keys already follow this through the agent socket
  relay, [harness user](2026-10-08-harness-user.md).)

## Consequences

- Future transports (a Nostr relay,
  `docs/design/2026-10-09-nostr-direction.md`) carry events, never
  workspaces.
- A credential design must substitute secrets outside the agent's reach
  rather than hand them to the agent; see
  `docs/design/2026-10-09-credential-substitution.md`.
- Each harness adapter must identify and refuse its off-machine
  features; the Claude Code list is in
  `docs/design/2026-10-09-remote-subagents.md` with its known gaps.
- The server never needs, and must not grow, an endpoint that receives
  file contents.
