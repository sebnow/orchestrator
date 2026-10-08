# The owner's interrupt: findings (2026-10-08)

The [shutdown findings](2026-10-08-shutdown-findings.md) saw Claude Code
exit 1 after a turn the daemon's shutdown interrupted; the last `result`
it wrote was the interrupted turn's, an error. The owner's Interrupt
command sends the same interrupt. Before the change this note
describes, the daemon let the harness's exit code decide the task's
state, so an interrupt seemed likely to end the task `failed`. The
[client protocol](../adr/2026-10-07-client-protocol.md) says an
interrupt leaves the task's session alive, which a `failed` task, which
the server does not resume, would break. This note records the live run
that checked whether an interrupt ends the task `failed`, the change
made, and the run that checked the change.

Terms follow the [daemon live findings](2026-10-07-daemon-live-findings.md):
a "turn" runs from a prompt to the `result` Claude Code writes for it,
the "harness" is the `claude` process, and "seq" numbers a task's events
from 1.

Markers: **UNVERIFIED** flags a claim not observed in the runs below and
not read from a source named here.

## Method

Environment as in the shutdown findings: the owner's macOS workstation
(Darwin 25.4.0, arm64), Claude Code 2.1.289 logged in with a Claude
subscription, model `haiku`, Go 1.26.8 from the Nix dev shell. Both runs
are of one test in `cmd/daemon/live_test.go`,
`TestLiveGivenRunningTurnWhenTheOwnerInterruptsItThenTheTaskIsPausedAndResumeFinishesTheSteps`.
It runs `cmd/server` and `cmd/daemon`, with the daemon starting `claude`
through a wrapper script that logs each invocation. It starts the
three-step task of the
[graceful pause spike](2026-10-07-graceful-pause-spike.md): three
sequential `ping -c 5 127.0.0.1` calls through the Bash tool, writing
`DONE-1`, `DONE-2` and `DONE-3` after each, then `FINISHED`. The test
answers the agent's permission requests through the task page, allowing
only ping, and posts the task page's `interrupt` form one second after
it allowed the first ping. It then expects the task `paused`, resumes
it, and expects it to finish in the same session.

| Run | Daemon | Harness processes | Result |
| --- | --- | ---: | --- |
| 1, ended 19:04 CEST | before the change | 1 | failed: task `failed` |
| 2, after run 1 | with the change | 2 | passed |

The Harness processes column counts the `claude` invocations other than
`--version` that the wrapper script logged, three in all. The test logs
were not committed.

## Findings

### An interrupt during a tool call ended the turn in an error `result` and exit code 1

In run 1, the task's events after the interrupt included these, with
seq 15 abridged:

    seq 12 {"type":"control_response","response":{"subtype":"success","request_id":"interrupt-1","response":{"still_queued":[]}}}
    seq 15 result "subtype":"error_during_execution", "is_error":true, "terminal_reason":"aborted_tools", "stop_reason":"tool_use"
    seq 17 harness_exited {"exit_code":1}

The `result` also carried `"errors":["[ede_diagnostic] result_type=user
last_content_type=n/a stop_reason=tool_use"]`. The daemon took the turn
as over, since nothing it had sent was still unanswered, and closed the
harness's input. The harness exited 1 with nothing on stderr. The server
read the exit as a failure, and the task ended `failed`, a state the
server takes no Resume in. The two shutdown runs in the shutdown
findings showed the same three events after the daemon's interrupt.

### With the change, the task is paused and Resume continues the session

The change is in `internal/daemon` (`cutShortByInterrupt` in
`serve.go`, `Task.Interrupt` in `task.go`) and `internal/server`
(`cutShortOf` in `state.go`). The daemon treats a turn ended by the
owner's interrupt as it treats one its own shutdown cut short: unless
the harness exits with code 0, it reports `harness_exited` with exit
code -1 and the error `interrupted by the owner`, or `interrupted by the
owner, before the harness reported a session` when no session was
recorded. The server reads either as a turn cut short, leaves the task
`paused`, and gives "paused: interrupted by the owner" as the reason
the task needs attention. Resume tells the agent that the owner
interrupted its last turn. A prompt the owner sends after the interrupt
starts a turn of its own; if that turn's `result` is the last before the
harness exits and is an error, the task ends `failed` as before. Neither
run exercised the case without a session or a prompt after the
interrupt; the offline tests in
`internal/daemon/interrupt_internal_test.go` cover the second with a
fake harness.

In run 2 the interrupted turn ended with the same `control_response`
and `result` events (seq 12 and 15) as in run 1. The daemon logged the
harness's own exit as `exit_code=1 error=""` and reported:

    seq 17 harness_exited {"exit_code":-1,"error":"interrupted by the owner"}

The task was `paused`, and the dashboard gave "paused: interrupted by
the owner". After Resume, the agent in a second harness process
continued session `c188d269-be88-42f5-8f12-33d4efb2bd32`, called ping
three more times (four calls across both processes), wrote `DONE-3` and
`FINISHED`, and the process exited 0. The resumed agent ran the
interrupted first step again, as one agent did after a shutdown in the
shutdown findings.

## Open

- Both runs interrupted the turn during a tool call. **UNVERIFIED:** an
  interrupt while the model is generating text, with no tool call
  running, ends the turn the same way.
- The runs used Claude Code 2.1.289 only. The pause relies on the
  harness exiting with a code other than 0. If a later version exits 0
  after an interrupted turn, the daemon reports that exit as it is, and
  the server, by its rules in `internal/server/state.go`, marks the
  task `finished`.
