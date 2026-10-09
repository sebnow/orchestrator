---
status: accepted
date: 2026-10-09
source: owner decision, working session on 2026-10-08
---

# Agents and placement

## Context

The owner wants a coordinating agent, a "brain", that starts specialist
agents as child tasks on whichever daemons suit them, receives their
results, and leaves the owner able to see the whole tree of tasks.
Before this record the owner kept such specialists as files of
instructions, one per kind of agent.

Before this record every task got the same system prompt composition
and the same tools. The server composes each task's system prompt from
its own instructions on reaching other tasks and whatever the owner's
request adds ([task interface](2026-10-07-task-interface.md),
[inbox delivery](2026-10-08-inbox-delivery.md)). The daemon hosts an
MCP gateway for each task's harness process, the Claude Code CLI
([human steering](2026-10-07-human-steering.md),
[harness adapter](2026-10-07-harness-adapter.md)). Through it every
task is offered `spawn_task(prompt, model?)` and
`send_message(to, text)`, called the agent tools below, besides the
gateway's permission and pause tools. A child starts with its parent's
model, workspace specification, pause limits, priority and filler
flag.

[Scheduling](2026-10-08-scheduling.md) places a task's first turn on
the daemon the owner named, or on the connected daemon with the most
free slots, and a child on its parent's daemon when that has a free
slot. The server does not know a daemon's operating system, its size,
or whether it has a GPU, so a task that needs such a machine can only
be bound to a daemon by name.

Under inbox delivery a child reports by calling `send_message`, and the
server does not announce the end of a child's turn. One of that
record's consequences is that a child that finishes its turn without
sending leaves its parent waiting until the owner prompts one of them.

## Decision

### Agent definitions

The server keeps agent definitions, called agents in the owner's
interface and API. An agent has:

- `name`, unique, with the characters a task id may have; it does not
  change once the agent exists;
- `description`, for the owner and for the tasks that may spawn it;
- `system_prompt`;
- `model`, optional;
- `tools`: which agent tools its tasks may call, none when the list is
  empty or left out. The permission and pause tools are always
  available;
- `pause_limits`, optional, defaulting to the task defaults;
- `priority` and `filler`, the defaults for its tasks, `normal` and
  not filler unless given;
- `requires`, a set of `key=value` labels (below).

The owner creates, edits and deletes agents in the GUI and through
`/v1/agents` (list, get, create, update, delete) on the owner API. The
server refuses to delete an agent that any task names. The GUI form
has exactly the agent's fields; to reuse an existing instructions file,
the owner pastes its contents into the system prompt field.

A task may name an agent: in the owner's `POST /v1/tasks` and the GUI's
new-task form, and as `spawn_task(agent?, prompt, model?, requires?)`.
The task records the agent's name, and its page and the dashboard show
it. The server composes the task's system prompt from the server's
instructions on reaching other tasks, then the agent's system prompt,
then what the owner's request adds. The instructions of a task that may
call `spawn_task` list every agent by name and description. A spawn
naming an agent that does not exist is refused, and the calling task
is told why.

A task that names an agent takes from it each setting the request
leaves out: model, pause limits, priority, filler flag and `requires`.
For a child, the agent's settings come before the parent's, which fill
in a model or pause limits the agent leaves out; a child still works
from its parent's workspace specification. A child that names no agent
has none, is allowed both agent tools, and inherits from its parent as
the Context describes.

The daemon offers a task only the agent tools its agent allows, and the
list of tools the harness lets the task call without asking follows the
same list. The start-task command carries the list in an optional
field, `tools`; a start-task command without it allows both tools. The
server's instructions to the task name only the tools it is allowed.

### Labels, facts and placement

A daemon has labels the owner sets and facts the daemon reports, both
`key=value`; where both set a key, the owner's label wins. The owner
sets labels on the daemon in the GUI, and the server keeps them. The
facts are:

- `os` and `arch`, the operating system and architecture the daemon
  was built for;
- `cpus`, the number of logical CPUs;
- `memory`, the machine's memory in bytes, where the daemon can read
  it;
- `harness` and `harness_version`, the harness's name and version;
- `gpu`: `nvidia` when `nvidia-smi` is on the daemon's `PATH`, `apple`
  on darwin/arm64, and absent otherwise.

The daemon reports its facts with `PUT /v1/daemons/{daemon}/facts`,
whose body is a JSON object of string values, each time it opens its
command stream: once at start and again after each reconnect. The
request replaces the daemon's facts, so repeating it changes nothing.
It is authenticated as the daemon's other routes are.

A task's `requires` comes from its agent. A `requires` given when the
task is created or spawned replaces the agent's whole set. Before the
scheduling record's placement rule applies to a task's first turn, the
server narrows the connected daemons to those whose merged labels hold
every required `key=value`, including when the owner named the daemon.
A child still goes to its parent's daemon first when that daemon
qualifies. A task for which no connected daemon qualifies stays queued,
and its reason names the pairs that no connected daemon has, as "no
daemon has `key=value`".

### Hand-back

When a child task's turn ends with the task `finished`, and the child
has not called `send_message`, to any task, since that turn started,
the server sends the child's final assistant text of the turn to its
parent as a message from the child. When the turn wrote no text, the
message states that the child finished without writing any. The
message is marked as a hand-back: the child's transcript shows its
reply handed back, and the parent's shows it as a report from the
child. Only child tasks hand back; the owner's tasks have no parent to
report to.

### Relation to earlier records

This record adds one endpoint to the daemon protocol of
[client protocol](2026-10-07-client-protocol.md), the facts endpoint,
and one optional field to its start-task command. It adds two optional
fields, `agent` and `requires`, to the spawn request of inbox delivery.
`/v1/agents`, the daemon labels and the new fields of `POST /v1/tasks`
belong to the owner API, not to the daemon protocol. The decisions of
client protocol stand.

Hand-back changes the rule in inbox delivery's "Spawning" section that
the server does not announce the end of a child's turn. A child that
finishes its turn without sending now reports through the hand-back. A
child that stops or fails is still announced to its parent by the
server's notice, as inbox delivery decided, and a child the owner
pauses still waits for the owner. The rest of inbox delivery stands.

The filter by labels narrows the daemons that the scheduling record's
placement rule chooses from, and that rule stays as it was.

This record supersedes none of these records.

### Rejected alternatives

- Typed daemon properties, such as a number of CPUs that `requires`
  could compare against. Matching strings needs no type per key, and
  the owner sets a label where a comparison would matter.
- Facts entered by the owner alone. A reported fact is refreshed every
  time the daemon connects, while one the owner typed stays as it was
  until the owner edits it.
- Announcing every end of a child's turn to its parent. A child that
  has already sent its result would report twice.

## Consequences

- A specialist is described once and used by name, by the owner and by
  the tasks allowed to spawn.
- An agent's tools limit whether its tasks can spawn or message other
  tasks, so a specialist can be kept from starting further work.
- A `requires` that no daemon will satisfy, such as a misspelt label,
  keeps its task queued until the owner stops it; the reason it shows
  names the label.
- Matching is exact, so `memory` or `cpus` can be required only as an
  exact value; a requirement on size needs a label the owner sets.
- A daemon that does not report facts, such as one whose version
  predates the facts endpoint, has only the owner's labels.
- A fact reflects the machine when the daemon last connected, so a
  harness upgraded since then shows its old version until the daemon
  reconnects.
- The hand-back is the child's last text, which may be a closing remark
  rather than its result. A child that wants to choose its report can
  still send it with `send_message`.
- The server does not delete tasks, so an agent that any task has
  named, ended tasks included, stays. The owner can still edit it,
  which changes only the tasks started afterwards.

Revisit if placement needs comparisons rather than exact matches, or if
hand-backs prove too noisy for parents.
