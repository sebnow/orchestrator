`subagent.jsonl` follows the stream-json lines of a recorded run, made
on 2026-10-09 with Claude Code 2.1.289, haiku and
`--forward-subagent-text` by
`TestLiveGivenPromptToUseASubagentWhenItRunsThenTheSubagentsMessagesCarryTheAgentCallAsTheirParent`
in `internal/daemon/live_test.go`
(`docs/design/2026-10-09-subagent-stream.md`). Ids, paths, signatures,
costs and long texts are replaced, fields the normaliser does not read
are trimmed, and lines of types the normaliser ignores or treats as
unknown are left out except for one `task_started`, one `task_progress`
and one `task_notification`: the run also had `command_lifecycle`,
`system/thinking_tokens`, `system/background_tasks_changed`,
`system/task_updated` and `rate_limit_event` lines.

The order is the run's. The subagent ran in the background: the
`Agent` call's result says it was launched, the turn ended with a
`result`, the subagent's messages followed with the call's id in
`parent_tool_use_id`, and the harness then started a turn of its own,
with a second `system/init`, to report the subagent's answer. No `user`
message carries the subagent's prompt; `system/task_started` does.
