---
status: accepted
date: 2026-10-10
source: >-
  owner decisions, 2026-10-10: a task that is not running and not
  dismissed takes a follow-up; messages can be queued for after the
  turn or injected now; resuming a long session must show its cost;
  failures before the harness starts are named as such
---

# Task follow-ups and steering

## Context

The first dogfood tasks showed a dead end: a task whose clone failed,
because the owner's ssh agent was locked, came up `failed`. [Task
lifetime](2026-10-08-task-lifetime.md) and [shutdown
recovery](2026-10-08-shutdown-recovery.md) both make `failed` terminal,
so the task could only be recreated, not retried with the same id. A
brain that has just finished its turn is the one the owner is most
likely to want to keep talking to.

The Agent SDK's agent loop queues a message written during a turn and
answers it in a new turn after the current one ends; headless mode
cannot inject a message mid-turn
(https://code.claude.com/docs/en/agent-sdk/agent-loop). A prompt the
owner wants acted on now, not after the turn, needs a different path
than queuing.

Resuming a session after its cache lifetime, one hour on a subscription
within plan usage, reprocesses the whole history as uncached input
(https://code.claude.com/docs/en/prompt-caching#resuming-a-session). A
long-running task's owner cannot judge whether to resume, steer, or
start over without knowing that cost in advance.

## Decision

### Follow-ups

A task that is not running and not dismissed accepts a follow-up
prompt, which starts a new turn on its session: `finished`, `failed`,
`paused` and `stopped` are all resumable. Dismissal is the only
terminal act.

A task that never got a session, because its workspace could not be
prepared or its harness could not be started, is started afresh by
placement, on any fitting daemon including the one it failed on, with
an optional prompt (Retry).

Dismissal discards the task on its daemon: the daemon deletes its
record, journal and workspace. Until dismissal, a stopped or failed
task's workspace and session are kept.

This amends three earlier records, each at one point:

- [Task lifetime](2026-10-08-task-lifetime.md) says "`stopped` and
  `failed` are terminal" and that stopping a finished or paused task
  has "the daemon delete[] the session id it stored for the task".
  Both now apply only up to dismissal: a stopped or failed task keeps
  its session and workspace, and takes a follow-up like any other.
- [Shutdown recovery](2026-10-08-shutdown-recovery.md) says "`failed`
  is terminal, for a harness that exits with an error on its own and
  for a task that cannot start." A failed task is no longer terminal;
  only a task that never got a session is retried by a fresh start
  rather than a follow-up on its (nonexistent) session.
- [Daemon loss](2026-10-08-daemon-loss.md) says "a moved task never
  starts again on a daemon it ran on before." That rule is about
  placement after a daemon is declared lost, and stands for it; it now
  excludes an owner's Retry or follow-up, which may land back on the
  daemon a start failure came from, since the daemon it failed on
  never ran any part of the task.

### Start failures

A task whose harness never started reports it as such, in the same
exit event: "workspace could not be prepared: …" when the clone or
setup failed, or "harness could not be started: …" when the process
itself would not come up. The task's header and transcript show
whichever applies, and its page offers Retry instead of Resume, since
the task never had a session to resume.

### Steering

A follow-up to a running task is either held for after the turn or
sent now. A held prompt is kept by the daemon, not written to the
harness, until the turn ends; the task page lists held prompts and
can withdraw them. "Now" interrupts the running turn and sends the
prompt on the same session; the transcript shows this as steering, not
as an interrupt that pauses the task. A turn that ends with a pause
settling, or with the process exiting, drops any held prompts, and the
transcript says so.

This amends [harness integration](2026-10-07-harness-integration.md)
and [task lifetime](2026-10-08-task-lifetime.md) at the point where
each has a follow-up prompt go to the harness as it arrives: harness
integration's consequence that "follow-up prompts can be sent over
stdin mid-session without restarting the harness", and task lifetime's
rule that the daemon closes the harness's input only once "no prompt
the daemon sent is still waiting for its turn". A held prompt is not
sent until the turn ends, so it is never one the daemon has already
given the harness to wait on; only a steered ("now") prompt still goes
straight to the running process.

### Cost on resume

A task's page gives the size of its session's context from the latest
turn's usage, as the harness reports it. Between turns, the follow-up
form says how much context resuming will read, and that a Claude
subscription's cache lasts one hour of inactivity within plan usage,
so the owner can judge a follow-up against a continuation before
sending either.

"Continue in a new task" starts a new task in the same project and
agent, with this task's final reply as its prompt, and links it to
this task as its continuation; the new task starts without the old
session's history.

### Protocol additions

- A `steer` flag on a prompt command chooses "now" over "held"; the
  steering transcript line and the choice between pause and interrupt
  both follow from it.
- A withdraw command drops a held prompt by id, the only way to remove
  one the daemon has not yet written to the harness.
- A discard command tells a daemon to delete a dismissed task's
  record, journal and workspace; before, nothing had to tell the
  daemon to delete anything at dismissal, because a dismissed task was
  already `stopped` or `failed` with its process gone. Now that those
  states keep their workspace until dismissal, the act of ending one
  for good has to reach the daemon as a command.
- Held and released prompt events report a held prompt's lifetime on
  the task page, matching how pause acknowledgement and interrupt
  already report theirs.
- The owner API gains daemon listing, and the continue, withdraw and
  tree operations, so that a script can do everything the task page
  now offers: see where a task could run, continue it, withdraw a held
  prompt, and read the tree a continuation or a spawn produces.

## Consequences

- Workspaces of stopped and failed tasks stay on disk until dismissal,
  where before they were deleted with the process; the owner trades
  that disk for the ability to retry without losing the workspace's
  state.
- Tasks that ended before this change cannot be followed up: their
  daemons already deleted the session and workspace that a follow-up
  would need, under the rules this record replaces.
- A message held in a task's inbox at a stop is delivered after all if
  the task is later resumed and finishes, since the inbox keeps queued
  messages across the states this record makes resumable.
- The context-size figure a task shows comes from the result's usage,
  as the harness reports it; what a multi-call turn's usage sums is not
  settled here.

Revisit if a harness other than Claude Code reports usage or session
cost differently, or if dismissal needs a grace period instead of
being immediate and final.
