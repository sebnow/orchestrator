# Daemon against live Claude Code: findings (2026-10-07)

The client daemon drives Claude Code through `internal/daemon`,
`internal/harness` and `internal/harness/claude`. Its live tests, in
`internal/daemon/live_test.go`, ran the real CLI and checked, from each
task's journal, the Claude Code behaviour the daemon relies on. Several
of those behaviours are marked **UNVERIFIED** in
[graceful pause](../adr/2026-10-07-graceful-pause.md) and
[client protocol](../adr/2026-10-07-client-protocol.md). This note
records what the runs showed about them.

Markers: **UNVERIFIED** flags a claim not observed in these runs and not
confirmed in a primary source, including inferred causes. Everything else
was observed in the runs below or quoted from the sources named where it
is used.

Terms:

- A "turn" runs from a user prompt to the `result` message Claude Code
  writes to stdout for it.
- A "message" is one line of Claude Code's stream-json stdout, named by
  its `type` and `subtype`, such as `system/thinking_tokens`.
- The "journal" is the daemon's per-task log, one JSON line per event:
  each stdout message, and each event the daemon originates, such as
  `permission_requested` or `pause_acknowledged`. "Seq" is the number the
  daemon gives each event, counting from 1 per task.
- The "gateway" is the daemon's MCP server. It serves the `permission`
  tool, which Claude Code calls to ask whether it may run a tool, and the
  `acknowledge_pause` tool, which the agent calls with a note saying
  where it stopped.
- A pause has two limits: the "acknowledgement limit", from delivering
  the pause request to the agent's `acknowledge_pause` call, and the
  "cleanup limit", from that call to the end of the turn. When a limit
  expires, the daemon interrupts the turn.
- A "graceful run" is a pause test in which the agent acknowledged and
  ended its turn within both limits. An "interrupted run" is one in which
  a limit expired first.

## Method

Environment: the project owner's macOS workstation (Darwin 25.4.0,
arm64), Claude Code 2.1.289 logged in with a Claude subscription,
started with `--model haiku`. The daemon started each harness as:

    claude -p --input-format stream-json --output-format stream-json
      --verbose --model haiku --permission-mode default
      --setting-sources project --strict-mcp-config
      --mcp-config '{"mcpServers":{"orchestrator":{"type":"http",
        "url":"http://127.0.0.1:<port>/tasks/<task>/mcp","timeout":100000000}}}'
      --permission-prompt-tool mcp__orchestrator__permission
      --allowedTools mcp__orchestrator__acknowledge_pause

A task given a system prompt also gets `--append-system-prompt <text>`;
the live tests gave none. Each task ran in an empty temporary directory.
Every prompt went to stdin with a fresh `uuid` and no `priority` field.
The pause request text was:

> Pause request from the operator. First call the acknowledge_pause tool
> with a one-sentence note saying where you are stopping and what
> remains. Then finish the step you are on, do not start another step,
> and end your turn.

The four live tests:

| Test | What it did |
| --- | --- |
| one turn | "Reply with exactly the word READY and nothing else."; one turn, no tool call |
| permission | asked for `touch spike-allowed.txt` then `touch spike-denied.txt` through the Bash tool; the test allowed the first after holding the answer 70 s and denied the second |
| graceful pause | the task from the [graceful pause spike](2026-10-07-graceful-pause-spike.md), Method: three sequential Bash calls of `ping -c 5 127.0.0.1`, about 4 s each; pause delivered 1 s after the test allowed the first ping; acknowledgement limit 90 s, cleanup limit 60 s; then "Resume the task from where you stopped and finish it." |
| interrupted pause | the same task and timing with both limits set to 1 ms; then "Reply with exactly the word AFTER and nothing else." |

The pause tests allowed every `ping` command and denied every other tool
call.

The runs invoked `claude` 20 times: 12 model sessions, 7
`claude --version` calls and one `claude auth status`. The permission
test and both pause tests ran three times each, the one-turn test twice,
and one session was a manual run of `cmd/daemon`. Two runs failed
because of bugs in the tests, both fixed before the final run:

- The first permission run's prompt asked only which command succeeded,
  so the reply did not mention the denied one.
- The first graceful run counted the interrupt that the test's own
  shutdown sends as an interrupt during the pause.

The behaviour of Claude Code in those two runs is included below.

The journals lived in the tests' temporary directories and were
removed. The seqs and figures below come from the test log output.
Unless a run is named, seqs are from the final run of the full suite,
whose first permission request was at 13:40:52 UTC.

## Findings

### The `user_message_uuid` echo

Claude Code 2.1.289 echoes the `uuid` of a stdin user message in the
`user_message_uuid` and `user_message_uuids` fields of later stdout
messages. In the final graceful run, the task prompt's uuid appeared on
these messages:

| Seq | Message |
| ---: | --- |
| 5, 6 | `system/thinking_tokens` |
| 7 | `assistant`, the turn's first reply |
| 17, 18, 26, 27 | `system/thinking_tokens` |
| 31 | `result/success`, with `user_message_uuids` holding the task prompt's uuid and the pause request's uuid |

The resume turn echoed its own prompt's uuid on seq 36 and 37
(`system/thinking_tokens`), 38 (first reply) and 53 (result). The
one-turn test's result carried `user_message_uuid` as well.

The pause request did not get a turn of its own: Claude Code read it
in the running turn. In this run, the pause request's uuid appeared on
no message before that turn's result. The `system/thinking_tokens`
messages at seq 26 and 27 came after the acknowledgement at seq 24 and
still named only the task prompt. The
[`user_message_uuid` section](https://code.claude.com/docs/en/agent-sdk/typescript.md#user_message_uuid)
of the Agent SDK TypeScript reference says a turn started by a regular
message "answers that message for its whole run", which matches. For the
daemon, this means the echo shows that Claude Code has read the pause
request only once the turn ends. The gateway acknowledgement arrived
earlier.

### Acknowledgement through the gateway tool

The agent called `acknowledge_pause` with a note in all six pause runs:
three graceful and three interrupted. Each call was journaled as
`pause_acknowledged`, and no call to a gateway tool asked for
permission. Notes from the final run:

- graceful, seq 24: "Completed first ping call. Two more ping calls
  remain (DONE-2 and DONE-3), followed by final FINISHED reply."
- interrupted, seq 31: "Paused at the start of the task before any ping
  calls—all three sequential ping commands remain to be executed."

**UNVERIFIED:** that `--allowedTools` is what kept `acknowledge_pause`
from asking for permission; every run passed it.

### Pause compliance

All three graceful runs stopped after the first of the three pings,
without an interrupt, and the resume turn finished with
`DONE-3\n\nFINISHED`. No run reached the 90 s acknowledgement limit:
each whole test took under 28 s. These are three Haiku runs of one task.
**UNVERIFIED:** compliance with other models, other tasks, or over more
runs.

### A pause request still unread at interrupt time runs afterwards

In each interrupted run, the turn that was running ended with
`result/error_during_execution`, `terminal_reason: "aborted_tools"` (seq
18 in the final run). Claude Code then ran the pause request as a turn
of its own. In it, the agent called `acknowledge_pause` (seq 31) and
replied "Paused and acknowledged. Ready to resume when you're ready—I'll
run the three sequential ping commands starting with the first one."
(result seq 41). The agent then answered the follow-up prompt with
"AFTER" (seq 48), in the same process. All three interrupted runs went
this way.

Claude Code answers an interrupt with a `control_response` message, the
interrupt receipt. The
[`SDKControlInterruptResponse` section](https://code.claude.com/docs/en/agent-sdk/typescript.md#sdkcontrolinterruptresponse)
of the SDK reference says the messages the receipt lists under
`still_queued` run after the interrupt unless the interrupt request sets
`cancel_queued: true`. The daemon sends the interrupt as the
[spike driver](../../spikes/mod-vs-stdout/driver.py) did, without
`cancel_queued`. **UNVERIFIED:** that the receipts listed the pause
request under `still_queued`; their contents were not inspected. In
these runs an interrupted pause cost one more short turn, in which the
agent also acknowledged.

### Permission requests that wait over a minute

In three runs of the permission test, Claude Code waited for an answer
held 70 s. The turn ended 1m11.3s, 1m11.6s and 1m11.7s after the first
`permission_requested` event. The allowed file was created. The denied
call appeared in the result's `permission_denials`, and in the final run
the reply was "`spike-allowed.txt` succeeded and `spike-denied.txt` was
denied by the operator."

These waits ran with the gateway's per-server `timeout` set to
100,000,000 ms. The [MCP page](https://code.claude.com/docs/en/mcp.md)
says an HTTP server otherwise gets a 60 s timer per request, until the
server's first response byte, and a 5-minute idle timeout; a per-server
`timeout` raises both.

- **UNVERIFIED:** whether Claude Code would have abandoned the 70 s wait
  without the `timeout`; every run set it.
- **UNVERIFIED:** that Claude Code honours the `timeout` field in an
  inline `--mcp-config`. The MCP page describes it for `.mcp.json`
  entries.
- **UNVERIFIED:** any wait longer than 72 s, including the waits of
  hours that "waits indefinitely" in the
  [client protocol](../adr/2026-10-07-client-protocol.md) ADR implies.

### `rate_limit_event`: documented fields and the fields the daemon reads

As of 2026-10-07, the
[`SDKRateLimitEvent` section](https://code.claude.com/docs/en/agent-sdk/typescript.md#sdkratelimitevent)
of the Agent SDK TypeScript reference documents the event. Its
`rate_limit_info` has `status` (`allowed`, `allowed_warning`,
`rejected`), `resetsAt`, `utilization`, and fields for credits-required
rejections. The [harness integration](../adr/2026-10-07-harness-integration.md)
ADR calls the event undocumented.

The daemon's quota reading uses fields the reference does not list:
`rateLimitType`, `overageStatus`, `overageDisabledReason`,
`isUsingOverage`, and `unifiedWindows`. Under the subscription login,
`unifiedWindows` is the only per-window utilization Claude Code sent.
The 13 events recorded by the
[mod versus stdout spike](2026-10-07-mod-vs-stdout-spike.md), in
`spikes/mod-vs-stdout/runs/*/stdout.jsonl`, do not carry the documented
top-level `utilization`. **UNVERIFIED:** whether the live runs' events
carried it; the daemon journals the windows from `unifiedWindows` and
the raw events were not checked for it.

### Quota readings

Each of the 12 sessions emitted one `rate_limit_event`, which the
daemon journaled followed by a `quota_observed` event. The five-hour
window stood at 0.98 utilization in the earlier runs and 0.99 in the
final run, with status `allowed_warning`, resetting at 17:10 UTC. The
seven-day window stood at 0.2, resetting 2026-10-10 at 17:00 UTC. No
request was rejected.

### `command_lifecycle` around stdin prompts

In the manual `cmd/daemon` run, stdout carried `command_lifecycle`
messages before the turn's `system/init` (seq 2 and 3) and after its
result (seq 12). The
[mod versus stdout spike](2026-10-07-mod-vs-stdout-spike.md) saw them
only around a turn that a Claude Code mod (a plugin of JavaScript or
TypeScript event handlers running inside Claude Code) submitted with
`$.prompt.submit`. **UNVERIFIED:** whether their `command_uuid` is the
stdin message's `uuid`; it was not compared.

## Open

- **UNVERIFIED:** pausing with `priority: 'now'`, during a long command
  or a running subagent, and resuming in a new process with `--resume`.
- **UNVERIFIED:** how Claude Code parses an `--append-system-prompt`
  value that starts with "-". The daemon passes the task's system prompt
  as the next argument, and a CLI parser may read a leading "-" as a
  flag.
- **UNVERIFIED:** whether the automatic backgrounding of MCP tool calls
  that run past two minutes, described in the
  [MCP page](https://code.claude.com/docs/en/mcp.md) under "Automatic
  backgrounding of long tool calls", applies to the permission tool.
- Whether `--allowedTools` is what stops `acknowledge_pause` from asking
  for permission; see "Acknowledgement through the gateway tool".
