# Task lifetime

- Status: Proposed
- Decided: 2026-10-08
- Source: owner decision, 2026-10-08;
  [MVP smoke findings](../design/2026-10-08-mvp-smoke-findings.md),
  "A task stays running between turns" and Open;
  [resume spike](../design/2026-10-08-resume-spike.md), Answers

## Context

The daemon runs one `claude -p` process per task and, as
[harness integration](2026-10-07-harness-integration.md) decided, sends
follow-up prompts to it over stdin. The process therefore lives until the
owner stops the task or the daemon shuts down. In the MVP smoke test
every idle task held its process, and showed `running`, until the daemon
shut down; only then did it become `finished`. A pause resumes "with a
further stdin prompt to the same process"
([graceful pause](2026-10-07-graceful-pause.md)), so a paused task holds
its process too.

Claude Code keeps each session in a file under the OS user's
`~/.claude/projects/`, and `--resume <session id>` starts a new process
in the same session. The resume spike ran four processes with the
daemon's flags, two of them resumes, with Claude Code 2.1.289 and Haiku:

- the conversation carried over, and the new process kept the session id;
- the resumed process followed the system prompt without being given it
  again; per `claude --help`, Claude Code reuses the system prompt it
  recorded first until the conversation is compacted;
- the gateway answered permission requests in a resumed process, at the
  URL given at that process's launch;
- a pause acknowledged in one process was finished in the next, given the
  stop note in the resume prompt;
- `total_cost_usd` continued across the processes of a session, so the
  latest result's figure is the task's running total.

## Decision

A task's harness process lives for one turn. When the turn ends and no
prompt the daemon sent is still waiting for its turn, the daemon closes
the harness's input and the process exits; the task is then `finished`.
Any later prompt resumes the task in a new process with the harness's
session id, in the same workspace: an owner's follow-up, a resume after
a pause, and inbox messages from
[agent messaging](2026-10-07-agent-messaging.md). Resuming a paused task
uses this path. Within a turn, pausing works as
[graceful pause](2026-10-07-graceful-pause.md) describes.

Once a pause has settled, the turn ends, the process exits, and the task
is `paused`, not `finished`. A pause that had not settled when the
process exited did not take effect, and the task is `finished`. The next
prompt starts the next process.

Between turns a task has no process, so it has no gateway registration
and nothing to interrupt. `stopped` and `failed` are terminal. Stopping
a `finished` or `paused` task ends it without starting a process, and
the daemon deletes the session id it stored for the task.

## Consequences

- The daemon keeps, per task and across its own restarts, the harness's
  session id, the settings needed to start the task again, whether it is
  paused and, if so, its stop note, and the seq of its last event. A
  task's events continue one seq sequence across its processes, so the
  server acknowledges them by seq as before. Once the server has
  acknowledged every event of a process, the daemon deletes the journal;
  the task's record stays until the task is stopped or fails.
- `finished` is not terminal: a prompt makes the task `running` again.
- Each turn pays the start-up of a new process, and the session must be
  resumable on the daemon's machine, by its OS user, from the task's
  workspace. **UNVERIFIED:** a resume from another directory, user or
  machine, after compaction, or with the session file gone; the spike
  tried none.
- A daemon shutdown between turns loses no work, because an idle task has
  no process. A turn the shutdown interrupts ends with a clean exit, and
  the task is `finished`. **UNVERIFIED:** that such a task resumes after
  the daemon restarts; the spike did not try it. A daemon that crashed
  mid-turn marks the task `failed` when it restarts.

Revisit when adopting a harness that cannot resume a session in a new
process.
