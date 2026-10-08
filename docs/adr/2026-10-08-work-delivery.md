---
status: accepted
date: 2026-10-08
source: owner decision, working session on 2026-10-08
---

# Work delivery

## Context

A task with a repository works in a clone on its daemon's machine. Until
now nothing took the work out of that clone: the daemon cloned public
repositories over https without credentials, and nothing pushed. The
owner could see what an agent said it did, but not get the result.

[Daemon loss](2026-10-08-daemon-loss.md) treats the work a lost daemon
held as gone, and states as a consequence that a moved task starts again and loses
everything it did on the lost daemon. This record
narrows that consequence: commits the lost daemon pushed survive, and a
moved task continues from them. It does not supersede the daemon loss
record, whose rules for declaring a daemon lost and moving its tasks
stand.

[Task credentials](2026-10-07-task-credentials.md) requires that task
credentials not be easily reachable by the agent and leaves the
mechanism undecided. Pushing needs credentials on the daemon's machine.

The [delivery research](../design/2026-10-08-delivery-research.md)
looked at how eight coding agents deliver their work: each pushes a
branch of its own, with a prefix, and none pushes the default branch.

## Decision

### Where the work goes

Work leaves the daemon by the daemon pushing a git branch to the task's
repository. The server receives only events about the push, not the
work itself.

Each task with a repository works on the branch `orchestrator/<task id>`.
The daemon creates it when it clones, at the task's ref. When the remote
already has that branch, as it does for a task moved off a lost daemon
that had pushed, the daemon checks the branch out from the remote
instead, so the new session continues from the pushed commits.

The daemon pushes the branch at the end of every turn, when the branch
holds commits the remote's branch lacks, and once more before it deletes
the workspace. The daemon pushes without force. When the push before deletion fails,
the daemon keeps the workspace and reports the failure.

### Guarding the push

The agent is told to commit its work with clear messages and never to
push, because the daemon pushes for it. A hook in the clone refuses to
push any ref but the task's branch. The daemon's own push refuses a
branch that is the task's start ref or the remote's default branch, so
the daemon never pushes the default branch.

### Credentials

The credentials the push needs are assumed present on the daemon's
machine, such as an ssh key in the daemon user's ssh setup.
Provisioning them is not decided here; task credentials leaves the
mechanism open. git runs with the user's and the system's git
configuration ignored and its prompts turned off, so that the owner's
URL rewrites and credential helpers do not apply; ssh authentication
comes from the daemon user's own ssh setup, which git uses regardless
of its configuration. The daemon accepts repositories given as https,
ssh, or scp-like ssh addresses, and refuses local paths, `file://` and
`git://`.

### Telling the owner

A new event, `branch_pushed`, reports each push: the branch, the commit
pushed, how many commits the branch holds beyond the task's start ref,
how many files the workspace left uncommitted, and the push's error, if
it failed. The server stores it like any event and shows the task's
branch, its latest commit and any push error on the task page and in
the dashboard's task list. The daemon chooses the branch name, and
the server shows the name the event reports.

### Rejected alternatives

- Uploading the work, as a diff, to the server. The owner does not want
  the work's contents sent to the server.
- Letting the agent push. The daemon could not control which branch
  the work lands on.
- Opening pull requests. That needs each hosting service's API; it may
  come later, on top of the pushed branch.

## Consequences

- Pushed work survives the daemon. A moved task's pushed commits are on its
  branch; only work that was never pushed is lost with the daemon, along
  with its harness session.
- Work the agent leaves uncommitted is not delivered. The event's count
  of uncommitted files shows when that happens.
- The agent runs as the OS user whose ssh key the daemon pushes with, so
  the agent can read that key and push elsewhere, bypassing the hook.
  The task credentials requirement is not met until provisioning is
  decided.
- A push the remote rejects, such as one behind a branch another daemon
  pushed meanwhile, leaves the work in the workspace and the error on
  the task page. Two daemons that both run a task, such as a lost daemon
  that returns after its task moved, can race to push the same branch.
- The end of each turn waits for the remote, and the task holds its
  slot meanwhile. Deleting a workspace waits for the remote too.
- One event kind is added to the client protocol.

Revisit if task credentials are provisioned per task, or if the owner
wants pull requests opened for finished work.
