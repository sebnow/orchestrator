---
status: superseded
date: 2026-10-08
superseded-by: docs/adr/2026-10-08-shutdown-recovery.md
source: >-
  coordinator decision, unattended;
  [task lifetime](2026-10-08-task-lifetime.md), Decision and
  Consequences;
  [resume spike](../design/2026-10-08-resume-spike.md), Answers
---

# Restart recovery

## Context

A task's harness process lives for one turn, and the daemon keeps what
it needs to resume the task across its own restarts: the harness's
session id, the task's settings, and its workspace
([task lifetime](2026-10-08-task-lifetime.md)). Until this decision, a
daemon that restarted found the tasks whose turn its previous run did
not end and closed each one's record with a `harness_exited` saying the
daemon restarted, and the server marked the task `failed`. The server
accepts no prompt, resume or message for a `failed` task, although such
a task's session and workspace are still on the machine.

The daemon saved a task's session id only when a process ended, so a
daemon that died during a task's first turn left no session id to
resume, even when the harness had reported one.

## Decision

A turn cut short by a daemon restart leaves its task `paused`, and the
owner resumes it as any paused task: Resume, or a follow-up prompt, is
queued as a turn, and the daemon starts a new process that continues
the task's session in its workspace. The resume prompt tells the agent
that its last turn was cut short. Messages in the task's inbox stay
queued until the owner resumes it. A filler task whose turn the
scheduler was pausing to free a slot
([scheduling](2026-10-08-scheduling.md), Yielding) is paused for the
owner too.

The daemon records the session id as soon as the harness reports it.
When no session id was recorded, because the harness died before
reporting one, Resume starts a new session in the same workspace with
the task's first prompt. If the owner sent a prompt instead of Resume,
that prompt follows the first. The error text of the `harness_exited`
says that no session was recorded, and the task page tells the owner
that Resume will start a new session.

A task whose start the daemon had not finished has no workspace ready
to resume in, so it fails. `failed` is terminal, for a harness that
exits with an error and for a task that cannot start.

The error text of the daemon's `harness_exited` names the case: a
restart with a session to continue, a restart without one, or a task
that cannot be resumed. The server reads the text, so the protocol
keeps its fields. A field of its own in `harness_exited` would need a
protocol change.

On a clean shutdown the daemon interrupts each running turn and closes
the harness's input, and the task ends as the harness's exit says.

## Consequences

- After a restart the task survives and the cut-short turn's progress
  is lost. The workspace keeps whatever that turn left, including a
  tool call that ran halfway; the agent is told that its turn was cut
  short, but not at which step.
- A task resumed without a session starts its conversation again, in a
  workspace its first turn may already have changed.
- The server and the daemon must agree on the error texts that mark a
  restart. A daemon that does not write these texts leaves the task
  `failed`.
- A turn interrupted by a clean shutdown ends `finished` when the
  harness exits cleanly, and nothing marks that its work stopped short.
- A turn interrupted by a clean shutdown ends `failed` when the daemon
  has to kill the harness after its shutdown timeout, 30 seconds by
  default.
- The daemon does not tie the harness process to its own life.
  In one live run, the harness outlived a daemon killed with SIGKILL by
  23 seconds and then exited on its own
  ([marker verification](../design/2026-10-08-marker-verification.md),
  "The harness outlived a killed daemon by 23 seconds"). The daemon does
  nothing about a harness that outlives it. A harness still running its
  turn when the restarted daemon resumes the session would share the
  session with the new process; this decision does not guard against
  that.
- Claude Code resumed a session whose last turn was cut off. In the
  same live run, the process the owner's Resume started continued the
  killed turn's session id, ran the steps that remained, and finished
  ([marker verification](../design/2026-10-08-marker-verification.md),
  "A turn cut off by the daemon's death resumed in the same session").

Revisit if the protocol gains a field saying why a process ended, or if
a clean shutdown should pause tasks too.
