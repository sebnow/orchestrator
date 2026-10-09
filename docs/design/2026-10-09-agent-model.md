# The agent model: tasks, subagents, facts and the hand-back (2026-10-09)

The owner wants a "brain" agent that starts specialist agents on the
daemons that suit them, hears their reports, and lets the owner see
the whole tree. [Agents and placement](../adr/2026-10-09-agents-and-placement.md)
records what was decided. This note records why, as it stood on
2026-10-09.

Markers: **UNVERIFIED** flags a claim not checked against a source
named here or against the code or tests of the repository.

## Two ways an agent can start another

An agent run by the orchestrator can start another agent in two ways.

- **A task.** The agent calls the gateway's `spawn_task`. The child is
  a task like any other: the server keeps it, schedules its turns,
  places it on a daemon, stores its transcript, and lets the owner
  pause, resume, prompt or stop it. Its report reaches the parent as a
  prompt once the parent's turn has ended
  ([inbox delivery](../adr/2026-10-08-inbox-delivery.md)).
- **A native subagent.** Claude Code's own `Agent` tool (listed as
  `Task` in the tool list of the 2.1.289 runs in
  `spikes/mod-vs-stdout/runs`) runs a subagent inside the same harness
  process. Its result comes back to the caller as the tool's result,
  within the same turn.

The orchestration unit is the task. Only a task is something the
scheduler can hold back for budget or a slot, place on a machine that
fits it, move off a lost daemon, or pause on the owner's word. A native
subagent shares its caller's process, slot, daemon and turn; the
scheduler cannot tell it is there. Work that needs a GPU, or that
should run while its parent waits without the parent holding a slot,
has to be a task.

Native subagents are allowed. **UNVERIFIED:** they are cheap for short
lookups that belong to the caller's turn. Without
`--forward-subagent-text`, their messages reach the transcript only as
tool calls and results, interleaved with the caller's. Claude Code
forwards a subagent's text and thinking too when it runs with
`--forward-subagent-text`, and marks each message of a subagent with
the id of the tool call that started it, in `parent_tool_use_id`
(<https://code.claude.com/docs/en/headless.md>, "Follow subagent
messages"; the flag needs Claude Code 2.1.211 or later). The daemon
passes the flag, the normaliser keeps the id on every transcript
entry it makes from such a message, and the task page nests those
entries under the tool call, folded, titled by the call's description.

None of the runs in `spikes/mod-vs-stdout/runs` started a subagent,
so the normaliser's fixture,
`internal/harness/claude/testdata/subagent.jsonl`, was written by hand
from the shape that page documents. The live run described under "The
hand-back" below was not set up to start one. **UNVERIFIED:** that
Claude Code 2.1.289 writes subagent messages in exactly the fixture's
shape, such as the first `user` message carrying the subagent's prompt
as a `text` block.

## Agents as records, not files

The owner keeps specialists as instruction files. The server keeps an
agent definition instead, because the server is what composes a task's
system prompt, decides its tools and places it; a file on the owner's
machine reaches none of those. The definition's fields are its name and
description, and what the server acts on: a system prompt, a model,
which of the gateway's agent tools (`spawn_task`, `send_message`) its
tasks may call, pause limits, priority, the filler flag of
[scheduling](../adr/2026-10-08-scheduling.md), and the labels it
requires. An existing file is imported by pasting it into the system
prompt field. A brain learns which agents exist
from its system prompt, which lists them by name and description when
the brain may spawn.

An agent's `tools` decide whether its tasks may spawn or message. A
specialist that should only do its job gets neither, so it cannot
start tasks or message them; it reaches its parent only through its
report. It can still run native subagents within its own turn.
Leaving `tools` out allows none, so that the owner grants these powers
on purpose.

## Facts are reported, not entered

Placement matches a task's required `key=value` labels against a
daemon's labels. Some of a daemon's labels describe the machine: its
operating system, architecture, CPUs, memory, harness and GPU. The
owner could have entered them, or they could have been modelled as
properties with types that a task could compare against ("at least 16
GiB").

The daemon reports them instead. It can read them, the owner would
have to look them up for each machine, and a reported fact is refreshed
each time the daemon connects, where an entered one goes stale when
the machine changes, such as after a harness upgrade. They are strings
rather than typed values because matching strings needs nothing per
key: the server need not know what `gpu` or `memory` mean to place a
task by them. A comparison the owner cares about becomes a label the
owner sets, such as `size=large`. An owner's label wins over a fact
with the same key. The cost is that `memory` and `cpus` are matched exactly, which
makes them useful to read on the daemon's page but rarely worth
requiring.

GPU detection reports only the vendor: `nvidia` when `nvidia-smi` is
on the daemon's `PATH`, `apple` on darwin/arm64. Anything finer, such
as the model or the memory of the card, is for the owner's labels.

## The hand-back

Before the hand-back, a child could report only by calling
`send_message`, and a child that ended its turn without calling it left
its parent waiting for a report that would never come. In the first live run of
messaging, a child called Claude Code's own `SendMessage` instead
([inbox delivery](../adr/2026-10-08-inbox-delivery.md), Consequences),
which shows that an agent can miss the one channel that reaches its
parent. A specialist whose agent does not allow `send_message` could
not report to its parent at all.

The rule: when a child's turn ends with the task `finished` and the
child has not sent any message since that turn started, the server
sends the turn's last main-conversation text to the parent, marked as a
hand-back. A child that sent a message has chosen its report, so
nothing more is sent; sending both would make the parent read the
result twice. Text that a native subagent wrote does not count as the
child's reply. A turn with no text at all still hands back a notice
that it finished without any, so that the parent learns the turn has
ended. A child that stops or fails is announced by the server's
notice, and a paused child waits for the owner.

One live run on 2026-10-09 exercised the hand-back of a finished
worker's reply:
`TestLiveGivenParentAgentThatSpawnsAWorkerThatCannotMessageWhenTheWorkerFinishesThenItsReplyIsHandedBackAndTheParentAnswers`
in `cmd/daemon/live_test.go`, with Claude Code 2.1.289 and model
`haiku` on the owner's macOS workstation. A parent agent allowed only
`spawn_task` started a worker agent allowed no agent tools and
requiring a label that only the owner had set on the one daemon, so
that the worker's placement depended on an owner's label. The
worker's only turn replied `PLUM` and ended; the server handed the
reply back, and the parent's next turn, resumed in the same session by
the hand-back, replied `PLUM`. The daemon started `claude` three
times besides `claude --version`, and the test passed.

**UNVERIFIED:** the last text of a turn is usually the agent's answer,
since an agent tends to finish with its result. It may also be a
closing remark that does not carry the result. The child's system prompt therefore tells
it that its final reply is handed back, so it should end its turn with
its result.
