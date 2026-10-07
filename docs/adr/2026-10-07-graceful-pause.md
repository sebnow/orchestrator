# Graceful pause

- Status: Accepted
- Decided: 2026-10-07
- Source: [graceful pause spike](../design/2026-10-07-graceful-pause-spike.md),
  lines 34-103 (Method), 199-314 (Answers), 316-338 (Recommendation), and
  340-357 (Open)

## Context

The subscription limits tokens per five-hour and weekly window, which makes
managing that budget a core scheduling concern for the orchestrator
([budget and pause](../design/2026-10-07-budget-and-pause.md)). Tasks
carry a priority, and filler tasks, which use budget that would otherwise
go unspent, yield as soon as a higher-priority task is runnable. Pausing a
task must not waste the tokens already spent on its current step.

A spike on 2026-10-07 ran Claude Code 2.1.289 in `-p` under a subscription
login, with the model `claude-haiku-4-5-20251001`, on a turn of three tool
calls. Each route was tested once, delivering a pause during the first
tool call.

- A stdin user message with no `priority` field reached the model in the
  same turn, at the first model request after the running tool returned.
  The model stopped after that step and said where it stopped. A further
  stdin prompt to the same process resumed the task from the second step.
  The [Agent SDK TypeScript reference](https://code.claude.com/docs/en/agent-sdk/typescript.md)
  documents this behaviour for a message without `priority`, and adds that
  if the turn ends first, the message starts the next turn.
- The mod API call `$.prompt.submit` waited until the turn ended, so the
  task ran to completion and nothing was paused.
- The mod API call `$.session.append` behaved like the stdin message but
  needs a mod.
- `$.turn.abort`, called from a mod's `tool.call` hook once the running
  tool returned, ended the turn at the tool boundary, kept the tool's
  result, and made no further model request. It was the only route tested
  that stopped at a tool boundary without relying on the model.
- A stdin `control_request` with subtype `interrupt` stopped the running
  tool within milliseconds. On resume, the model ran the interrupted step
  again. The request is documented in the Agent SDK TypeScript reference,
  not on the [headless page](https://code.claude.com/docs/en/headless.md).

With the stdin message and with `$.session.append`, the model chose to
stop; neither route enforces it. In these single runs, pausing and
resuming cost 4 to 15 percent more than running through.

The client daemon is the adapter boundary for the harness
([harness independence](2026-10-07-harness-independence.md)), and the
[harness adapter](2026-10-07-harness-adapter.md) uses stdin and stdout
only, with no mod loaded.

## Decision

A pause is cooperative. The daemon asks the agent to acknowledge the
pause, finish its current step, stop, and report where it stopped. The
session stays alive and resumes in place with a follow-up prompt. An
abrupt interrupt is a separate path for emergencies.

The agent acknowledges by calling a tool on the daemon's MCP gateway; its
reply text is the stop note kept for the record and for the resume
prompt. The daemon starts a timer when it delivers the pause. No
acknowledgement within the first limit means the agent ignored the
request, and the daemon interrupts. An acknowledgement extends the limit
to a cleanup allowance, after which the daemon interrupts anyway. Both
limits are per-task settings. A tool call on its own is not a signal
either way, because cleaning up may take several.

The implementation for Claude Code 2.1.289:

- Pause: a stdin user message with no `priority` field and a `uuid`.
- Resume: a further stdin prompt to the same process.
- Emergency: the stdin `control_request` interrupt.
- `$.prompt.submit` is not used for pausing.

## Consequences

- The pause depends on the model complying. **UNVERIFIED:** compliance
  beyond one Haiku run per route, and whether the model acknowledges
  through the gateway tool reliably; the spike sent no acknowledgement
  request.
- The daemon's MCP gateway gains a pause-acknowledgement tool.
- Claude Code echoes the message's `uuid` as `user_message_uuid`, which
  the daemon uses to confirm that the turn picked up the pause.
  **UNVERIFIED:** the Agent SDK TypeScript reference documents the echo,
  but the spike sent no `uuid`.
- **UNVERIFIED:** pause behaviour with `priority: 'now'`, during a long
  command, or while a subagent runs, and resuming with `--resume` in a new
  process. None was tested.
- A stop at a tool boundary that does not depend on the model needs a mod
  calling `$.turn.abort` from `tool.call`. The
  [harness adapter](2026-10-07-harness-adapter.md) decision defers adding
  a mod.
