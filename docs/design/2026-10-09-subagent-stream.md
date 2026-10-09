# The stream of a native subagent, observed (2026-10-09)

`internal/harness/claude/testdata/subagent.jsonl` was written by hand
from the documented shape
(<https://code.claude.com/docs/en/headless.md>, "Follow subagent
messages"), because no recorded run had a subagent. This note records
one live run in which Claude Code started a subagent with its own
`Agent` tool, how its stream differed from the hand-written fixture,
and what changed as a result.

Markers: **UNVERIFIED** flags a claim not checked against the run
below, the code, or a source named here.

## The run

One task, one `claude` process, one haiku session:
`TestLiveGivenPromptToUseASubagentWhenItRunsThenTheSubagentsMessagesCarryTheAgentCallAsTheirParent`
in `internal/daemon/live_test.go`, run with `go test -tags live` on
2026-10-09. Claude Code 2.1.289, model `claude-haiku-4-5-20251001`,
started by the daemon with the arguments in
`internal/harness/claude/process.go`, `--forward-subagent-text`
among them. The working directory held three files. The prompt was
"Use a subagent to count the files in this directory and then reply
with the number only." Every permission request was allowed. Haiku
started a subagent at the first attempt. The run cost $0.0729 by the last
`total_cost_usd`.

The 33 stream-json lines, in order, with the subagent's lines marked
by the `parent_tool_use_id` they carried (the id of the `Agent` call):

| # | type / subtype | parent | content |
|---|---|---|---|
| 1-2 | `command_lifecycle` | | queued, started |
| 3 | `system/init` | | `tools` lists `Task`, not `Agent` |
| 4-8 | `system/thinking_tokens` | | |
| 9 | `assistant` | null | `thinking`, empty text, with a signature |
| 10 | `assistant` | null | `tool_use` named `Agent`, input `description` and `prompt` only |
| 11 | `system/background_tasks_changed` | | one `local_agent` task |
| 12 | `system/task_started` | | `tool_use_id` of the call, `is_backgrounded: true`, `subagent_type: general-purpose`, the prompt |
| 13 | `user` | null | `tool_result` of the call: "Async agent launched successfully", and `tool_use_result.status: async_launched` |
| 14 | `rate_limit_event` | | |
| 15-16 | `system/thinking_tokens` | | |
| 17-18 | `assistant` | null | empty `thinking`; text "I've launched a subagent to count the files. Waiting for the result..." |
| 19 | `result/success` | | the first turn ends |
| 20 | `command_lifecycle` | | completed |
| 21-22 | `assistant` | call | empty `thinking`; `tool_use` Bash `find ... -type f \| wc -l` |
| 23 | `system/task_progress` | | `tool_use_id` of the call |
| 24 | `user` | call | `tool_result` "3" |
| 25-26 | `assistant` | call | empty `thinking`; text "3" |
| 27 | `system/background_tasks_changed` | | no tasks |
| 28 | `system/task_updated` | | status completed |
| 29 | `system/task_notification` | | `tool_use_id` of the call, `summary: "3"`, `status: completed` |
| 30 | `system/init` | | a second init in the same process |
| 31-32 | `assistant` | null | empty `thinking`; text "3" |
| 33 | `result/success` | | the second turn ends |

The subagent's `assistant` and `user` lines also carried
`subagent_type` and `task_description`. The daemon journaled
`harness_exited` with exit code 0 after line 33.

## How it differed from the fixture

- **The subagent ran in the background.** The fixture had the
  `Agent` call's result carry the subagent's answer, and the turn ended
  after it. In the run, the call's result came at once and said only
  that the agent had been launched; the turn ended (line 19) before
  any of the subagent's lines; the answer reached the main
  conversation through `system/task_notification`; and Claude Code
  then started a turn of its own, with a second `system/init` and a
  second `result`, without any prompt from the daemon. The call's
  input held no `run_in_background`, yet `task_started` says
  `is_backgrounded: true`. **UNVERIFIED:** why; whether Claude Code
  2.1.289 backgrounds every `Agent` call in `-p` stream-json mode, or
  the model chose it in a way the input does not show.
- **The subagent's prompt was in the call, not in a `user` line.** The
  fixture began the subagent with a `user` message holding its prompt.
  In the run the prompt was in the `Agent` call's input and in
  `system/task_started`.
- **The tool's names differ.** The fixture's `system/init` listed
  `Agent`. The run's listed `Task`, while the call was named `Agent`,
  as `docs/design/2026-10-09-agent-model.md` records for the tool lists
  of earlier runs.
- **Thinking was empty.** Every `thinking` block, the main
  conversation's and the subagent's, had empty text and a signature.
  The fixture had thinking text.
- **More system lines.** `command_lifecycle`, `system/thinking_tokens`,
  `system/background_tasks_changed`, `system/task_started`,
  `system/task_progress`, `system/task_updated` and
  `system/task_notification` were not in the fixture. The `task_*`
  lines name the call in `tool_use_id`, not `parent_tool_use_id`.

The run matched the fixture in three ways. Every `assistant` and
`user` line the subagent wrote carried the call's id in `parent_tool_use_id`, the main
conversation's lines carried `null`, and with `--forward-subagent-text`
the subagent's text reached the stream (line 26).

## What changed

- The fixture now follows the run, with ids, paths and signatures
  replaced, and with the lines of several kinds that the normaliser
  handles alike, such as `system/thinking_tokens`, removed;
  `internal/harness/claude/testdata/README.md` says what was left out.
- The normaliser, `Normalise` in
  `internal/harness/claude/normalise.go`, did not change. Run over the
  recorded lines, it gave each transcript entry made from the
  subagent's lines (21, 22 and 24 to 26) the call's id as its
  `ParentToolUseID`, and every other entry none. The GUI
  (`TranscriptEntries` in `internal/component/transcript.go`) nests an
  entry under the call its `ParentToolUseID` names, wherever in the
  transcript the entry falls, so subagent entries that arrive after
  the turn's `result` still fold under the call. The `task_*` lines
  become unknown entries at the top level of the transcript.
- The live test above checks the shape again on a newer Claude Code
  when run with `go test -tags live ./internal/daemon/`.

## Consequences seen, not acted on

- At line 19 the daemon saw a turn end with no prompt outstanding and
  closed the harness's input (`handleOutput` and `run` in
  `internal/daemon/task.go`). The process did not exit: it finished
  the subagent and ran the second turn first. The task stays running,
  and holds one of its daemon's `-slots-per-daemon` slots, until the
  process exits (README, "Scheduling"), so a background subagent
  lengthens the turn as the server sees it.
- The hand-back (`finalReply` in `internal/server/handback.go`) runs
  once the task is finished and takes the last main-conversation text
  since the process started. In this run that was the second turn's
  "3", not the first turn's "Waiting for the result...".
  **UNVERIFIED:** that the second turn always comes before the process
  exits; one run showed it.
- **UNVERIFIED:** the shape without `--forward-subagent-text`; the
  run passed that flag.
