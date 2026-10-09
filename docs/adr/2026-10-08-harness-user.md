---
status: accepted
date: 2026-10-08
source: owner decision, working session on 2026-10-08
---

# Harness user

## Context

[Task credentials](2026-10-07-task-credentials.md) requires that task
credentials not be easily reachable by the agent, and leaves the
mechanism undecided. A daemon that runs the harness, `claude`, as its
own OS user lets the agent's Bash tool read everything the daemon can:
the daemon's mTLS key, which lets its holder act as the daemon towards
the server, the daemon's state, and every task's journal. This record
keeps the daemon's files out of the agent's reach. It does not
supersede task credentials, which still governs per-task credentials,
whose mechanism is still undecided.

The daemon runs git in each task's workspace: it clones, configures the
clone, counts commits, and pushes. `git help git`, section SECURITY,
warns that git runs commands named in a repository's configuration and
hooks, so running git in a repository someone else can write runs their
code. The agent can write its workspace, so a daemon that runs git
there as itself runs the agent's code with the daemon's rights.

## Decision

The owner can name a second, unprivileged OS user, the harness user,
when starting a daemon. The daemon then runs the harness and every
command that touches a workspace as that user, through sudo. Without a
harness user, the daemon runs everything as itself. One harness user
serves every task on a daemon.

The harness user creates and owns the workspaces. That user cannot
enter the daemon's state directory, so the workspaces live in a
separate directory that the owner names. The daemon does not run git in
a workspace as itself.

A sudoers rule lets the daemon's user run only `claude`, `git` and `rm`
as the harness user, without a password, and nothing as root. sudo
gives the harness user's home to the harness, so the harness uses that
user's own Claude Code login. The owner logs that user in once per
machine; the daemon does not manage the login.

The harness user cannot open the daemon's ssh agent socket. When the
daemon has an ssh agent, it serves a socket of its own that the harness
user can open, relays each connection to the agent, and gives the
harness's commands that socket as their agent. If the daemon has no ssh
agent, the harness gets none.

The daemon's user cannot signal the harness user's processes, and sudo
cannot relay SIGKILL to the command it runs (sudo(8), "Signal
handling"). To kill a harness, the daemon sends sudo SIGTERM, which
sudo relays, and closes the harness's input. A daemon that restarts and
finds a harness its previous run left sends SIGTERM to the sudo process
it recorded.

## Consequences

- File permissions keep the agent from reading the daemon's key, its
  state and its journals.
- The agent can still read its own Claude Code login, its workspace,
  and every other task's workspace on the daemon, since all tasks share
  the harness user. It can see in the process list the URL of the
  daemon's MCP gateway, which serves every task's tools on loopback,
  and so call another task's tools.
- The agent can use the daemon's ssh agent through the relayed socket,
  and so reach every repository that agent's keys open, for as long as
  the daemon runs. It can no longer read the daemon user's own ssh
  files.
- A killed harness ends only if it exits on SIGTERM or when its input
  closes; the daemon cannot force it.
- Setting up a harness user needs root once per machine: creating the
  user, writing the sudoers rule, creating the workspace directory, and
  logging the user in to Claude Code.
- The checklist in README.md, section "Running the harness as another
  user", has not been run.

Revisit if tasks get users of their own, or if the daemon gains a way
to kill the harness user's processes.
