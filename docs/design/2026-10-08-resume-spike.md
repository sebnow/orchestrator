# Resuming a task in a new process: spike findings (2026-10-08)

The [MVP smoke findings](2026-10-08-mvp-smoke-findings.md) left open
whether a task must hold its `claude` process between turns. On
2026-10-08 the owner chose that a task's process exits when its turn
ends, and that a later prompt starts a new process with
`--resume <session id>` in the same workspace. The
[graceful pause](../adr/2026-10-07-graceful-pause.md) record lists
resuming with `--resume` in a new process as untested. This spike ran
four `claude` processes with the daemon's flags to answer five
questions:

1. Does the conversation carry over into the new process?
2. Is the `session_id` of the new process's `system/init` and `result`
   the same as before, or new?
3. Must `--append-system-prompt` and `--mcp-config` be passed again?
4. Can the agent still reach the daemon's gateway after a resume?
5. Can a pause acknowledged in one process be resumed in another?

Markers: **UNVERIFIED** flags a claim not observed in this run and not
confirmed in a primary source, including inferred causes. Everything else
was observed in the run below, read from `claude --help` of 2.1.289, or
read from the source files named where it is used.

Terms follow the [MVP smoke findings](2026-10-08-mvp-smoke-findings.md):
the "harness" is Claude Code, a "message" is one stream-json stdout line,
and the "gateway" is the daemon's MCP server with the `permission` tool,
which Claude Code calls to ask whether it may run a tool, and the
`acknowledge_pause` tool, which the agent calls with a "stop note" saying
where it stopped. Further terms:

- A "process" is one `claude` process; a "session" is Claude Code's
  conversation, named by its `session_id`, which several processes can
  share.
- A pause has "settled" once the turn that answers the pause request has
  ended.
- A `command_lifecycle` message is Claude Code's report that a stdin
  prompt was `queued`, `started` or `completed`; its `command_uuid` is the
  `uuid` the daemon gave the prompt.

## Method

Environment: the project owner's macOS workstation (Darwin 25.4.0,
arm64), Claude Code 2.1.289 logged in with a Claude subscription, model
`haiku` (`claude-haiku-4-5-20251001` in `system/init`).

A Go test in package `internal/daemon`, written against the repository
at change `mztryknm` and not kept, drove the daemon's own code: one
`Gateway`, one `Daemon`, and `Daemon.StartTask` for each process. Each
process started with the arguments built by the `arguments` function in
`internal/harness/claude/process.go`, changed by the wrapper below. Each
process was its own daemon task (S1 to S4), so each had its own journal
and its own gateway URL, `/tasks/S<n>/mcp`. After the turn ended, or
after the pause settled, the test closed the harness's stdin and waited
for it to exit, as the daemon is to do after each turn.

The harness's executable was a wrapper script. When an environment
variable named a session, it appended `--resume <session>`; when another
was set, it dropped `--append-system-prompt` and its value. It logged
the final arguments and ran the real `claude`. Its log shows the four
processes; `claude --version` was run once before them, outside the log.
The system prompt in every task was "Begin every reply with the token
[ORCH] on its own line.", so its effect shows in the replies.

| Process | Workdir | `--resume` | `--append-system-prompt` | Prompt |
| --- | --- | --- | --- | --- |
| S1 | `work-codeword` | none | given | "The codeword is MARMALADE. Remember it. Reply with exactly OK." |
| S2 | `work-codeword` | S1's session | given | "What is the codeword? Then use the Bash tool to run \`echo resumed\`, and reply with one line holding the codeword and the command's output." |
| S3 | `work-pause` | none | given | the three-step task below |
| S4 | `work-pause` | S3's session | dropped | "Resume the task from where you stopped and finish it. Your note when you stopped: " followed by S3's stop note |

The three-step task is the one of the
[graceful pause spike](2026-10-07-graceful-pause-spike.md), Method: run
`ping -c 5 127.0.0.1` three times with the Bash tool, writing `DONE-1`,
`DONE-2` and `DONE-3` after each, then reply `FINISHED`. In S3 the test
allowed the first ping, called `Task.Pause` one second later, and closed
stdin once the pause had settled. The pause limits were 90 s to
acknowledge and 1 minute to clean up. S2 and S4 allowed any `echo` or
`ping` the agent asked for.

The journals and the wrapper's log were kept in `/tmp/resume-spike` on
the owner's workstation and not committed. The excerpts below are
verbatim stdout lines with fields left out (`…`).

## Findings

### The conversation carries over

S2 answered from S1's conversation. Its result:

    {"type":"result","subtype":"success","is_error":false,"num_turns":2,"result":"MARMALADE resumed","stop_reason":"end_turn","session_id":"d3b7060b-bcd0-4d5e-8775-506cfc91cf06","total_cost_usd":0.022630100000000004,…}

S1's result, for comparison:

    {"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"[ORCH]\n\nOK","stop_reason":"end_turn","session_id":"d3b7060b-bcd0-4d5e-8775-506cfc91cf06","total_cost_usd":0.0163247,…}

### The session id stays the same

Every message of S2 carried S1's `session_id`, from the first line, a
`command_lifecycle` with `"state":"queued"`, through `system/init` to
`result`; S4 carried S3's in the same way. S2's `system/init`:

    {"type":"system","subtype":"init","cwd":"/private/tmp/resume-spike/work-codeword","session_id":"d3b7060b-bcd0-4d5e-8775-506cfc91cf06","mcp_servers":[{"name":"orchestrator","status":"connected","source":"dynamic"}],"model":"claude-haiku-4-5-20251001","permissionMode":"default",…}

`claude --help` describes `--fork-session` as "When resuming, create a
new session ID instead of reusing the original", so reusing it is the
default. In every process the first message was the `command_lifecycle`
of the prompt the daemon had written to stdin; no message, and so no
session id, came before the first prompt.

### The recorded system prompt is reused; `--mcp-config` was not tested

S4 ran without `--append-system-prompt`. Its first text block was the
marker, in an assistant message of its own:

    {"type":"assistant","message":{"content":[{"type":"text","text":"[ORCH]"}],…},"session_id":"61df9c08-eb58-4857-bcae-9d231224da7c",…}

S2, given the system prompt, wrote the same block before its tool call.
Neither result shows the marker, because a `result` carries only the
turn's last text block.

`claude --help` documents `--system-prompt-snapshot`, on by default: the
system prompt, "--append-system-prompt included", is recorded on the
conversation's first request, and "every later request and resume sends
the record as-is, even when a later launch passes different text, until
the conversation is compacted". S3's session file,
`~/.claude/projects/-private-tmp-resume-spike-work-pause/<session>.jsonl`,
holds `prompt_snapshot` entries containing the [ORCH] instruction, all
written by S3; S4 wrote none. **UNVERIFIED:** that S4 followed the
snapshot rather than imitating the `[ORCH]` lines of its earlier
replies; the run cannot tell the two apart. **UNVERIFIED:** that a later
launch's different system prompt is ignored, and what happens after
compaction; no run passed different text or compacted.

Every process was given `--mcp-config`, each with its own gateway URL,
and every `system/init` listed the gateway as `connected`. S2 and S4
reached the gateway at the URL of their own launch, not the one their
session started with. The snapshots name no MCP server, so the snapshot
does not carry the MCP configuration over. **UNVERIFIED:** what a
resumed process does without `--mcp-config`; no process ran without it.

### The gateway works after a resume

S4 asked for both remaining pings through the gateway's `permission`
tool; the daemon journaled them:

    {"seq":10,"kind":"permission_requested","payload":{"request_id":"a79fd26a-1c05-4c5d-b6e5-54d38efa4888","tool":"Bash","input":{"command":"ping -c 5 127.0.0.1","description":"Run second ping to localhost"}},…}
    {"seq":19,"kind":"permission_requested","payload":{"request_id":"de05815b-4ccf-4b70-bb9c-68d09e37d62a","tool":"Bash","input":{"command":"ping -c 5 127.0.0.1","description":"Run third ping to localhost"}},…}

S2's and S4's `system/init` list `mcp__orchestrator__acknowledge_pause`
among the tools. No resumed process called it. S3 called it after
calling `ToolSearch`, Claude Code's tool for loading the definition of a
tool it has deferred, as the agent did in the smoke test. S2's
`echo resumed` ran without a permission request, so S2 did not exercise
the gateway. **UNVERIFIED:** why Claude Code ran `echo` without asking;
the permission mode was `default`.

### A pause in one process resumes in the next

S3 acknowledged the pause with the note "Completed first ping command.
Remaining: run second and third ping commands sequentially, write DONE-2
and DONE-3 after each, then reply FINISHED.", wrote `DONE-1`, and ended
its turn without an interrupt. The turn's result lists two prompt uuids:
`ab844fbb-…`, the task prompt's, queued at seq 2, and `ac91a3de-…`, the
pause request's, queued at seq 13 while the first ping ran:

    {"type":"result","subtype":"success","num_turns":4,"result":"DONE-1","session_id":"61df9c08-eb58-4857-bcae-9d231224da7c","total_cost_usd":0.028267200000000003,"user_message_uuid":"ab844fbb-240b-4253-999d-f198b1cc2332","user_message_uuids":["ab844fbb-240b-4253-999d-f198b1cc2332","ac91a3de-9bab-4eaa-ae89-1f59aa3112dd"],…}

The journal then holds `pause_settled` (`"interrupted":false`) at seq 35,
the task prompt's `command_lifecycle` `completed` at seq 36, written
after the result, and `harness_exited` with code 0 at seq 37.

S4, resumed from S3's session with the stop note in its prompt, ran the
second and third pings, wrote `DONE-2`, then `DONE-3` and `FINISHED`:

    {"type":"result","subtype":"success","num_turns":3,"result":"DONE-3\n\nFINISHED","session_id":"61df9c08-eb58-4857-bcae-9d231224da7c","total_cost_usd":0.039233300000000006,"user_message_uuid":"09ca18ff-e3a6-423c-9d5b-9ed8996a391e",…}

The two processes ran three pings between them, none twice. The smoke
test's pause, resumed in the same process, reached $0.0282 at the pause
and $0.0388 at the end; this run reached $0.0283 and $0.0392, so in
these single runs resuming in a new process cost about the same as
resuming in the same one.

### Other observations

- `total_cost_usd` continues across the processes of a session: S2's
  $0.0226 includes S1's $0.0163, and S4's $0.0392 includes S3's
  $0.0283. The session file records a `cost-state` entry with the same
  running totals. `num_turns` counts only the process's own turn.
- Each process exited with code 0, 1.3 to 2.5 s after its result, once
  stdin was closed. The lines after the result, such as the task
  prompt's `command_lifecycle` `completed`, came before the exit.
- Session files live under `~/.claude/projects/`, in a directory named
  after the working directory's real path (`/tmp` resolves to
  `/private/tmp` on macOS). **UNVERIFIED:** that a resume fails from
  another working directory, another OS user or another machine; none
  was tried.
- The four processes cost $0.0163, $0.0063, $0.0283 and $0.0110, the
  differences of the running totals above.

## Answers

1. Yes, in one run: S2 recalled the codeword planted in S1.
2. The same. Resuming keeps the session id unless `--fork-session` is
   given.
3. `--append-system-prompt`: not needed in one run; per `claude --help`
   the recorded prompt is reused until compaction. `--mcp-config`: not
   tested without it; given each time, it lets the gateway URL change.
4. Yes, in one run: the `permission` tool answered two requests in a
   resumed process. `acknowledge_pause` was offered but not called after
   a resume.
5. Yes, in one run: the agent paused in S3 and finished the remaining
   steps in S4.

## Open

- **UNVERIFIED:** that a resume works after compaction, after a daemon
  restart, or when the session file is missing; none was tried.
- **UNVERIFIED:** how an interrupted pause, with no stop note, resumes in
  a new process; this run's pause was acknowledged.
- **UNVERIFIED:** whether the agent calls `acknowledge_pause` reliably in
  a resumed process; one pause was observed, in a fresh process.
