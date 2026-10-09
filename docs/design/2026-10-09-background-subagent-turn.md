# The turn of a background subagent (2026-10-09)

`docs/design/2026-10-09-subagent-stream.md` recorded a run in which
Claude Code ran one of its own subagents, started with its `Agent` tool,
in the background. Claude Code wrote a `result` before the subagent had
done anything, and the daemon took it as the end of the turn and closed
the harness's input. Claude Code then finished the subagent and, without
a new prompt, ran a turn of its own to report it. **UNVERIFIED:** that
Claude Code always does so once its input is closed; one run showed it.
The turn must not end while a background subagent runs: the daemon keeps
the harness's input open until the subagent is done and the follow-up
`result` arrives. This note records how that is done and the live runs
that checked it.

Markers: **UNVERIFIED** flags a claim not checked against a run, the
code, or a source named here.

## The rule

The Claude Code process driver, `turn` in
`internal/harness/claude/process.go`, sets `Output.TurnEnded` on a
`result` only when no subagent the process started is still running.
A `result` written while one runs is passed on as ordinary output.
Following subagents is the harness package's job, as
`docs/adr/2026-10-07-harness-independence.md` requires. As
`docs/adr/2026-10-07-client-protocol.md` says, the daemon detects the
end of a turn from a `result`; the harness decides which `result` that
is.

A subagent counts as running from its `system/task_started` with
`task_type: "local_agent"` until any one of these arrives:

- `system/task_notification` with the same `task_id`;
- `system/task_updated` with the same `task_id` and a `patch.status`
  of `completed`, `failed` or `killed`;
- `system/background_tasks_changed` with an empty `tasks` list, which
  ends every subagent being followed.

Any one signal is enough, so the turn still ends if a Claude Code
release stops sending one of them. The shapes are those of the Agent
SDK TypeScript reference
(<https://code.claude.com/docs/en/agent-sdk/typescript.md>,
`SDKTaskStartedMessage`, `SDKTaskNotificationMessage`,
`SDKTaskUpdatedMessage`, `SDKBackgroundTasksChangedMessage`), and they
match the runs below and `spikes/mod-vs-stdout/runs`. The subagent is
keyed by `task_id`: `task_updated` and `background_tasks_changed` carry
only `task_id`, and the reference marks `tool_use_id` optional.

Only subagents are followed. A `local_bash` task, which is a background
Bash command or a Monitor watch, may never end (a dev server, a
watcher), and in the spike Claude Code killed a background `sleep`
itself 5 s after its input closed
(`docs/design/2026-10-07-mod-vs-stdout-spike.md`). Tasks with
`ambient: true`, and tasks of type `remote_agent`, are not followed
either. A foreground subagent is followed too. In the first run below
its `task_notification` came before the call's `tool_result`, so it had
ended before any `result`.

The `result` written while a subagent runs echoes the prompt it answered
in `user_message_uuid`. The follow-up turn's `result` echoed none in the
second run below; it carried `origin.kind: "task-notification"` instead.
The driver therefore carries the prompt ids of withheld results to the
`result` that ends the turn, so `Output.Answering` there lists every
prompt the turn answered. Without this the daemon would keep the first
prompt outstanding and never close the input.

## The daemon

`handleOutput` and `run` in `internal/daemon/task.go` close the input on
a line with `TurnEnded` once no prompt the daemon sent is outstanding,
and the carried prompt ids clear the outstanding prompt on the line that
ends the turn. Until then the task reports `Busy`, and a prompt sent to
it goes to the harness's input. **UNVERIFIED:** what Claude Code does
with a prompt that arrives while a background subagent runs; no run has
sent one.

A pause settles when the turn that answers it ends, so a pause sent
while a background subagent runs settles only after the subagent and
the follow-up turn. This also applies to a pause the daemon interrupts
when the pause reaches its time limit.

## Fail-open and stuck cases

- **No `task_started`.** In a stream without one, every `result` ends
  a turn. If Claude Code stops sending `task_started`, or renames
  `local_agent`, turns end at the first `result` and the task does not
  get stuck.
- **No completion.** A Claude Code that reports a subagent started and
  never reports it finished leaves the task running, holding its slot,
  until the owner stops it. Stop closes the input itself and kills the
  harness if it outlasts the stop's deadline. There is no timeout.
  **UNVERIFIED:** whether an interrupt makes Claude Code end or report
  a running background subagent. If it does not, an interrupt leaves
  the turn open too.
- **The process exits.** The task ends when the harness exits,
  whatever subagents the driver was following.
- **A foreground subagent moved to the background.** An empty
  `background_tasks_changed` also makes the driver stop following a
  foreground subagent. If that subagent later moves to the background,
  the turn ends at the next `result`.

## The live runs

`TestLiveGivenPromptToUseASubagentWhenItRunsThenTheSubagentsMessagesCarryTheAgentCallAsTheirParent`
in `internal/daemon/live_test.go` wraps the harness to record how many
lines had been read when the daemon first closed the input, waits for
the harness to exit by itself, and checks that the close came after the
last `result`. It ran twice on 2026-10-09 with
`go test -tags live ./internal/daemon/ -run TestLiveGivenPromptToUseASubagent`,
on Claude Code 2.1.289 and haiku.

1. With the prompt "Use a subagent to count the files in this
   directory and then reply with the number only.", haiku's `Agent`
   call carried `"run_in_background": false`. The subagent ran in the
   foreground (`task_started` with `is_backgrounded: false`), and its
   prompt arrived as a `user` line carrying the call's id (a background
   subagent's prompt does not). The stream had one `result`, on line 23
   of 24; the daemon closed the input after it, and the harness exited
   0. The run cost $0.0687 by `total_cost_usd`.
   `docs/design/2026-10-09-subagent-stream.md` left open whether Claude
   Code backgrounds every `Agent` call; Claude Code 2.1.289 does not. In
   the run that note records, the call had no `run_in_background` field
   and was backgrounded. **UNVERIFIED:** that an absent field always
   means the background.
2. With the prompt "Use a subagent running in the background to count
   the files in this directory. Once it reports back, reply with the
   number only.", the call carried `"run_in_background": true`, and the
   31 lines had the shape of the run in
   `docs/design/2026-10-09-subagent-stream.md`:
   `background_tasks_changed` with one `local_agent` task,
   `task_started` with `is_backgrounded: true`, a first `result`
   ("Subagent is counting the files now. I'll report back when it
   finishes.") echoing the prompt's id, the subagent's lines,
   `background_tasks_changed` with no tasks, `task_updated`
   `completed`, `task_notification` `completed`, a second
   `system/init`, and a second `result` ("3") with no echo. The daemon
   counted one turn ended, closed the input after line 31 of 31, the
   second `result`, and the harness exited 0. The run cost $0.0426.

The test keeps the second prompt, and fails when the subagent runs in
the foreground, since the background case is then not exercised.

`internal/harness/claude/testdata/subagent.jsonl` holds the
`background_tasks_changed` and `task_updated` lines, in the second
run's shape, and the unit tests in
`internal/harness/claude/turn_test.go` replay it: the first `result`
does not end the turn, the second does, each signal alone ends the
subagent, a stream without task lines ends a turn at every `result`,
and a subagent never reported finished leaves the turn open.
