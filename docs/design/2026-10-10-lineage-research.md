# Grouping, lineage and durable context in agent products (2026-10-10)

The owner asked what Claude Projects, Grok, Cursor, pi durable and Amp
do about grouping tasks, lineage of spawned agents, and durable
context, and what relates to this project (tasks with a parent id, a
children table, hand-back, messaging, agent definitions, a workspace
per task). This note records the survey. Much of it comes from
secondary sources, named per item.

## Claude Projects

Workspaces bundle conversations with uploaded knowledge files and
custom instructions, so context is not re-explained
(<https://www.educative.io/courses/claude-ai-guide/claude-projects-and-memory>,
a tutorial site). Claude Code stores a conversation tree with
`uuid`/`parentUuid` for forks on retry or edit
(<https://skywalking.apache.org/docs/skywalking-ai-sessionizer/next/en/adapters/claude-code-glossary/>).
**UNVERIFIED** that any lineage UI exists. A "coordinator" dividing
work across parallel threads is described at
<https://thenewstack.io/claude-code-parallel-projects/> (press); the
division logic is not stored as metadata.

## Grok Projects

Dedicated workspaces with custom instructions (4,000 characters), model
selection, uploaded source files, and AGENTS.md auto-loaded
(<https://storylane.io/tutorials/how-to-use-grok-projects>, a tutorial
site). **UNVERIFIED**: no lineage mechanism found. `--fork-session` when
resuming branches a session.

## Cursor

Background agents run on cloud VMs on separate branches, pushing PRs.
Subagents with isolated context are spawned through a Task tool whose
prompt carries the informal reason; each returns an agent id for
resumption; up to 8 parallel agents per workspace. Side chats get the
parent history as hidden context
(<https://cursor.com/docs/subagents>,
<https://cursor.com/docs/agent/subagents>,
<https://www.learncursor.dev/learn/cursor-agents/side-chats>).
**UNVERIFIED**: no parent-child tree view, no stored spawn reason.

## pi durable

An experimental runtime, separate from pi 1.0, for persistent
multi-conversation applications: committed message history,
transcripts, request manifests, progress. One durable session exists
per run id; JSONL is stored under `sha256(runId)`, with SQLite or
custom backends under exclusive ownership; checkpoint recovery is
supported
(<https://letsdatascience.com/news/pi-10-adds-mcp-and-durable-harness-6241c6f7>,
<https://cdn.jsdelivr.net/npm/pi-fabric@0.109.6/docs/durable-pi.md>).
**UNVERIFIED**: no parent-child or spawn chain tracking found.

## Amp

Amp has workspaces (members, threads, projects), projects (codebase
plus orb size, secrets, setup scripts, shipping behaviour), threads
(durable conversations), and orbs (cloud sandboxes that pause and
resume). Thread Map draws threads as graph nodes with edges typed
reference, continuation, or handoff (`threads: map` in the CLI).
AGENTS.md from the working directory, parents, system locations, and
workspace defaults load into every thread. Thread visibility is
unlisted/workspace/group/private
(<https://ampcode.com/docs/projects>,
<https://ampcode.com/docs/collaborate/workspaces>,
<https://hackernoon.com/sourcegraph-spinoff-amp-rolls-out-thread-map-to-manage-agent-workflows>,
<https://hackernoon.com/too-many-agent-chats-amps-new-thread-map-shows-what-connects-to-what>,
press). **UNVERIFIED**: whether a spawn reason is stored, whether
threads fork explicitly.

## Capabilities and who has them

- Task tree with spawn reasons: none store a reason as data; Cursor's
  prompt parameter is informal; Amp types the edges post hoc.
- Project-level instructions and knowledge inherited by all runs:
  Claude, Grok, and Amp — yes; Cursor — no; pi — UNVERIFIED.
- Durable sessions that survive crashes: all, in different forms.
- Lineage visualisation: Amp's Thread Map; Claude Code's tree without a
  documented UI; the rest — none found.
- Forking: Claude Code's conversation tree, Grok's `--fork-session`;
  others UNVERIFIED or not found.
- Parallel children with shared state: the Claude Code coordinator
  account; Cursor — isolated; Amp — UNVERIFIED.
- Sibling messaging: none, as a channel.
- Workspace-wide configuration: Amp — extensive; Claude and Grok —
  instructions and files; Cursor — none.

## Correspondence to this project

- Thread or conversation → task.
- Project or workspace → nothing yet.
- Instructions and knowledge files → agent definition system prompts
  only, nothing per repository.
- Orb → the task's workspace on a daemon.
- Handoff edge → hand-back.
- Reference and continuation edges → nothing.
- Spawn reason → the child's prompt only.

These gaps are the owner's open decision at the time of writing.
