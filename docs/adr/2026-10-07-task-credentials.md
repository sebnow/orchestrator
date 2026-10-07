# Task credentials

- Status: Accepted
- Decided: 2026-10-06
- Source: [initial brainstorm](../design/2026-10-06-brainstorm.md),
  decision 10

## Context

Safety comes from machine isolation. The whole machine an agent runs on
is its sandbox, so the agent runs with broad permissions and without
in-process sandboxing. A client daemon on each machine spawns the
harness, the Claude Code CLI (`claude`). Tasks need credentials such as
git tokens and cloud keys.

## Decision

Task credentials must not be easily reachable by the agent. A vault-style
mechanism is preferred.

The Anthropic OAuth token is out of scope. The agent's Bash tool runs as
the same OS user as `claude`, which can read the token, so the daemon
does not try to hide it.

## Consequences

- The mechanism is not decided. One idea, not evaluated, is a credential
  helper or proxy in the daemon so that the agent does not hold
  long-lived tokens.
