---
status: accepted
date: 2026-10-10
source: >-
  owner decisions, 2026-10-10: models as an ordered list of
  harness-specific identifiers; effort per agent; tool restrictions as
  neutral classes mapped by each harness adapter, failing closed;
  capacity reported by the daemon as slots derived from memory and
  load; the orchestrator, not the owner, tells agents how its tools
  and workspaces behave
---

# Agent models, effort, tool classes and daemon capacity

## Context

Agent definitions ([agents and placement](2026-10-09-agents-and-placement.md))
name one `model` and a list of gateway tools (`spawn_task`,
`send_message`) the agent's tasks may call. The owner wants to run the
same agent on more than one harness, Claude Code today and Codex, Pi
or others later, where the same model has a different identifier per
harness and per provider behind it (Anthropic, Bedrock, OpenRouter). A
single `model` string cannot name a model across that range.

The owner's own agent roles need a scout that cannot edit and a junior
that cannot spawn further work. An agent's gateway tools already gate
`spawn_task` and `send_message`, but nothing gates the harness's own
tools, such as its file editor or shell; only the harness itself can
enforce that kind of restriction, through whatever mechanism it offers
for the purpose.

[Scheduling](2026-10-08-scheduling.md) fixes a slot count per daemon
through a server flag, defaulting to two. That record's own
consequences call the default and the flag "starting points, not
measured against the owner's plan", a stand-in for a real measurement
of what a daemon can carry.

The first dogfood agent definitions had to explain, in their own
system prompts, how `spawn_task` works and that a child's report
arrives as the next prompt after its turn ends. That explanation is
the orchestrator's own mechanism, not a judgment call that belongs to
a specialist's role.

[Harness independence](2026-10-07-harness-independence.md) keeps
harness-specific names inside the daemon's adapter, so that another
harness can be substituted later. Models as harness-specific
identifiers, tool restrictions as a harness's own mechanism, and
capacity reported by the harness's own machine all have to be carried
through that boundary without the server or the owner having to know
a harness's names for them.

## Decision

### Models

An agent definition lists acceptable models in order of preference.
Each entry is a model identifier exactly as a harness accepts it,
optionally qualified by harness name (`claude-code:fable`); an
unqualified entry matches any harness. A daemon advertises the models
it provides as names, through an owner-set label `models` merged with
any fact its adapter reports; the orchestrator keeps no table of
equivalent names across harnesses or providers.

Placement chooses the first entry in the agent's list that some
eligible daemon advertises, and records the chosen model on the task.
An agent none of the connected daemons can serve waits like an unmet
`requires`, and the GUI says which model it waits for. This replaces
the single `model` field of the agents record.

### Effort

An agent definition carries an effort on a neutral scale (`low`,
`medium`, `high`, `max`), which each harness adapter maps to its own
levels. The task carries the effort to the daemon alongside the
chosen model.

### Tool classes

An agent definition may restrict the harness tools its tasks get, as
a list of neutral classes: `read` (files and search), `edit`, `shell`,
`web`, `subagents`, `mcp`. Leaving the list unset means every tool.
Each harness adapter maps the classes to its own harness's tools and
applies the restriction through that harness's own mechanism,
preferring an allow-list form where the harness has one. Gateway
tools, `spawn_task` and `send_message`, are unaffected and stay as
they are.

The adapter fails closed: it compares the tools its harness advertises
at start against its mapping, and when a restricted agent's harness
advertises a tool the adapter cannot classify, the daemon stops the
task before its first turn and reports the unclassified tool names. An
unrestricted agent runs regardless of what its harness advertises.

### Capacity

A daemon reports its capacity as a fact, `slots`, derived from free
memory and load average, recomputed on a timer and again whenever a
harness starts or exits on it. The scheduler uses the reported number
as the daemon's capacity; an owner label `slots` caps it, following
the same owner-over-fact rule as the daemon's other labels and facts.
The server-wide slots flag is removed. This replaces the slot clause
of [scheduling](2026-10-08-scheduling.md) alone; the rest of that
record, including priority, filler, yielding and the budget rules,
stands.

### Mechanics in the prompt

The orchestrator composes, for every task, the part of the system
prompt that describes its own mechanisms, built from the task's tools
and its parent: the workspace, its branch, the push at turn end and
the mirror as `origin`; for a task that may call `spawn_task`, how it
works, that a child's report arrives as the next prompt after the
turn in which it was sent ends, and the agents the task may name,
with their descriptions; for a task that may call `send_message`,
what it reaches; for a child task, who spawned it and for what
purpose. An agent definition carries only the role, its judgment
rules and its reporting shape; it no longer explains the
orchestrator's own mechanisms. Agent definitions are the owner's and
live in the server, not in this repository.

## Consequences

- `agents.model` becomes a list, and `effort` and `tool_classes`
  columns are added to the agents table; this is a schema migration.
- The daemon protocol's `StartTask` carries the chosen model, the
  effort, and the tool classes, alongside the gateway tools it
  already carries.
- The Claude Code adapter's class mapping (`read`: Read, Grep, Glob;
  `edit`: Edit, Write, NotebookEdit; `shell`: Bash; `web`: WebSearch,
  WebFetch; `subagents`: Agent, Task; `mcp`: the tools of configured
  MCP servers) lives in `internal/harness/claude`, the one place to
  update when Claude Code renames or adds a tool.
- Because the adapter fails closed, a Claude Code rename that falls
  outside the mapping becomes a reported stop for restricted agents,
  not a silent widening of what they can do.
- The dashboard shows each daemon's reported `slots`, and the owner's
  `slots` label can lower what it shows; `-slots-per-daemon` is
  removed from the server's flags.
- The owner's agent definitions get shorter: a role, its judgment and
  its report, without the orchestrator's own mechanisms repeated in
  every one of them.

Revisit if a harness needs a tool restriction finer than the six
classes, or if the reported `slots` figure proves a poor measure of
what a daemon can actually carry.
