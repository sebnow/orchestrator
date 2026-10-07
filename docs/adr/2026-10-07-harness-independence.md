# Harness independence

- Status: Accepted
- Decided: 2026-10-07
- Source: owner decision; closes the open question "How agents reach the
  server" in the [initial brainstorm](../design/2026-10-06-brainstorm.md),
  section "Open questions"
- Amended by: [client protocol](2026-10-07-client-protocol.md), which keeps Claude-shaped types in a harness package shared by the daemon and the server, rather than inside the adapter

## Context

A client daemon on each machine runs the harness, the Claude Code CLI, and
connects to the orchestrator server. The daemon drives the harness through
several Claude Code features: stream-json over stdin and stdout
([harness adapter](2026-10-07-harness-adapter.md)), and an MCP tool passed
to `--permission-prompt-tool` for permission prompts
([human steering](2026-10-07-human-steering.md)). Claude Code also offers
mods, JS/TS function hooks that run inside the Claude Code process; the
harness adapter decision defers loading one. The daemon will also need to
pause agents and to read the subscription's quota figures
([budget and pause](../design/2026-10-07-budget-and-pause.md)).

[Harness integration](2026-10-07-harness-integration.md) puts the adapter
behind an interface because the stream-json format is undocumented in
places and, **UNVERIFIED**, declared subject to change.

The initial brainstorm left open how agents reach the server to spawn
agents, read and write inboxes, and answer permission prompts: through an
MCP server hosted by the daemon, a mod, or both. Human steering and
[agent messaging](2026-10-07-agent-messaging.md) also leave this open.

## Decision

The client daemon is the adapter boundary for the harness. The
orchestrator's protocol, task model, and server must not depend on Claude
Code specifics, so that another harness can be substituted later.

Which harness feature serves which function is an implementation choice,
judged on complexity and efficiency, not an architectural decision. With
Claude Code, for example, permission prompts can go over the MCP tool, a
pause over stdin, quota figures over a mod if one is added, and inbox and
spawn calls over the daemon's MCP server. The brainstorm's open question
"How agents reach the server" is therefore implementation detail.

## Consequences

- The adapter interface is designed around harness-neutral events and
  commands. Claude-shaped types, such as stream-json messages, stay inside
  the adapter.
- Substituting another harness gives the adapter interface a second reason
  to exist, alongside the partly undocumented stream-json format.
- Moving a function from one harness feature to another, such as from the
  MCP server to a mod, should change only the daemon.
