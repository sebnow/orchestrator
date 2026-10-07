# Harness integration

- Status: Accepted
- Decided: 2026-10-06
- Source: [initial brainstorm](../design/2026-10-06-brainstorm.md), decision 9

## Context

The client daemon is written in Go and runs the harness, the Claude Code
CLI. There is no Go SDK. The Claude Agent SDK, which wraps
the CLI as a subprocess, targets only TypeScript and Python, and
[harness billing](2026-10-07-harness-billing.md) excludes it.

The CLI runs headless as `claude -p --input-format stream-json
--output-format stream-json`. Input over stdin is newline-delimited JSON.
Output message types are documented in part. `rate_limit_event` and the
`system` subtypes `hook_started`, `hook_response`, `thinking_tokens`, and
`commands_changed` were observed but are undocumented. Sources:
https://code.claude.com/docs/en/headless.md,
https://code.claude.com/docs/en/cli-reference.md.

An experiment on 2026-10-06 with Claude Code 2.1.289 under subscription
login wrote two user messages to one `claude -p` process, 25 seconds
apart. The process stayed alive between them, emitted a `system/init` and
a `result` per turn under the same `session_id`, and exited when stdin
closed. The input format
`{"type":"user","message":{"role":"user","content":"..."}}` is
**UNVERIFIED** in the documentation and confirmed only by this
experiment.

## Decision

The Go daemon drives the Claude Code CLI process directly. The adapter
between the harness and the client protocol is isolated behind an
interface, because the stream-json format is undocumented in places and,
**UNVERIFIED**, declared subject to change.

## Consequences

- Follow-up prompts can be sent over stdin mid-session without restarting
  the harness.
- The daemon reads `result` messages, with `session_id`, `subtype`,
  `usage`, and `total_cost_usd`, and the full assistant message stream
  from stdout.

Revisit if a spike shows that a Claude Code mod (JS/TS function hooks
running inside the Claude Code process) gives the daemon more control
over the session or more information than stdout does.
