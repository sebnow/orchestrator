`subagent.jsonl` was written by hand, not recorded: no recorded run had
a subagent. It follows the shape that
https://code.claude.com/docs/en/headless.md, "Follow subagent messages",
documents for a run with `--forward-subagent-text`: the subagent's
messages carry the id of the spawning `Agent` tool call in
`parent_tool_use_id`, its first message is a `user` message with its
prompt, and the main conversation's messages carry `null`. The other
fields copy the recorded runs under `spikes/mod-vs-stdout/runs`.
