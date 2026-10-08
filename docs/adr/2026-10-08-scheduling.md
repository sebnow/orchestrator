# Scheduling

- Status: Proposed
- Decided: 2026-10-08
- Source: decided by the coordinating agent under the owner's standing
  instruction, 2026-10-08, for the owner's review;
  [budget and pause](../design/2026-10-07-budget-and-pause.md), Idea;
  [graceful pause](2026-10-07-graceful-pause.md), Context and Decision;
  [task lifetime](2026-10-08-task-lifetime.md), Decision;
  [inbox delivery](2026-10-08-inbox-delivery.md), Delivery and Spawning;
  [harness billing](2026-10-07-harness-billing.md), Consequences;
  [harness adapter](2026-10-07-harness-adapter.md), Context and
  Consequences

## Context

The owner's subscription limits tokens per five-hour and weekly window,
and every daemon spends from the same account
([harness billing](2026-10-07-harness-billing.md)). Anthropic documents
limits on tokens per window, and none on concurrent sessions per
account. The [budget and pause](../design/2026-10-07-budget-and-pause.md)
note asks for priorities, and for filler tasks, which spend budget that
would otherwise go unspent and give way as soon as other work can run.

The server issues every command the moment it is asked to. An owner's
task starts on the daemon the owner names, a spawned child on its
parent's daemon, and a message to a finished task becomes its next
prompt at once ([inbox delivery](2026-10-08-inbox-delivery.md)). Nothing
limits how many tasks run on a daemon or on the account.

Under [task lifetime](2026-10-08-task-lifetime.md), each turn runs in its
own harness process. A task's start, an owner's follow-up prompt, a
resume, and a delivered message each launch a harness process if the
task has none running. A turn is therefore the smallest unit of work
that can be held back without interrupting a model request in progress.

The only quota figures the server has are the daemons' `quota_observed`
events, taken from Claude Code's undocumented `rate_limit_event`
([harness adapter](2026-10-07-harness-adapter.md)). Each carries a status
(`allowed`, `warning`, `rejected` or `unknown`) and, per window, its
utilization from 0 to 1 and when it resets. Claude Code sent the event
once per process in every run of the
[mod versus stdout spike](../design/2026-10-07-mod-vs-stdout-spike.md),
so a reading is only as fresh as the newest turn on any daemon.

[Graceful pause](2026-10-07-graceful-pause.md) stops a task at a point
the agent chooses, and a resume continues it in a new process. In the
[graceful pause spike](../design/2026-10-07-graceful-pause-spike.md),
pausing and resuming cost 4 to 15 percent more, in Claude Code's
reported `total_cost_usd`, than the same work run uninterrupted.

## Decision

### Turns are queued and admitted

The unit of scheduling is a turn. A start, an owner's prompt, a resume,
or the delivery of waiting messages creates a pending turn, which the
server keeps in one queue for all daemons. A scheduler on the server
admits pending turns. Admitting a turn issues the command the server
would otherwise issue immediately, so the daemon protocol does not
change. Pause, interrupt, stop and permission answers are issued
immediately, without queueing.

### Priority and filler

Each task has a priority, `low`, `normal` or `high` (default `normal`),
and a filler flag. The scheduler considers pending turns in priority
order, oldest first within a priority, and admits each one that has a
slot and the budget's leave. A turn that cannot be placed does not hold
up later turns that can. Filler turns are ordered among themselves the
same way, after every non-filler turn, and none is admitted while a
non-filler turn waits only for a free slot.

### Placement and slots

The server keeps a slot count per daemon. Its default is set in the
server's configuration and ships as two. A task holds a slot from the
admission of its turn until its process exits: while it is `pending`,
`running`, `awaiting_permission` or `pausing`. Queued, `finished`,
`paused` and `yielded` tasks hold none.

Placement happens when a task's first turn is admitted. The turn goes
to the daemon the owner named, if any, otherwise to the connected daemon
with the most free slots, ties broken by the daemon's id. Every later
turn goes to the task's daemon, where its workspace is. A spawned child
goes to its parent's daemon, as
[inbox delivery](2026-10-08-inbox-delivery.md) decided, unless that
daemon has no free slot at admission; then the server places it as it
would a fresh task. Either way the child starts in a fresh clone.

### Budget

The windows belong to the account, so the newest `quota_observed` from
any daemon is the account's reading. The rules:

- Status `rejected` admits nothing until the earliest `resets_at` among
  the reading's windows. After that the reading no longer applies, and
  the budget is unknown until a new one arrives.
- A filler turn needs the five-hour window's utilization below a filler
  threshold (default 0.5).
- A `low` turn needs it below a low threshold (default 0.85).
- `normal` and `high` turns are stopped only by `rejected`.
- Statuses `allowed`, `warning` and `unknown` are treated alike; only
  the utilization counts.

The weekly window counts only through `rejected`. Once a window's
`resets_at` passes, the server ignores that window's figures. With no
reading, or none for the five-hour window, the budget is unknown. An
unknown budget admits every turn except filler, which needs a reading
below the filler threshold. The owner's interface shows how old the
reading is.

### Yielding

When a non-filler turn is waiting for a slot on a daemon that has none
free, the scheduler pauses one running filler task there, the one whose
current turn was admitted most recently, through the ordinary graceful
pause. A turn not yet placed waits on the connected daemon with such a
filler task. A task the scheduler paused is `yielded`, not `paused`: the
scheduler queues its resume as a filler turn, whereas an owner's pause
waits for the owner. The owner may resume a `yielded` task, which makes
that resume an ordinary turn rather than a filler one.

### Observable

A task with a turn waiting shows as queued in the owner's interface,
with its place in the queue and the reason it waits: no free slot, work
of higher priority waiting, the budget, the daemon not connected, or
`rejected` until a given time.

### Rejected alternatives

- Scheduling on the daemon. The budget is the account's, shared by every
  daemon, so only the server sees all of it.
- A fixed number of concurrent tasks per account. Concurrency does not
  track token use, so a fixed number would leave budget unspent or
  overrun it. Slots limit the load on each daemon; the budget rules
  limit spending.
- Yielding tasks other than filler. Each yield costs a pause and a
  resume, and an owner who started ordinary work does not expect it to
  stop on its own.

## Consequences

- An owner's follow-up prompt, a message delivery, and a child's start
  can wait for a slot or for budget. The owner sees why; an agent that
  spawned a child does not.
- Filler depends on readings. A server that has run no turn since the
  five-hour window reset holds filler until a non-filler turn brings a
  fresh reading. **UNVERIFIED:** that `rate_limit_event` arrives often
  enough to keep the reading current; the spike saw one per process.
- A filler task waits behind any non-filler turn that could run but for
  a slot, so a steady stream of ordinary work keeps filler from running,
  as the budget and pause note asks.
- When the weekly window is the one that rejected, the five-hour reset
  lifts the hold early, and the next turn spends a process on learning
  that the account is still rejected.
- A yield costs what a pause costs, and depends on the model stopping
  when asked. If the agent does not stop within the pause limits that
  [graceful pause](2026-10-07-graceful-pause.md) sets, the daemon
  interrupts it.
- A turn for a daemon that is not connected waits until it connects, so
  a task bound to a daemon that never returns stays queued until the
  owner stops it.
- **UNVERIFIED:** that two concurrent sessions per daemon, and the 0.5
  and 0.85 thresholds, suit the owner's subscription plan; none was
  measured.

Revisit if Claude Code documents a quota interface, or Anthropic
documents a session cap per account.
