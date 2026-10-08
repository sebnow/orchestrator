---
status: accepted
date: 2026-10-08
supersedes: docs/adr/2026-10-08-restart-recovery.md
source: >-
  owner's rule that a clean shutdown stops a task until it is resumed,
  2026-10-08;
  [restart recovery](2026-10-08-restart-recovery.md), Decision;
  [shutdown findings](../design/2026-10-08-shutdown-findings.md)
---

# Shutdown recovery

## Context

A task's harness process lives for one turn, and the daemon keeps what
it needs to resume the task across its own restarts: the harness's
session id, the task's settings, and its workspace
([task lifetime](2026-10-08-task-lifetime.md)).
[Restart recovery](2026-10-08-restart-recovery.md) decided that a turn
cut short by a daemon restart leaves its task `paused` for the owner to
resume. Claude Code resumed such a session and finished the task's
steps in a live run
([marker verification](../design/2026-10-08-marker-verification.md),
"A turn cut off by the daemon's death resumed in the same session").
Restart recovery left two cases open.

Under restart recovery, a clean shutdown interrupted the running turn,
closed the harness's input, and let the harness's exit decide the
task's state; [task lifetime](2026-10-08-task-lifetime.md) expected a
clean exit and a `finished` task. In three live runs the harness exited
with code 1 instead, and in the two before this decision the task
`failed`, which is terminal
([marker verification](../design/2026-10-08-marker-verification.md),
"A turn interrupted by a clean shutdown ended `failed`";
[shutdown findings](../design/2026-10-08-shutdown-findings.md), "Both
interrupted runs exited 1 with an error `result` last"). In the two
runs whose output was kept, the interrupted turn's `result`, an error,
was the last `result` the harness wrote; in an earlier run that exited
0 after an interrupt, a successful turn followed it. The owner's rule
is that a clean shutdown means the task has stopped, and nothing
happens to it until it is resumed.

The harness can also outlive the daemon. In two live runs it ran on
for 23 and for about 18 seconds after the daemon was killed with
SIGKILL
([marker verification](../design/2026-10-08-marker-verification.md),
"The harness outlived a killed daemon by 23 seconds";
[shutdown findings](../design/2026-10-08-shutdown-findings.md), "The
restarted daemon waited for the old harness to exit"). A daemon
restarted in that time could resume the session while the old process
still ran.

## Decision

A turn cut short by the daemon, by a restart or by a clean shutdown,
leaves its task `paused`, and the owner resumes it as any paused task:
Resume, or a follow-up prompt, is queued as a turn, and the daemon
starts a new process that continues the task's session in its
workspace. The resume prompt tells the agent that its last turn was cut
short. Messages in the task's inbox stay queued until the owner resumes
it. A filler task whose turn the scheduler was pausing to free a slot
([scheduling](2026-10-08-scheduling.md), Yielding) is paused for the
owner too.

On a clean shutdown the daemon interrupts each running turn, closes the
harness's input, and kills the harness if it has not exited within the
shutdown timeout. A turn it interrupted is reported cut short unless
the harness exits with code 0. The daemon treats a clean exit as a turn
that ended before the interrupt, and the task ends as that exit says.

The daemon records the session id as soon as the harness reports it.
When no session id was recorded, because the harness died before
reporting one, Resume starts a new session in the same workspace with
the task's first prompt. If the owner sent a prompt instead of Resume,
that prompt follows the first. The error text of the `harness_exited`
says that no session was recorded, and the task page tells the owner
that Resume will start a new session.

A task whose start the daemon had not finished has no workspace ready
to resume in, so it fails. `failed` is terminal, for a harness that
exits with an error on its own and for a task that cannot start.

The daemon records each harness process's pid and start time in the
task's record while it runs. A restarted daemon, before it reports any
turn cut short, waits up to the shutdown timeout, 30 seconds by
default, for each recorded harness still running to exit, and then
kills those that have not. It treats a process as the old harness only
if the process with that pid has the recorded start time. If it could
not record a harness's start time, it neither waits for nor kills that
pid.

The `harness_exited` that reports a turn cut short has exit code -1 and
an error text that names the case: a restart or a shutdown, with a
session to continue or without one, or a restart after which the task
cannot be resumed. When a shutdown cuts short the turn of a task that
cannot be resumed, the harness's exit code decides the task's state.
The server reads the case from the error text, so the protocol's fields
do not change.

## Consequences

- After a restart or a shutdown the task survives, and the cut-short
  turn does not complete; what it did stays in the workspace and the
  session, including a tool call that ran halfway. The agent is told
  that its turn was cut short, but not at which step. In one live run
  the resumed agent repeated the interrupted step, and in two it went
  on with the next
  ([shutdown findings](../design/2026-10-08-shutdown-findings.md), "A
  shutdown pauses the task, and Resume continues the session";
  [marker verification](../design/2026-10-08-marker-verification.md),
  "A turn cut off by the daemon's death resumed in the same session").
- A task resumed without a session starts its conversation again, in a
  workspace its first turn may already have changed.
- The server and the daemon must agree on the error texts that mark a
  turn cut short. A daemon that does not write these texts leaves the
  task `failed`.
- The `harness_exited` of a turn a shutdown cut short does not carry the
  harness's own exit code; the daemon logs it, and the event keeps the
  end of the harness's stderr.
- A clean shutdown leaves a running task `paused`, unless its harness
  exits with code 0 or the task cannot be resumed; the harness's exit
  then decides.
- A daemon restart takes up to the shutdown timeout longer while a
  harness of its previous run is still running. In one live run the
  restarted daemon waited 18 seconds for the old harness to exit.
- A process that reuses a harness's pid in the same second the harness
  started would be taken for it, because the start time has a
  resolution of one second. The daemon reads start times with
  `ps -o lstart=`, which was checked on macOS only; when `ps` fails, the
  daemon neither waits for nor kills the old harness.

Revisit if the protocol gains a field saying why a process ended.
