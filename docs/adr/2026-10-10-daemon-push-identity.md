---
status: accepted
date: 2026-10-10
source: >-
  owner decisions, 2026-10-10: the server orchestrates credentials;
  security is to be considered and the channel must be TLS; a public
  key may cross the wire; the design was accepted with history
  rewriting and multiple branches noted as open
---

# Daemon push identity and mirror

## Context

[Work delivery](2026-10-08-work-delivery.md) has the daemon push the
task's branch, and assumes credentials such as an ssh key in the daemon
user's setup. On the owner's Mac that is the owner's ssh-agent, relayed
to the harness user ([harness user](2026-10-08-harness-user.md)). A VPS
daemon has no owner agent.

In harness-user mode every git command on the workspace runs as the
harness user, so any credential that user can reach, the agent can use
for as long as its task runs. [Work stays on the daemon's
machine](2026-10-09-work-stays-on-the-machine.md) requires credentials
to stay with the daemon, outside the agent's process.

## Decision

### The daemon's own key

Each daemon generates an ed25519 key pair at first start, kept in its
state directory with the same permissions as its TLS key. The private
key never leaves the machine. The daemon reports the public key as a
daemon fact over the mTLS channel ([client
protocol](2026-10-07-client-protocol.md)); the server stores and shows
it on the daemon's page and in the API.

Registering it at the forge, a deploy key per repository or a machine
user, is the owner's job for now; the server doing it through the
forge's API is a later record.

### The mirror

The daemon keeps a bare mirror, which it owns, for each repository its
tasks use, under its state directory. Clone, fetch and push against the
remote run as the daemon user from the mirror, with the daemon's key,
and in single-user mode also with the keys of the owner's agent when
present. The harness user never runs git against the remote.

A task's workspace is cloned from the mirror by the harness user, with
the mirror as its `origin`. The workspace holds no credential and has
no route to the remote.

### Delivery

The daemon fetches the task's branch from the workspace into the
mirror, with the upload-pack side run as the harness user, then pushes
the branch from the mirror to the remote without force, as [work
delivery](2026-10-08-work-delivery.md) records. A push the remote
refuses, such as after history rewriting on the task branch, is
reported as a push error and not retried with force. A task delivers
one branch, its own; work on several branches per task is not decided
here.

### The agent socket relay

The ssh-agent relay to the harness user is no longer needed for
delivery and is kept for the agent's own use in single-user mode, where
the owner intends the local harness to have the owner's environment.

## Consequences

- Disk for mirrors, one bare repository per repository a daemon's tasks
  use.
- The clone's pre-push hook becomes redundant, since the harness user
  no longer pushes to the remote, and stays.
- Moving a task to another daemon checks the branch out from the remote
  through the new daemon's mirror.
- The sudoers rule for git already covers `git upload-pack`.
- `requireRemote` and the accepted URL forms are unchanged.
- On the Mac the flow is the same without sudo.

Revisit if a task needs to deliver work on more than one branch, or if
the owner wants the server to register deploy keys through a forge's
API.
