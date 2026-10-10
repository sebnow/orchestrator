---
status: accepted
date: 2026-10-10
source: >-
  owner decisions, 2026-10-10, after the survey in
  docs/design/2026-10-10-lineage-research.md and Cursor's Projects
  announcement (<https://cursor.com/blog/projects>)
---

# Projects and lineage

## Context

Tasks are flat today: each is created with its own repository URL,
prompt and agent ([agents and placement](2026-10-09-agents-and-placement.md)).
A spawned task records its parent and the parent's page lists its
children, but the only statement of why a child exists is its prompt,
and nothing above tasks holds instructions or a repository for reuse.

The first dogfood run (`docs/design/2026-10-09-dogfood-run.md`, finding
10) showed agents working without the owner's conventions because
nothing carried them: the repository had no `CLAUDE.md`, and neither
agent in the run had a way to pick up such conventions even if the
repository had one.

The survey in `docs/design/2026-10-10-lineage-research.md` and Cursor's
Projects announcement show the products surveyed converging on a
reusable container above conversations: Claude and Grok Projects give
every run shared instructions and uploaded files; Amp's projects and
workspaces do the same and add setup scripts and shipping behaviour;
Cursor's Projects sync files across every agent a project uses, add a
coordinator agent that directs the others, and can run the coordinator
on a schedule or in response to a Slack channel or a repository's pull
requests. The same products show only weak lineage: Amp draws a Thread
Map of typed edges between threads, but none of them stores a spawn's
reason as data, and the survey found no parent-child tree view at all
in Claude Code, Grok or Cursor.

## Decision

### Projects

A project is a reusable, long-lived container the owner defines: a
name, instructions, an optional repository (URL and start ref), and an
optional default agent. Projects are not goal-scoped; a goal is a root
task and the tree beneath it. A task belongs to at most one project. A
task in a project takes the project's repository unless it has none; a
task without a project may name its own repository or run in an empty
workspace, as today. A project without a repository gives its tasks an
empty workspace.

Project instructions are appended to the system prompt of every task
in the project, after the agent definition's prompt, so the agent's
role comes first and the project's conventions second.

### Spawn purpose

A spawn states its purpose: `spawn_task` requires a one-line `purpose`,
why the child exists and what the parent expects back. It is stored on
the child, shown wherever the child is listed, and placed at the top of
the child's prompt. Tasks the owner creates may carry a purpose but
need not.

A child belongs to its parent's project.

### The tree

The tree is first-class: a view rooted at any task shows its
descendants with purpose, state, cost and branch; a project page lists
its root tasks. The owner API exposes `project`, `parent` and `purpose`
on a task and a subtree endpoint rooted at a task.

### Deferred

Not decided here: shared project memory and files, a standing
coordinator per project, recurring work on a schedule (the owner wants
it; the mechanism is a later record), credential grants and placement
labels on projects.

## Consequences

- A `projects` table and a `project` column on tasks.
- `Spawn.Purpose` is a protocol addition on the daemon-to-server
  request ([inbox delivery](2026-10-08-inbox-delivery.md), "Wire"),
  justified as the lineage record.
- The new-task form and `POST /v1/tasks` take a project.
- The system prompt is composed from the agent definition, the
  project's instructions, then the task, extending the composition
  [agents and placement](2026-10-09-agents-and-placement.md) decided.
- The children table and the dashboard show purpose.
- Nothing changes for tasks without a project.

Revisit if the owner wants shared project memory, a standing
coordinator, or recurring work scheduled, since each needs its own
record.
