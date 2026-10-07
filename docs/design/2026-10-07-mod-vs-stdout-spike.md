# Mod versus stdout as the harness adapter: spike findings (2026-10-07)

The orchestrator's client daemon runs Claude Code (`claude -p`, the
harness) on a machine and reports what the agent does to the server. The
part of the daemon that talks to the harness is the harness adapter. The
brainstorm in `docs/design/2026-10-06-brainstorm.md` left open whether the
adapter should learn what the harness does from a Claude Code mod loaded
into `claude -p`, or by parsing its stream-json stdout (section "Open
questions", "Mod versus stdout as the harness adapter"). A mod is a plugin
of JavaScript or TypeScript event handlers ("hooks") that run inside the
Claude Code process.

This note records an empirical spike on that question. It reports what was
observed and recommends a direction; the decision will be recorded as an
ADR under `docs/adr/`.

Markers: **UNVERIFIED** flags a claim not observed in these runs and not
confirmed in a primary source, including inferred causes. Everything else
was observed in the runs below or quoted from the sources listed under
Method.

Terms: "Claude Code" here also covers what its debug log and mod API call
the engine. Mods run in priority groups called tiers; a mod loaded with
`--plugin-dir` is in the `user` tier, and built-in mods run in `prepend`
(before it) or `builtin` (after it). A permission "host" is whatever
answers a tool call that needs approval: a person in the interactive UI,
or in `-p` the tool named by `--permission-prompt-tool`. The "core
verdict" is the decision Claude Code reaches from its rules and permission
mode before any mod or host answers.

## Method

Environment: the project owner's macOS workstation, Claude Code 2.1.289,
logged in with a Claude subscription, model `claude-haiku-4-5-20251001`.
The machine has managed settings, so Claude Code ran its built-in guard mod
`cc-plugin-sec-default` before every user mod (debug log: "seated
outermost: this machine has managed settings"). The guard can withhold
events from user mods; see Stability caveats.

Sources read, under `https://code.claude.com/docs/en/`: the mod pages
`plugins/mods/overview.md` ("the overview"), `plugins/mods/reference.md`
("the mods reference"), `plugins/mods/api.md` and
`plugins/mods/create.md`, and the headless page `headless.md`. Also the
TypeScript declarations Claude Code 2.1.289 writes for mods, and the
`reference.md` of the `plugin-authoring` skill bundled with Claude Code
2.1.289 ("the plugin-authoring reference"). The declarations' header says:
"EARLY ACCESS: this surface may change between releases without notice."

Harness, in `spikes/mod-vs-stdout/` (see its README):

- `spike-probe`, a mod loaded with `--plugin-dir`. One hook on every event
  except `turn.step` (`on('!turn.step')`) and a generator hook on
  `turn.step`. Each hook POSTs the event to a local receiver with
  `$.http.fetch` before and after `next(e)`, and awaits the POST. Its
  `tool.check` hook asks the receiver for a verdict. A `$.clock.every`
  timer polls the receiver every 250 ms for `abort`, `submit` and
  `snapshot` commands.
- A Python driver that spawns `claude -p --input-format stream-json
  --output-format stream-json --verbose --model haiku`, writes turns to
  stdin, records every stdout line, and serves as the receiver. Mod events
  and stdout lines land in one `events.jsonl`, timestamped on one clock.
- A stdio MCP server exposing `approve`, for `--permission-prompt-tool`. It
  asks the same receiver, so both permission routes use one policy.

Every run added `--setting-sources project --strict-mcp-config` and ran in
an empty temporary directory, so the owner's plugins, settings hooks,
permission rules and MCP connectors stayed out.

| Run | Start (UTC) | Mod | Flags beyond the common ones | Turns |
| --- | --- | --- | --- | --- |
| `observe` | 07:44:33 | yes | `--allowedTools 'Bash(date)' --include-partial-messages` | run `date`; reply TWO |
| `control-1` | 07:45:58 | yes | `--permission-mode manual` | two `touch` calls (allow, deny); `sleep 30` + abort; mod submits THREE; stdin FOUR |
| `control-2` | 07:46:50 | yes | `--permission-mode manual` | as `control-1`, long command `sleep 30 && echo finished` |
| `abort` | 07:47:58 | yes | `--permission-mode manual` | `ping -c 20 127.0.0.1`, mod aborts after 3 s; stdin AFTER |
| `mcp` | 07:48:41 | no | `--permission-mode manual --mcp-config ... --permission-prompt-tool mcp__perm__approve` | two `touch` calls (allow, deny); `ping`, stdin interrupt after 3 s; stdin THREE |

Logs: `spikes/mod-vs-stdout/runs/<run>/events.jsonl` (combined) and
`stdout.jsonl` (raw stdout), with e-mail addresses and account ids
redacted. The debug logs were kept locally and not committed.
`timeline.py` prints a run as one line per record. All five runs exited
with code 0, wrote nothing to stderr, and hit no driver timeout. Claude
Code reported a cumulative `total_cost_usd` of $0.019 to $0.034 per run.

## Raw observations

Times are milliseconds since the driver spawned `claude`.

### Run `observe`: one tool-using turn and one plain turn

| ms | Source | Event | Fields of note |
| ---: | --- | --- | --- |
| 17 | stdin | user message | "Use the Bash tool to run `date`..." |
| 379 | mod | `session.start` | `cwd`, `surface: null`, `isInteractive: false` |
| 383 | mod | snapshot | `$.session.id()` = `a277afe5-...`, model, `$.session.version()` 2.1.289 |
| 694 | mod | `prompt.submit` | `text`, `origin.kind: "sdk"`, `wait: false` |
| 696-707 | mod | `session.append` x7 | the prompt, then six `attachment` rows: environment, model, deferred tools, agent listing, skill listing, token reminder |
| 713 | stdout | `system/init` | `session_id`, `tools`, `plugins` (lists `spike-probe`), `capabilities` |
| 717 | mod | `turn.start` | `turnId` |
| 750 | mod | `turn.step` before | `index: 0`, `model`, `messageCount: 12` |
| 2024-2394 | both | stream | each mod chunk (`thinking`, `tool`, `input`) about 1 ms before the matching stdout `stream_event` |
| 2397 | mod | `session.append` | assistant row with the `tool_use` block in Messages API form |
| 2401 | stdout | `assistant` | `tool_use` Bash `{"command":"date"}` |
| 2402 | mod | `tool.call` before | `tool`, `tool_use_id`, `command` |
| 2406-2411 | mod | `tool.check` | core verdict `{"decision":"allow","rule":"Bash(date)"}` |
| 2414 | stdout | `rate_limit_event` | see Quota and rate limits |
| 2418 | mod | `session.measure` | `context`, `rateLimits`, `cost`, `changed` |
| 2418 | mod | `turn.step` after | `stopReason: "tool_use"`, `usage` for this request |
| 2614 | mod | `tool.call` after | `result.stdout`, `text` |
| 2620 | stdout | `user` | `tool_result` |
| 3850 | stdout | `assistant` | text `Wed Oct  7 09:44:36 CEST 2026` |
| 3865 | mod | `turn.step` after | `stopReason: "end_turn"`, `usage` |
| 3870 | stdout | `result/success` | `session_id`, `usage`, `total_cost_usd` 0.019171, `num_turns`, `duration_ms`, `ttft_ms`, `permission_denials`, `terminal_reason` |
| 3872 | mod | `turn.complete` | `answer`, `durationMs`, `isAborted`, `reason: "answer"`, `turnId`, `usage` |
| 3875 | mod | snapshot | `$.session.usage().cost.usd` 0.019171, equal to stdout's `total_cost_usd` |
| 5871-7017 | both | second turn | same pattern; stdout emits `system/init` again for the turn |
| 7947 | driver | stdin closed | |
| 8259 | mod | `session.end` | `reason: "other"`, `sessionId`, `resume.id` |

The mod's streamed chunks lead stdout by about 1 ms. The probe awaits its
POST before passing each chunk on, which likely explains the lead
(**UNVERIFIED**).

Stdout in this run carried these message types: `system/init`,
`system/status`, `system/thinking_tokens`, `stream_event`, `assistant`,
`user`, `rate_limit_event`, `result`.

Mod events raised by Claude Code itself, the same in all four mod runs:
`session.start`, `session.end`, `prompt.submit`, `prompt.attachment`,
`session.append`, `turn.start`, `turn.step`, `turn.complete`, `tool.call`,
`tool.check`, `tool.describe` (30, once per tool), `command.describe` (120),
`agent.offer`, `session.measure`. The mod also saw every `$` call made by
the built-in mods `cc-plugin-telemetry` and `cc-plugin-agents-md` (140
`env.get` calls per session from telemetry alone).

These events fired but the guard withheld them from the probe
(debug log: "spike-probe: <event> bypassed by cc-plugin-sec-default (tier
user); beneath runs"): `classic.SessionStart`, `classic.UserPromptSubmit`,
`classic.PreToolUse`, `classic.PostToolUse`, `classic.PostToolBatch`,
`classic.Stop`, `classic.MessageDisplay`, `classic.SessionEnd`,
`prompt.compose`, `prompt.section`, `prompt.context`, `settings.read`,
`attribution.text`.

Not seen in any run; the runs did nothing expected to trigger them:
`agent.spawn`, `session.compact`, `session.receive`, `session.send`,
`ui.*`.

### Runs `control-1` and `control-2`: permissions, abort, mod-submitted turn

Permissions (both runs alike; `control-1` times):

| ms | Source | Event | Fields of note |
| ---: | --- | --- | --- |
| 2496 | stdout | `assistant` | `tool_use` `touch spike-allowed.txt` |
| 2512 | mod | `tool.check` after `next` | core verdict `ask`, reason "touch in '...' needs approval..." |
| 2514 | receiver, mod | verdict | `allow`; the mod returns `{decision: "allow"}` |
| 2657 | stdout | `user` | `tool_result` "(Bash completed with no output)"; the file exists afterwards |
| 2896 | mod | `tool.check` after `next` | core verdict `ask` for `touch spike-denied.txt` |
| 2898 | stdout | `system/permission_denied` | `decision_reason_type: "hook"`, `decision_reason`: the receiver's text |
| 2905 | stdout | `user` | `tool_result` `is_error: true`, "Permission to use Bash denied by plugins cc-plugin-sec-default, spike-probe: Denied by the spike receiver acting as the daemon." |
| 4561 | stdout | `result/success` | `permission_denials` lists the denied call |

The debug log records the outcome as `tool.check Bash ...: ask -> allow by
plugins cc-plugin-sec-default, spike-probe`.

Abort. In both runs Claude Code refused the long command before running it
(`tool_result`: "Blocked: standalone sleep 30. To wait for a condition, use
Monitor..."; in `control-2`: "Blocked: sleep 30 followed by: echo
finished"). In `control-2` the model then ran the command with
`run_in_background: true`. In both runs the abort therefore arrived while
the model was generating:

| Run | Enqueued | Mod polled | `$.turn.abort` resolved | stdout `result` | Result fields |
| --- | ---: | ---: | ---: | ---: | --- |
| `control-1` | 12230 | 12363 | 12366 | 12373 | `subtype: success`, `terminal_reason: "aborted_streaming"`, `result` cut mid-sentence |
| `control-2` | 14037 | 14264 | 14267 | 14270 | `subtype: success`, `terminal_reason: "aborted_streaming"`, `result: ""` |

The mod's `turn.complete` carried `isAborted: true` and
`reason: "aborted"`. In `control-2` the background `sleep` survived the
abort. Claude Code killed it after stdin closed (`task_updated` `status:
"killed"`).

Mod-submitted turn (`$.prompt.submit({ text, asUser: true })`), with a
stdin turn after it for comparison (`control-1`):

| ms | Source | Event |
| ---: | --- | --- |
| 14264 | receiver | `submit` enqueued |
| 14388 | mod | command polled; `$.prompt.submit` called at 14391 |
| 14392 | stdout | `command_lifecycle` `state: "started"`, `command_uuid` |
| 14404 | stdout | `system/init` |
| 14408 | mod | `turn.start`; the submit promise resolves with `origin: {kind: "plugin", name: "spike-probe", asUser: true}` |
| 15437 | stdout | `assistant` text `THREE` |
| 15457 | stdout | `result/success` `THREE` |
| 15463 | stdout | `command_lifecycle` `state: "completed"` |
| 17468 | stdin | user message FOUR |
| 17480 | mod | `prompt.submit` with `origin.kind: "sdk"` |
| 17489 | mod | `turn.start` |
| 18400 | stdout | `result/success` `FOUR` |

Stdout showed the mod-submitted turn like any other: `init`, `assistant`,
`result`. It also bracketed the turn with `command_lifecycle` messages,
which a stdin turn does not get. The submitted text never appeared on
stdout as a `user` message (`--replay-user-messages` was not set).

### Run `abort`: abort during a running foreground tool

| ms | Source | Event | Fields of note |
| ---: | --- | --- | --- |
| 8376 | mod | `prompt.submit` | first turn, 8.4 s after the stdin write (see Latency of mod reports) |
| 10197 | stdout | `assistant` | `tool_use` `ping -c 20 127.0.0.1` |
| 10216 | mod | `tool.check` answered | `allow` (core verdict was `ask`) |
| 13209 | receiver | `abort` enqueued | |
| 13250 | mod | command polled, `$.turn.abort` called | |
| 13251 | stdout | `system/task_started` | `is_backgrounded: true`, `tool_use_id` of the ping |
| 13256 | mod | `$.turn.abort` resolved | |
| 13259 | stdout | `user` | `tool_result` "Command running in background with ID: byuxvduz6..." |
| 13264 | stdout | `result/success` | `terminal_reason: "aborted_tools"`, `result: ""` |
| 13265 | mod | `turn.complete` | `isAborted: true`, `reason: "aborted"` |
| 15268-16254 | both | stdin turn AFTER | answered normally |
| 22386 | stdout | `system/task_updated` | `status: "killed"`, 5 s after stdin closed (debug log: "print wind-down: killing background shell ... after 5000ms grace") |

`$.turn.abort` ended the turn but did not stop the running command:
Claude Code moved it to a background task. The build's declarations carry
an internal flag for this, `backgroundedByTurnAbort`, documented as "True if
a plugin's turn abort moved the running command to the background". The
mod's `tool.call` hook for the ping never received its result (one
`before`, no `after`). In `control-1` and `control-2` the cut step's
`turn.step` report was lost too: the in-flight `$.http.fetch` was rejected
with the abandoned dispatch, and the debug log says "hooks chain failed".

### Run `mcp`: no mod, `--permission-prompt-tool` and stdin interrupt

| ms | Source | Event | Fields of note |
| ---: | --- | --- | --- |
| 2599 | stdout | `assistant` | `tool_use` `touch spike-allowed.txt` |
| 2635 | receiver | `/decide` from the MCP tool | `{tool_name, input, tool_use_id}` |
| 2750 | stdout | `user` | `tool_result` success |
| 2880 | receiver | `/decide` | deny |
| 2882 | stdout | `user` | `tool_result` `is_error: true`, the receiver's text; `tool_result_meta.non_execution_kind: "permission-rule"`; no `system/permission_denied` message |
| 8038 | stdout | `assistant` | `tool_use` `ping -c 20 127.0.0.1`; allowed at 8042 |
| 11038 | stdin | `{"type":"control_request","request_id":"spike-int-1","request":{"subtype":"interrupt"}}` | |
| 11042 | stdout | `control_response` | `{"subtype":"success","request_id":"spike-int-1","response":{"still_queued":[]}}` |
| 11046 | stdout | `user` | `tool_result` `is_error: true` "The user doesn't want to proceed with this tool use..."; then a text block "[Request interrupted by user for tool use]" |
| 11048 | stdout | `result/error_during_execution` | `is_error: true`, `terminal_reason: "aborted_tools"` |
| 13042-13889 | both | stdin turn THREE | answered normally |
| 16588 | driver | exit 0 | 1.5 s after stdin closed; no background task to reap |

The stdin interrupt stopped the running `ping`; no background task was
created. The `control_request` format is not on the headless page. It
follows the Agent SDK's wire protocol (**UNVERIFIED** as a documented
interface), and it worked.

### Latency of mod reports

Delay from the mod stamping a report (`Date.now()`) to the receiver
reading it, over every report in a run:

| Run | Reports | p50 ms | p95 ms | max ms |
| --- | ---: | ---: | ---: | ---: |
| `observe` | 764 | 1.6 | 47.2 | 97.7 |
| `control-1` | 880 | 1.8 | 48.3 | 139.7 |
| `control-2` | 915 | 1.7 | 67.2 | 142.0 |
| `abort` | 718 | 1.5 | 192.3 | 5928.6 |

The slow reports were all at start-up, where Claude Code dispatches the
150 `command.describe` and `tool.describe` events of a session
concurrently. In `abort` they
arrived in steps of about 1, 2, 4 and 6 s, which fits TCP connection
retries against the Python receiver's listen backlog of 5 (**UNVERIFIED**
cause). Because the mod awaits each POST before calling `next`, the first
prompt of `abort` reached `prompt.submit` 8.4 s after its stdin write,
against 0.5 to 0.7 s in the other runs.

Order: reports of concurrently dispatched events (`command.describe`,
`env.get`) arrived out of sequence by under 1 ms. The lifecycle events
(`prompt.submit`, `turn.start`, `tool.call`, `tool.check`,
`turn.complete`) arrived in the order raised.

## Side by side

What each side carries at each point, from the runs above. "Mod" means a
mod in the `user` tier on a machine where the built-in guard runs.

| Point | stdout (stream-json, `--verbose`) | Mod |
| --- | --- | --- |
| Session start | `system/init`: `session_id`, model, tools, MCP server status, plugins, `capabilities`; repeated at each turn | `session.start` (`cwd`, `isInteractive`); `$.session.id()`, `model()`, `version()` on demand |
| Prompt accepted | nothing (`user` echo only with `--replay-user-messages`) | `prompt.submit` with `origin.kind` (`sdk`, `plugin`) |
| Injected context | not shown | `session.append` rows for each system reminder (environment, skills, agents, date, ...) |
| Turn start | `system/init`, `system/status` `requesting` | `turn.start` with `turnId` |
| Model request | `stream_event` `message_start` (with `--include-partial-messages`) | `turn.step` before: `index`, `model`, `messageCount` |
| Streaming text, thinking, tool input | `stream_event` deltas (with `--include-partial-messages`) | `turn.step` chunks `text`, `thinking`, `tool`, `input`, `stop`; same pieces, same order |
| Thinking progress | `system/thinking_tokens` estimates | not seen |
| Assistant message | `assistant`, one per content block, `usage` as of message start | `session.append` with the content blocks in Messages API form |
| Per-request usage | `stream_event` `message_delta` (partial messages only) | `turn.step` after: `stopReason`, `usage` with `model` |
| Tool about to run | `assistant` `tool_use` | `tool.call` before (can rewrite, deny or answer) |
| Permission decision | `system/permission_denied` on a deny; the MCP route answers out of band | `tool.check`: core verdict and reason (`ask`, "needs approval"), and the mod's own verdict |
| Tool result | `user` `tool_result`; `tool_use_result` | `tool.call` after: typed `result`, `text`, `isError` |
| Background task | `system/task_started`, `task_updated`, `task_notification`, `background_tasks_changed` | not seen |
| Rate limits | `rate_limit_event`, once per process in every run | `session.measure` after each turn; `$.session.usage().rateLimits` on demand |
| Context window fill | not seen | `session.measure.context` (`tokens`, `window`, `percent`) |
| Turn end | `result`: `subtype`, `terminal_reason`, `result`, `usage`, `modelUsage`, cumulative `total_cost_usd`, `num_turns`, `duration_ms`, `duration_api_ms`, `ttft_ms`, `permission_denials` | `turn.complete`: `answer`, `durationMs`, `reason`, `isAborted`, `turnId`, `usage`; cumulative cost from `$.session.usage().cost.usd` |
| Mod-submitted turn | `command_lifecycle` started and completed around a normal turn | `prompt.submit` with `origin.kind: "plugin"` |
| Session end | process exit | `session.end`: `reason`, `sessionId`, `resume` |

## Answers

### Hook events in `-p` compared with stdout

Every lifecycle event the runs triggered fired in `-p`; all but the 13
the guard withheld reached the mod. They are listed under run `observe`
and compared point by point in the side-by-side table. The mod and stdout
share most lifecycle points. The mod alone sees three: the system
reminders injected into the conversation, Claude Code's own permission
verdict before any host is asked, and the context-window fill. Stdout
alone has three: background-task lifecycle messages, thinking token
estimates, and the result-level metrics (`num_turns`, `duration_api_ms`,
`ttft_ms`, `modelUsage`). The withheld events include all `classic.*`
events and the system prompt events (`prompt.compose`, `prompt.section`,
`prompt.context`).

### Session id, usage, cost and full assistant content

The mod sees all four:

- Session id: `$.session.id()`, and `sessionId` on `session.end`.
- Usage: `turn.step` after (per request) and `turn.complete` (per turn),
  each with `input_tokens`, `output_tokens`, `cache_read_input_tokens`,
  `cache_creation_input_tokens` and the model.
- Cost: cumulative, from `$.session.usage().cost.usd` and
  `session.measure.cost`. It matched stdout's `total_cost_usd` exactly
  (0.019171 after the first turn of `observe`). Cost is reported only
  cumulatively; per-turn cost is the difference between two readings.
- Full content: `session.append` carries every row, including thinking,
  text and tool_use blocks in Messages API form. `turn.step` streams the
  same content as chunks. `tool.call` carries the tool input and the typed
  result.

The brainstorm listed session id, usage, cost and the full assistant
message stream as available only on stdout; the mod sees all four. Stdout
alone has the `result` metadata named in the previous answer. Stdout's
`result.subtype` also did not distinguish the mod-aborted turns: its value
was `success`, and only `terminal_reason` showed the abort.

### Permissions and interrupting a turn

A mod can answer a permission prompt in `-p`. Its `tool.check` hook saw
Claude Code's own verdict, `ask`, and returned `allow` or `deny`, and that
verdict stood. No MCP server and no `--permission-prompt-tool` were
needed. The brainstorm named the MCP permission tool as the only route.
Two others exist: the mods reference documents `tool.check` returning
`{ decision }`, and the headless page documents `PermissionRequest`
settings hooks.

Compared with `--permission-prompt-tool`:

- Both routes produced the same allow and deny outcomes. Measured from
  the `tool_use` appearing on stdout to the question reaching the
  receiver, the two calls took 36 ms and 5 ms through MCP (run `mcp`, the
  deny's `tool_use` at 2875) and 17 ms and 8 ms through the mod (run
  `control-1`, the deny's `tool_use` at 2889).
- The mod route also saw Claude Code's own verdict and reason, and a mod deny
  produced a `system/permission_denied` stdout message with
  `decision_reason_type: "hook"`. The MCP deny produced none, only the
  error `tool_result`.
- The MCP route keeps policy out of the Claude Code process and uses a
  documented CLI flag. The mod route depends on the early-access mod API
  and, on managed machines, on the built-in guard admitting the verdict.
  **UNVERIFIED:** whether the guard admits a mod's `allow` against a `deny`
  rule; the reference says that needs the managed setting
  `allowModsToOverrideDenyRules`.
- **UNVERIFIED:** what happens when the mod cannot reach the daemon. The
  probe then returns Claude Code's `ask`, and the headless page says an
  `ask` with no host is denied in `-p`. This was not run.

A mod can stop a running turn with `$.turn.abort({ turnId })`. In all
three tries stdout's `result` followed the call within 15 ms. It did not
stop a running foreground Bash command: Claude Code moved the command to a
background task that ran until the process exited. The stdin interrupt
(`control_request` with `subtype: "interrupt"`) stopped the running
command and acknowledged with a `control_response`. To stop a running
command, use the stdin interrupt. To end the turn and let the command
finish in the background, use `$.turn.abort`.

### Outbound and inbound messaging

Outbound with `$.http.fetch` worked for every event, except calls in
flight when a turn was aborted. Local POSTs took a median of 1.5 to
1.8 ms. With each POST awaited, lifecycle events arrived in order, and the
receiver was on Claude Code's critical path: a slow receiver
delayed the session, by 8.4 s at start-up in `abort`. A real adapter would
queue reports and send them without blocking the hook, trading ordering
and delivery on abort for latency (**UNVERIFIED**, not built).

Inbound, the mod received commands by polling every 250 ms, which
worked: commands were picked up 40 to 227 ms after they were enqueued.
**UNVERIFIED:** the declarations offer two paths that were not tried:
`$.process.spawn` streaming a long-lived child (for example `curl -N` on a
daemon endpoint), and `$.http.fetch` over a Unix socket (`socketPath`).

`$.prompt.submit` works in `-p`. The submitted turn started 17 to 60 ms
after the call. That matches a stdin write, which started its turn 21 to
58 ms after the write. Stdout reported the turn normally and bracketed it
with `command_lifecycle` messages. The documented semantics differ from
stdin: the prompt waits until the session is idle and is "never folded
into a running turn" (plugin-authoring reference). Stdin gives the same
follow-up turn without a mod, as the brainstorm's experiment showed
(section "Experiment: follow-up turns over stdin mid-session").

### Quota and rate limits

Both sides see quota; they differ in shape and frequency.

Stdout, in every run, emitted one `rate_limit_event` per process, at the
first API response, for example:

    {"type":"rate_limit_event","rate_limit_info":{"status":"allowed",
     "resetsAt":1791375000,"rateLimitType":"five_hour",
     "overageStatus":"rejected","overageDisabledReason":"out_of_credits",
     "isUsingOverage":false,"unifiedWindows":{
       "five_hour":{"utilization":0.06,"resetsAt":1791375000},
       "seven_day":{"utilization":0.03,"resetsAt":1791651600}}}, ...}

It was not repeated on later turns of the same process, so its trigger may
be a change of status rather than each response (**UNVERIFIED**; the event
is undocumented).

The mod received `session.measure` after every turn, with:

    "rateLimits":[{"kind":"five_hour","percentUsed":6,"resetsAt":"2026-10-07T12:10:00.000Z"},
                  {"kind":"seven_day","percentUsed":3,"resetsAt":"2026-10-10T17:00:00.000Z"}]

It could also read the same figures at any time with `$.session.usage()`.
Both are documented: the mods reference says `session.measure` fires
"After each turn, and when a plan limit's percent used changes". The mod's
figures are whole or one-decimal percentages; stdout's carry fractional
utilization and the overage status, which the mod lacks. Across the runs
the five-hour window went from 6 to 9 percent.

This matters for the budget idea in
`docs/design/2026-10-07-budget-and-pause.md`: scheduling tasks by
priority against the five-hour and weekly quota windows, and pausing work
gracefully. Of the two sources, only the mod's figures are documented,
and only the mod can read them on demand.

### Stability caveats

- No crashes, no stderr output, exit code 0 in all runs. Mods loaded
  without any environment flag. During early access they needed
  `CLAUDE_CODE_ENABLE_FUNCTION_HOOKS`; the overview says 2.1.287 and later
  ignore it.
- The mod API is labelled early access in the build's own declarations
  ("this surface may change between releases without notice"). The online
  reference can describe a later release than the one installed: on
  2026-10-07 it described 2.1.290, while 2.1.289 was installed.
- On a machine with managed settings, or for a Team or Enterprise login
  (mods reference), the built-in guard `cc-plugin-sec-default` runs first
  and withholds some events from user mods; it withheld 13 here. The
  brainstorm plans to run agents on cloud VPSes as well as the owner's
  laptop. **UNVERIFIED:** a VPS with no managed settings and a personal
  plan would not run the guard, so a mod there would see more events. What the
  adapter sees would then depend on the account and machine it runs
  under.
- Hooks are on Claude Code's critical path: an awaited call in a hook
  delays the session (8.4 s observed).
- On abort, in-flight `$` calls from the aborted dispatch are rejected,
  and the `after` side of the cut tool call or request is never reported.
- Claude Code writes into the mod's folder when it loads it
  (`.claude-plugin/types/`, `tsconfig.json`), so the folder must be
  writable.
- The probe's catch-all hook also fired for `engine.create`, where `$` is
  empty, and failed there once in `observe` (`[WARN] hooks module
  spike-probe: the on("!turn.step") hook failed at engine.create`). Claude
  Code skipped it and continued. After `observe`, the probe was changed
  to skip `engine.create`.
- Unrelated to mods but seen here: in the foreground, Claude Code blocks a
  standalone `sleep 30` and `sleep 30 && echo finished`. When a `-p`
  process exits, Claude Code kills background shells after a 5 s grace.

## Recommendation

Keep stdout and stdin as the adapter's base. Add a mod only for what stdout
cannot give, and keep it behind the same adapter interface (brainstorm,
decision 9: the Go daemon drives the CLI directly behind an adapter
interface).

Reasons:

- Stdout and stdin need no code inside the harness and use documented CLI
  flags, except the stdin interrupt, whose `control_request` format is
  undocumented (**UNVERIFIED** as an interface). They give everything the
  brainstorm needed from them: session id, usage, cost, the full message
  stream, follow-up turns, and an interrupt that stops running tools.
- The mod is more informative on four points: the context-window fill,
  the rate-limit windows after every turn and on demand, the system
  reminders injected into the conversation, and the core permission
  verdict. It is more powerful on one: permissions are answered in-process
  without hosting an MCP server.
- Against it: the API is early access, an awaited call in a hook delays
  the session, and what the mod sees depends on managed settings and the
  account's plan.

For the budget and pausing idea, stdout falls short on quota: in these
runs it reported quota once per process. A small mod that only reports
`session.measure` to the daemon, without blocking, should close that gap
(**UNVERIFIED**; not built). Permissions can stay with
`--permission-prompt-tool` (brainstorm, decision 6: permission prompts go
through an MCP tool the daemon hosts) unless the daemon is to see Claude
Code's own verdict or avoid the MCP server; in that case a mod
`tool.check` hook is a working alternative.

## Open

- **UNVERIFIED:** the guard's effect on a VPS with no managed settings and
  a personal subscription; this spike ran only on a managed machine.
- **UNVERIFIED:** inbound push to a mod through `$.process.spawn` or a Unix
  socket, instead of polling.
- **UNVERIFIED:** whether `$.session.append` (a user-role row added to a
  running turn, per the plugin-authoring reference) could deliver a
  "finish up" message mid-turn for a graceful pause. Neither
  `$.prompt.submit` nor a stdin message was tested mid-turn.
- **UNVERIFIED:** whether a mod can end a turn at a clean point by calling
  `$.turn.abort` from `tool.call` after the current tool returns.
- **UNVERIFIED:** whether `rate_limit_event` repeats when the status changes.
  Utilization rose across separate processes and never within one.
- **UNVERIFIED:** behaviour when the mod's daemon is unreachable during a
  `tool.check`.
