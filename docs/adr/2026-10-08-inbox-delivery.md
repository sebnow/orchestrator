---
status: proposed
date: 2026-10-08
source: >-
  owner decision, 2026-10-08;
  [agent messaging](2026-10-07-agent-messaging.md), Consequences;
  [client protocol](2026-10-07-client-protocol.md), Agent requests;
  [task lifetime](2026-10-08-task-lifetime.md), Decision
---

# Inbox delivery

## Context

[Agent messaging](2026-10-07-agent-messaging.md) gives each agent an
inbox, has the server route messages between agents, lets an agent ask
the server to spawn another, and keeps a parent from blocking on its
children. It leaves open how the inbox is exposed to the agent.
[Harness independence](2026-10-07-harness-independence.md) makes the
harness feature that carries it an implementation choice within the
daemon, and [client protocol](2026-10-07-client-protocol.md) says agent
requests travel the daemon's protocol without defining their shape.

[Task lifetime](2026-10-08-task-lifetime.md) runs one harness process
per turn. Between turns a task has no process, so the agent cannot read
its inbox. Task lifetime lists inbox messages among the prompts that
start a task's next process. An agent that polled an inbox would have
to keep its turn open, spending tokens and holding a process while it
waits.

The server composes each task's system prompt
([task interface](2026-10-07-task-interface.md)). The daemon hosts an
MCP gateway for each task's process
([human steering](2026-10-07-human-steering.md)); the gateway serves
the permission prompt and the pause acknowledgement
([graceful pause](2026-10-07-graceful-pause.md)).

The server tracks each task's state. Besides `running`, `stopped` and
`failed`, a task is `pausing` while a pause it was asked for has not
taken effect, `paused` once it has, `awaiting_permission` while the
harness waits for a permission answer, and `finished` when its process
exited after a turn with no pause in effect.

## Decision

### Exposure

The agent reaches the server through two tools on the daemon's
gateway:

- `spawn_task(prompt, model?)` starts a child task and returns its id;
- `send_message(to, text)` sends text to another task, named by its id.

The agent does not read its inbox. A message reaches its recipient as
the recipient's next prompt, so an agent that wants replies ends its
turn and is woken by them. It spends no tokens and holds no process
while it waits.

### Delivery

What happens to a message depends on the recipient's state when it is
sent:

- `finished`: the server issues it at once as a follow-up prompt, which
  starts the task's next process.
- `running`, `pausing` or `awaiting_permission`: it waits in the
  recipient's inbox. When the task next becomes `finished`, the server
  issues every waiting message as one prompt. If the task becomes
  `paused` instead, the waiting messages follow the rule for `paused`.
- `paused`: it waits until the owner resumes the task. When the resumed
  turn ends with the task `finished`, it is delivered with any other
  waiting messages. A pause is the owner's hold, and a message must not
  undo it.
- `stopped` or `failed`: the server refuses it, and the sender sees the
  error. A message to a task that does not exist, or to the sender
  itself, is refused the same way.

The server chooses the prompt's wording, for example "Message from task
X: …". When several messages are delivered together, the prompt has one
such part per message. The follow-up prompt command gains an optional
`from`, the id of the task whose message it delivers.

### Spawning

A child is an ordinary task with its parent recorded on the server. The
server places the child on the parent's daemon. The child starts in a
fresh workspace made from the parent's workspace specification, with
the parent's model unless `spawn_task` names one. The system prompt the
server composes for the child gives its parent's id and tells it to
report results to the parent with `send_message`.

When a child ends `stopped` or `failed`, the server tells its parent,
because that child will not report. The server does not announce the
end of a child's turn; the child reports explicitly, because it decides
whether it has a result worth sending.

### Wire

The daemon forwards an agent's tool call as
`POST /v1/daemons/{daemon}/tasks/{task}/requests`, with a body naming
the request's kind, `spawn` or `send`, and its payload. It relays the
server's synchronous reply, or the error text, to the agent as the
tool's result. The server accepts the request only from the daemon the
task is assigned to, and only while the task has a process running.

The protocol gains two additions:

- The request endpoint answers synchronously because the agent's tool
  call waits on it: the spawn returns the child's id, and a refused
  send must reach the sender within its turn. Events flow from daemon
  to server and get no reply, so a request that needs an answer cannot
  be an event. The path names the task, so the server checks, with the
  same daemon authentication as the other daemon routes, that the
  calling daemon holds the task.
- `from` on the follow-up prompt is the only mark on the wire that a
  prompt delivers a message. Without it the daemon and the owner's web
  interface would have to guess from the prompt's text, which the
  server words as it likes.

The start-task command does not carry the parent's id; the server keeps
it, and the server alone routes by it.

## Consequences

- An agent with children ends its turn to wait for them, and the owner
  sees a parent `finished` while its children work.
- Delivery follows state changes the server already tracks. A message
  to a running task waits for the turn to end, so a long turn delays
  it.
- A message waiting for a recipient that then stops or fails is never
  delivered, and its sender is not told. Only a parent learns when its
  child stops or fails. Resolved by change `rztyqxsy`: the server tells
  each such sender in a notice delivered as its next prompt, the way it
  tells a parent.
- A child that finishes its turn without calling `send_message` leaves
  its parent waiting until the owner steps in.
- The server's composed system prompt names the gateway's tools, so
  renaming a tool changes the server's wording as well as the daemon.
  This amends [harness independence](2026-10-07-harness-independence.md),
  under which moving a function between harness features changed only
  the daemon.
- A parent and its children share one daemon, and with it that
  machine's capacity.
- A harness may have messaging tools of its own. Claude Code 2.1.289
  has `SendMessage` and `ListAgents`, which reach other Claude sessions
  on the same machine. In the first live run, a child asked to "send it
  to your parent" called those instead of `send_message`, as its session
  file showed. Its result reached the owner's interactive session, and
  the parent waited for a report that never came. The daemon's Claude
  Code adapter therefore denies both tools to tasks, and the system
  prompt says that only the orchestrator's tools reach other tasks.

Revisit when the server places tasks by a scheduler, or if agents need
to read messages within a turn.
