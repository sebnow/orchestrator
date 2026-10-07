# Graceful pause: spike findings (2026-10-07)

The orchestrator's client daemon runs Claude Code agents (`claude -p`) on
a machine on the server's behalf. The budget idea in
`docs/design/2026-10-07-budget-and-pause.md` asks for a graceful pause:
the daemon tells a running agent to finish what it is doing and stop at a
clean point, so the tokens spent on the current step are not wasted, and
resumes it later. That note lists, under "Facts to establish", whether a
pause can be delivered as a follow-up prompt over stdin or needs a
mechanism inside Claude Code, and what "a clean point" means.

This spike answers those questions with observed runs. It follows the mod
versus stdout spike (`docs/design/2026-10-07-mod-vs-stdout-spike.md`),
reuses its probe and driver in `spikes/mod-vs-stdout/`, and assumes the
direction recommended there: the daemon drives `claude -p` over stdin and
stdout, and uses a Claude Code mod (a plugin of JavaScript or TypeScript
event handlers that runs inside the Claude Code process) only where those
fall short.

Markers: **UNVERIFIED** flags a claim not observed in these runs and not
confirmed in a primary source, including inferred causes. Everything else
was observed in the runs below or quoted from the sources listed under
Method.

Terms: a "step" is one tool call and the model request that follows it. A
"route" is a way of delivering a pause. A "turn" runs from one user prompt
to the `result` message stdout emits for it. Names such as
`prompt.submit`, `session.append` and `turn.step` in the tables are mod
events the probe observed; `$.prompt.submit` and `$.session.append` are
the mod API calls of the same names, which a mod makes. Field names such
as `door`, `messageCount` and `command_lifecycle` are Claude Code's own,
quoted from its output; the mod versus stdout spike describes them.

## Method

Environment: macOS 26.4.1 on arm64, Claude Code 2.1.289, logged in with a
Claude subscription, model `claude-haiku-4-5-20251001`. Every run used
`--setting-sources project --strict-mcp-config --allowedTools
'Bash(ping:*)'` in an empty temporary directory. The `spike-probe` mod was
loaded in every run with `SPIKE_QUIET=1`, which stops it reporting tool
and command descriptions and other mods' calls. It observed every run and
acted only where a route needed it.

Sources:

- The Agent SDK TypeScript reference,
  `https://code.claude.com/docs/en/agent-sdk/typescript.md` ("the SDK
  reference").
- The npm package `@anthropic-ai/claude-agent-sdk` 0.3.292 (`sdk.d.ts`,
  `sdk.mjs`).
- The headless page, `https://code.claude.com/docs/en/headless.md`.
- The `reference.md` of the `plugin-authoring` skill bundled with Claude
  Code 2.1.289 ("the plugin-authoring reference").

Each run sent one prompt over stdin that produces a turn of three steps:

> Use the Bash tool to run `ping -c 5 127.0.0.1` three times, one call
> after another, never in parallel and never in the background. After each
> call finishes, write the single word DONE-1, DONE-2 or DONE-3 before
> starting the next call. When all three are done, reply with exactly
> FINISHED.

Each ping takes about 4 s. One second after the mod saw the first ping's
`tool.call`, the driver applied the run's route. The pause text, where a
route carries text, was:

> Pause request from the operator: finish the step you are on, then stop.
> Do not start another step. Reply with one sentence saying where you
> stopped.

In every run except `pause-baseline`, once every started turn had a
`result`, the driver sent a resume prompt over stdin, "Resume the task
from where you stopped and finish it.", and waited for its `result`.

| Run | Start (UTC) | Route |
| --- | --- | --- |
| `pause-baseline` | 11:41:26 | none; the turn runs to the end, no resume |
| `pause-stdin` | 11:42:19 | the pause text as a stdin user message, no `priority` field |
| `pause-submit` | 11:42:49 | the pause text through the mod's `$.prompt.submit({ text, asUser: true })` |
| `pause-append` | 11:48:14 | the pause text through the mod's `$.session.append`, a user row |
| `pause-abort-after-tool` | 11:46:05 | the mod arms a flag; its `tool.call` hook calls `$.turn.abort` when the running tool returns |
| `pause-interrupt` | 11:46:33 | a stdin `control_request` with `subtype: "interrupt"` |

A first `pause-append` attempt at 11:43:41 was discarded: Claude Code's
first model request took 99 s, the debug log showing start-up fetches
timing out ("fetchBootstrapData failed: AxiosError: timeout of 5000ms
exceeded"), and the driver stopped waiting for the first ping before
applying the route.

Logs: `spikes/mod-vs-stdout/runs/pause-*/events.jsonl` and
`stdout.jsonl`. `events.jsonl` holds mod events, stdin writes, stdout
lines and the driver's own actions (for example enqueuing a command for
the mod) on one clock. Both were redacted with
`spikes/mod-vs-stdout/redact.py` before they entered the repository. All
six runs exited with code 0 and wrote nothing to stderr.

In the five runs with a resume turn, the driver closed stdin while that
turn was still running. The cause was a driver bug: its wait for the n-th
`result` counted results again on every rescan of the log and returned
early. It is fixed in the commit that adds these scenarios and described
in `spikes/mod-vs-stdout/README.md`, section "Known artifacts of the
probe". Each of those turns still finished and produced its `result`
before the process exited.

## Raw observations

Times are milliseconds since the driver spawned `claude`.

### Run `pause-baseline`

Three steps in one turn: first `tool_use` at 9572, the pings returned at
15291, 20860 and 28153, the model wrote DONE-1 and DONE-2 between them,
and the `result` "DONE-3\n\nFINISHED" arrived at 29944 (`duration_ms`
29637).

### Run `pause-stdin`

| ms | Source | Event | Fields of note |
| ---: | --- | --- | --- |
| 3574 | stdout | `assistant` | `tool_use` ping 1 |
| 4586 | stdin | user message | the pause text |
| 7722 | mod | `tool.call` after | ping 1 returned |
| 7729 | stdout | `user` | `tool_result` of ping 1 |
| 7735 | mod | `prompt.submit` (event) | the pause text, `turnId` of the running turn, `origin.kind: "sdk"` |
| 7740 | mod | `session.append` (event) | `door: "delivery"`, attachment `queued_command`: "<system-reminder>\nThe user sent a new message while you were working:\nPause request from the operator: ..." |
| 7747 | mod | `turn.step` before | `index: 1` of the same turn |
| 10314 | stdout | `assistant` | "DONE-1\n\nI received a pause request and am stopping here after completing the first ping command." |
| 10327 | stdout | `result/success` | `terminal_reason: "completed"`, `duration_ms` 10048 |
| 13400 | stdin | resume prompt | |
| 15520-27017 | stdout | resume turn | pings 2 and 3, "DONE-2", then "DONE-3\n\nFINISHED" |

The pause text did not appear on stdout, and no stdout message marked the
moment it was read.

### Run `pause-submit`

The mod called `$.prompt.submit` at 15073, during ping 1. The running turn
did not see it: the model ran all three pings and the turn ended with
"DONE-3\n\nFINISHED" at 36990. Stdout then emitted `command_lifecycle`
`started`, and the submitted text started a turn of its own at 37006,
which answered "I've completed the full task of running three sequential
pings and provided the final FINISHED response." The resume turn answered
that the task was already complete.

### Run `pause-append`

| ms | Source | Event | Fields of note |
| ---: | --- | --- | --- |
| 4505 | stdout | `assistant` | `tool_use` ping 1 |
| 5518 | driver | command enqueued | `append` |
| 5649 | mod | `$.session.append` resolved | the stored row: `type: "user"`, `isMeta: true`, the pause text |
| 8651 | mod | `tool.call` after | ping 1 returned |
| 8657 | stdout | `user` | `tool_result` of ping 1 |
| 8669 | mod | `turn.step` before | `index: 1`, `messageCount` 18 |
| 19577 | stdout | `assistant` | thinking block, the step's first output |
| 20812 | stdout | `assistant` | "DONE-1\n\nI stopped after completing the first ping command and did not proceed to the second one." |
| 20826 | stdout | `result/success` | `terminal_reason: "completed"`, `duration_ms` 20549 |
| 26828-40606 | stdout | resume turn | pings 2 and 3, then "DONE-3\n\nFINISHED" |

In the session's transcript file the appended row is stored after ping
1's `tool_result`, although it was appended while ping 1 was running.
11 s passed between the step's request (8669) and its first output
(19577); the logs do not show why (**UNVERIFIED**: model latency). The
pause text did not appear on stdout.

### Run `pause-abort-after-tool`

| ms | Source | Event | Fields of note |
| ---: | --- | --- | --- |
| 4729 | stdout | `assistant` | `tool_use` ping 1, after the text "I'll run the ping command three times sequentially as requested." |
| 5741 | driver | command enqueued | `abort-after-tool` |
| 5895 | mod | command | `abort-after-tool` armed |
| 8877 | mod | `tool.call` after | ping 1 returned; the hook calls `$.turn.abort` at 8879 |
| 8886 | stdout | `user` | `tool_result` of ping 1, `is_error: false` |
| 8891 | stdout | `result/success` | `terminal_reason: "aborted_tools"`, `is_error: false`, `result: ""`, `num_turns` 2, `duration_ms` 8615 |
| 8892 | mod | `turn.complete` | `isAborted: true`, `reason: "aborted"` |
| 13912 | stdout | `assistant` | resume turn: "DONE-1" |
| 14084-26367 | stdout | resume turn | pings 2 and 3, then "DONE-3\n\nFINISHED" |

The mod saw one `turn.step` (index 0) in the aborted turn: no model
request followed ping 1. The resume turn did not repeat ping 1.
`num_turns` in stdout's `result` does not match this document's turns, nor
the model requests the mod saw (2 for one request here, 4 for four in
`pause-baseline`, 3 for one in `pause-interrupt`); its meaning is
**UNVERIFIED**.

### Run `pause-interrupt`

| ms | Source | Event | Fields of note |
| ---: | --- | --- | --- |
| 5997 | stdout | `assistant` | `tool_use` ping 1 |
| 7009 | stdin | `control_request` | `{"type":"control_request","request_id":"spike-int-1","request":{"subtype":"interrupt"}}` |
| 7017 | stdout | `control_response` | `{"type":"control_response","response":{"subtype":"success","request_id":"spike-int-1","response":{"still_queued":[]}}}` |
| 7045 | stdout | `user` | `tool_result` `is_error: true`: "The user doesn't want to proceed with this tool use. The tool use was rejected ..." |
| 7048 | mod | `session.append` (event) | `door: "note"`: "[Request interrupted by user for tool use]" |
| 7053 | stdout | `result/error_during_execution` | `is_error: true`, `terminal_reason: "aborted_tools"`, `duration_ms` 6780 |
| 13335-31506 | stdout | resume turn | ping 1 again, then pings 2 and 3, "DONE-1", "DONE-2", then "DONE-3\n\nFINISHED" |

## Answers

### Mid-turn delivery of a pause instruction

| Route | Reached the model | Model stopped early | What stdout showed |
| --- | --- | --- | --- |
| stdin user message | in the same turn, at the first model request after the running tool returned (6 ms after its `tool_result`) | yes, after step 1, with a one-sentence note | the model's stop note and one `result/success`; nothing for the message itself |
| `$.prompt.submit` | only after the turn ended, as a turn of its own | no; all three steps ran | `command_lifecycle` around the extra turn |
| `$.session.append` | in the same turn, at the first model request after the running tool returned | yes, after step 1, with a one-sentence note | the model's stop note and one `result/success`; nothing for the row itself |

The stdin route matches the SDK reference. A user message sent during a
running turn with no `priority` field behaves as `'next'`: "Claude reads
the message in the same turn, as soon as the tool calls it is running
finish. If the turn ends first, the message starts the next turn." The
reference documents `'later'`, which holds the message until the turn
ends and so cannot pause mid-turn. It also documents `'now'` with
`origin: { kind: "human" }`: running commands that can continue move to
the background, and Claude reads the message in the same turn; without
that origin, `'now'` interrupts the turn. Neither value was run, so their
documented behaviour is **UNVERIFIED** here.

`$.session.append` behaved like the stdin route, but needs a mod.
`$.prompt.submit` did not deliver the pause: it waits until the session
is idle, as the plugin-authoring reference says ("never folded into a
running turn").

In both routes that reached the model, the stop was the model's choice.
Haiku complied in the one run of each; neither route enforces the stop.

Stdout did not show when the stdin message was read. The SDK reference
says that when a user message carries a `uuid`, Claude Code echoes it as
`user_message_uuid` on the first reply after the turn picks the message
up and on the turn's `result`. The runs sent no `uuid`, so this is
**UNVERIFIED** here.

### Clean stop at a tool boundary

`$.turn.abort`, called from the mod's `tool.call` hook after the running
tool returned, ended the turn 14 ms after the tool returned, without
another model request. The tool's result was kept: stdout showed its
`tool_result` with `is_error: false`, and the transcript has it. Stdout's
`result` had `subtype: "success"`, `is_error: false`, `terminal_reason:
"aborted_tools"`, `result: ""`, with usage of 10 input, 261 output,
14,167 cache-read and 7,220 cache-write tokens. The same process then
accepted a stdin resume prompt. The model continued from step 2: it wrote
the missing DONE-1, ran pings 2 and 3, and finished.

Of the routes tested, this is the only one that stopped at a tool boundary
without relying on the model. It needs a mod.

### The stdin interrupt

The request and its acknowledgement are in the `pause-interrupt` table;
the acknowledgement arrived 8 ms after the request. The running ping was
stopped and recorded as a rejected tool use (`is_error: true`). The turn's
`result` had `subtype: "error_during_execution"`, `is_error: true`,
`terminal_reason: "aborted_tools"`. A follow-up stdin turn worked, and in
this run the model re-ran the cut step from the start.

The interrupt request is documented for SDK clients, not on the headless
page:

- The SDK reference documents `interrupt()`, its receipt
  `SDKControlInterruptResponse { still_queued: string[]; cancelled?:
  string[] }`, the `control_request` envelope `{ type: "control_request",
  request_id, request }`, a `cancel_queued: true` option for "a client
  that drives the CLI's control protocol directly", and the
  `interrupt_receipt_v1` and `interrupt_cancel_queued_v1` capabilities.
- The package's `sdk.d.ts` declares `SDKControlInterruptRequest = {
  subtype: 'interrupt'; cancel_queued?: boolean }`, and `sdk.mjs` sends
  `{subtype:"interrupt", ...}` from `interrupt()`.
- `interrupt_send_now_v1` appears in `system/init`, the first message of
  each turn on stdout, but not in the SDK reference's capability table
  (**UNVERIFIED** meaning).

### Cost of each route

From stdout's `result` messages. `total_cost_usd` is Claude Code's
cumulative figure for the process, and its `modelUsage` entry marks it
`costBasis: "list"`, so a turn's cost is the difference from the previous
`result`. **UNVERIFIED:** that the subscription's quota tracks these
figures. Each figure is one run, and thinking length varies between runs.

| Run | Paused turn: output tokens, USD | Later turns: output tokens, USD | Total output tokens | Total USD |
| --- | --- | --- | ---: | ---: |
| `pause-baseline` | 485, 0.0275 (the whole task) | | 485 | 0.0275 |
| `pause-stdin` | 316, 0.0208 | resume 334, 0.0105 | 650 | 0.0313 |
| `pause-submit` | 467, 0.0274 (not paused) | pause turn 107, 0.0030; resume 179, 0.0035 | 753 | 0.0339 |
| `pause-append` | 270, 0.0204 | resume 330, 0.0113 | 600 | 0.0317 |
| `pause-abort-after-tool` | 261, 0.0172 | resume 347, 0.0113 | 608 | 0.0285 |
| `pause-interrupt` | 189, 0.0168 | resume 405, 0.0140 | 594 | 0.0308 |

Prompt cache, from the same `result` messages:

| Run | First turn: cache write, cache read | Resume turn: cache write, cache read |
| --- | --- | --- |
| `pause-baseline` | 8,554, 79,702 | |
| `pause-stdin` | 7,816, 35,544 | 1,056, 66,740 |
| `pause-submit` | 8,541, 79,703 | 157, 22,789 |
| `pause-append` | 7,725, 35,544 | 1,551, 65,915 |
| `pause-abort-after-tool` | 7,220, 14,167 | 1,484, 65,802 |
| `pause-interrupt` | 7,210, 14,167 | 1,594, 87,760 |

Excluding `pause-submit`, which did not pause, pausing and resuming cost
4 to 15 percent more than running through (0.0285 to 0.0317 against
0.0275). The tool-boundary abort had the lowest total in these single
runs; skipping the request that writes a stop note is a likely reason
(**UNVERIFIED**). The interrupt's paused turn was the cheapest, but its
resume turn ran the cut step again. With a 4 s ping that cost little;
for a long command it would repeat the whole command (**UNVERIFIED**).
`$.prompt.submit` paused nothing and added two small turns.

Time from the driver issuing the pause to the paused turn's `result`:
stdin 5.7 s (4586 to 10327, 3.1 s of it waiting for ping 1), append 15.3 s
(5518 to 20826, including the 11 s above), tool-boundary abort 3.2 s
(5741 to 8891), interrupt 44 ms (7009 to 7053).

## Recommendation

Deliver a graceful pause as a stdin user message with no `priority` field
and a `uuid`, worded as an instruction to finish the current step, stop,
and say where it stopped. In these runs the model then stopped at the end
of the tool call that was running when the message arrived. Resume with a
follow-up stdin prompt in the same process. This uses only stdin and
stdout, and the behaviour is documented in the SDK reference.

- The daemon should confirm pickup through the `user_message_uuid` echo
  on stdout (**UNVERIFIED**; see Open), and treat the turn's `result` as
  the paused state.
- If the turn ends before the message is read, the message starts a turn
  of its own (SDK reference). The daemon should then treat that turn's
  reply as the stop note.
- Keep the stdin interrupt for emergencies. It stopped the running tool
  within milliseconds, and in the one run the resume turn redid the
  interrupted step.
- Do not use `$.prompt.submit` for pausing.
- A stop that does not depend on the model's compliance needs a mod
  (`$.turn.abort` from `tool.call`). It stopped without relying on the
  model and had the lowest total cost here, in a single run. It could be
  part of a mod that enforces the budget, if one is built.

## Open

- **UNVERIFIED:** the `uuid` echo (`user_message_uuid`) on a stdin pause
  message in Claude Code 2.1.289; the SDK reference gives Agent SDK
  version requirements, not CLI versions.
- **UNVERIFIED:** an explicit `priority: 'next'`, and `priority: 'now'`
  with `origin: { kind: "human" }` as a faster pause during a long
  command; per the SDK reference the latter moves the command to the
  background instead of waiting for it.
- **UNVERIFIED:** pause behaviour when the running step is one long
  command (minutes rather than seconds) or a subagent, and when the model
  is writing text with no tool call running.
- **UNVERIFIED:** model compliance beyond one Haiku run per route, and
  with larger models or longer instructions in the conversation.
- **UNVERIFIED:** resuming a paused session in a new process
  (`--resume <session-id>`) instead of the same one.
- **UNVERIFIED:** what `interrupt_send_now_v1` in `system/init` and
  `num_turns` in `result` mean.
