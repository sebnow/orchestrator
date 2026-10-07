# Human steering

- Status: Accepted
- Decided: 2026-10-06
- Source: [initial brainstorm](../design/2026-10-06-brainstorm.md), decision 6

## Context

The orchestrator is meant to run agents hands-off. Some moments still
need a human, such as permission prompts.

A client daemon on each machine spawns the harness, the Claude Code CLI,
as `claude -p --input-format stream-json --output-format stream-json`,
and connects to the orchestrator server. In `-p` mode, permission answers
cannot be delivered over stdin. They go through `--permission-prompt-tool`,
an MCP tool that the harness calls and waits on. Sources:
https://code.claude.com/docs/en/headless.md,
https://code.claude.com/docs/en/cli-reference.md.

## Decision

A human steps in only at moments that need a human decision, such as
permission prompts. The daemon hosts an MCP server that provides the
harness's `--permission-prompt-tool`.

## Consequences

- Agents also need to reach the server to spawn other agents and to read
  and write their inboxes ([agent messaging](2026-10-07-agent-messaging.md)).
  The daemon's MCP server is one candidate for this. A Claude Code mod,
  JS/TS function hooks that run inside the Claude Code process, is
  another. This decision does not choose between them.
