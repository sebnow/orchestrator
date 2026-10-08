---
status: accepted
date: 2026-10-08
source: owner decision, working session on 2026-10-08
---

# Daemon loss

## Context

Daemons run on cloud VPSes, which are ephemeral, and on the owner's
laptop ([client protocol](2026-10-07-client-protocol.md)). A VPS may
disappear for good, taking its daemon with it.

[Scheduling](2026-10-08-scheduling.md) places a task when its first turn
is admitted and sends every later turn to the task's daemon, where its
workspace is. One of its consequences is that a task bound to a daemon
that never returns stays queued until the owner stops it. The owner
wants such tasks handled without that step. This record changes that
consequence: the tasks of a daemon the server declares lost are placed
again. The scheduling decision's rules for placing a first turn, and
its slots, priority, filler and budget, stay as they are, so this
record does not supersede the scheduling record.

A task's workspace and its harness session exist only on its daemon's
machine. The harness, Claude Code, keeps its session in a file under the
OS user's `~/.claude/projects/` on that machine
([task lifetime](2026-10-08-task-lifetime.md)), so a daemon on another
machine has no access to it.

The server treats a daemon as connected while its command stream is
open. On an idle stream it sends a keepalive, an SSE comment line, every
15 seconds. It records when it last saw each daemon, at each POST of
events, GET of acknowledgements, and opening of the command stream. A
daemon waits at most 30 seconds between attempts to reach the server,
and ends a command stream that has sent nothing for 45 seconds.

## Decision

### When a daemon is lost

A daemon is lost when its command stream is closed and the server has
not seen it for the daemon timeout, a server setting that defaults to 10
minutes. The close of its command stream counts as seeing it, so that a
daemon connected for longer than the timeout is not lost the moment its
stream drops. The timeout runs from the later of the server's start and
the last time the server saw the daemon, so that a server restart does
not declare its daemons lost. A lost daemon stays lost until the server
sees it again. The owner's daemon list shows each lost daemon and since
when it has been lost.

### Moving its tasks

The server treats the work a lost daemon held as gone, so each of its
tasks that has work to do is moved: its next turn is a new start on
another connected daemon, in a fresh clone of its repository and a new
harness session.

- A task with a turn under way (`pending`, `running`,
  `awaiting_permission` or `pausing`), or with a prompt or resume
  queued, moves as soon as the daemon is declared lost. So does a
  `finished` task with a message delivery queued. A `yielded` task has
  the scheduler's resume queued, and moves with them.
- A `finished` or `paused` task with no prompt or resume queued moves
  when its next one is queued, or for a `finished` task a message
  delivery, if its daemon is still lost then. A daemon that returns
  first keeps it. A delivery to a `paused` task waits for the owner's
  resume, as before.
- A task that has not started, bound to a lost daemon by the owner, is
  placed as if no daemon had been named.

A moved task is placed as a task started without a daemon, on the
connected daemon with the most free slots. A child task is placed by
the scheduling record's rule for children: on its parent's current
daemon if that has a free slot, otherwise on the connected daemon with
the most free slots. Either way a moved task never starts again on a
daemon it ran on before. Such a daemon may have returned still holding
the task's old record, and would take the new start for one it has
already run and skip it.

The new start replaces the owner's queued prompts and any queued resume.
It carries the task's first prompt and a note telling the agent that its
earlier work on the lost daemon is gone, and quotes the owner's latest
prompt to the task: the latest queued one if any, otherwise the latest
delivered one. The other queued prompts are dropped. Queued message
deliveries stay queued. The task's transcript shows the move, naming the
lost daemon and the new one, and the prompt the new session started
with.

The new daemon numbers the task's events from 1, as for any start. The
server keeps them after the events it already holds for the task, and
acknowledges them in the new daemon's numbering.

### A lost daemon that returns

When a daemon sends events for a task the server has moved away from
it, the server answers HTTP 409 on the events endpoint, as it already
does for a task that is not the daemon's. The answer's body is JSON that
names the reason, `task_moved` for a moved task and `not_assigned` for
any other task that is not the daemon's, with the task's id and a
message. No event or command kind changes.

On a `task_moved` refusal the daemon kills the task's harness if one
runs, and deletes the task's record, journal and workspace. On any other
refusal it stops sending the task's events and keeps the rest, as
before; the reason lets it tell the two apart.

### Rejected alternatives

- A heartbeat message from the daemon. The open command stream, with
  its keepalive, already says that a daemon is there.
- Moving every task of a lost daemon at once. A daemon that returns
  would lose tasks it could have continued, and each move starts the
  work again.
- Telling a returning daemon of its moved tasks over the command stream.
  That needs a new command kind. Refusing events already reaches the
  daemons that need it most: a daemon cut off during a turn still has
  events to send for the task.
- Having the new daemon number the moved task's events after those the
  server holds. The start command would need a field for it.

## Consequences

- A moved task starts its work again. What it did on the lost daemon,
  in its workspace and its conversation, is gone, and so are messages
  delivered to it there.
- A daemon away for longer than the timeout, such as a laptop that
  slept, loses the tasks that moved at once even if it returns; the
  timeout trades that against how long a task waits for a daemon that
  is gone.
- A returning daemon learns that a task moved only when it sends an
  event for it. A daemon whose events for a moved task had all reached
  the server keeps the task's record and workspace until they are
  deleted by hand.
- Only the owner's latest queued prompt reaches the new start; earlier
  prompts the owner queued while the task waited are dropped.
- A moved task waits while every connected daemon is one it ran on.
- The server keeps the highest running cost the harness reported for a
  task. A new session starts its running total again from zero, so a
  moved task's shown cost stays at the earlier session's total until the
  new session's total passes it, and undercounts what the task spent.
- A task's stored events no longer share the numbering of any one
  daemon's journal once it has moved.

Revisit if a task's work is kept off its daemon's machine, so that a
moved task could continue it instead of starting again.
