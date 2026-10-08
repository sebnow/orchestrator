# Clean shutdown and a harness that outlives the daemon: findings (2026-10-08)

The [marker verification](2026-10-08-marker-verification.md) left two
findings open. A turn interrupted by a clean shutdown ended `failed`,
with the harness exiting 1 for a reason the run did not record. After
SIGKILL of the daemon, the harness lived another 23 s, and nothing
stopped a restarted daemon from resuming the session while it ran. This
note records the three live runs made to settle them, the code in
Claude Code that appears to set the exit code, and what the daemon does
in both cases since [shutdown recovery](../adr/2026-10-08-shutdown-recovery.md).

Terms follow the [daemon live findings](2026-10-07-daemon-live-findings.md):
a "turn" runs from a prompt to the `result` Claude Code writes for it,
the "harness" is the `claude` process, the "gateway" is the daemon's MCP
server with the `permission` tool, and "seq" numbers a task's events
from 1.

Markers: **UNVERIFIED** flags a claim not observed in the runs below and
not read from a source named here, including inferred causes.

## Method

Environment as in the marker verification: the owner's macOS
workstation (Darwin 25.4.0, arm64), Claude Code 2.1.289 logged in with
a Claude subscription, model `haiku`, Go 1.26.8 from the Nix dev shell.
The tests are in `cmd/daemon/live_test.go` and use the three-step task
of the [graceful pause spike](2026-10-07-graceful-pause-spike.md): three
sequential `ping -c 5 127.0.0.1` calls through the Bash tool, then
`FINISHED`. Each signals the daemon one second after the test allowed
the first ping.

| Run, ended | Test | Daemon | Sessions | Result |
| --- | --- | --- | ---: | --- |
| 1, 18:10 CEST | `TestLiveGivenDaemonShutDownCleanlyMidTurnWhenItRestartsAndTheOwnerFollowsUpThenANewProcessContinuesTheSession` | before shutdown recovery | 1 | failed, as before: task `failed` |
| 2, 18:29 CEST | `TestLiveGivenDaemonShutDownCleanlyMidTurnWhenItRestartsAndTheOwnerResumesThenTheTaskWasPausedAndTheTurnContinuesTheSession` | with shutdown recovery | 2 | passed |
| 3, 18:30 CEST | `TestLiveGivenDaemonKilledMidTurnWhenItRestartsAndTheOwnerResumesThenTheOldHarnessIsGoneAndTheTurnContinuesTheSession` | with shutdown recovery | 2 | passed |

The Sessions column counts the `claude` invocations other than
`--version` that the tests' wrapper script logged, five in all; each is
one harness process. The test logs are in `/tmp` on the owner's
workstation and not committed.

## Findings

### Both interrupted runs exited 1 with an error `result` last

Run 1 repeated the failing clean-shutdown test with the `harness_exited`
event logged in full. After SIGTERM, the daemon interrupted the turn and
closed the harness's stdin. The harness then wrote these, among other
lines:

    seq 12 {"type":"control_response","response":{"subtype":"success","request_id":"interrupt-1","response":{"still_queued":[]}}}
    seq 15 result error_during_execution, "is_error":true, "terminal_reason":"aborted_tools"
    seq 17 harness_exited {"exit_code":1}

The harness wrote nothing to stderr; the event omits `stderr` when it is
empty (`internal/protocol/protocol.go`). The interrupt receipt and the
turn's `result` both arrived although stdin was closed right after the
interrupt was written. Run 2 ended the same way: the daemon logged the
harness's own exit as `exit_code=1 error=""`, again with nothing on
stderr. In both runs the `result` at seq 15 was the last `result`.

**UNVERIFIED** (code read, not traced): the exit code is set at the end
of Claude Code's headless run. The 2.1.289 executable,
`bin/.claude-wrapped` in the Nix package, embeds its JavaScript; at
byte offset 206444977 the function that writes the run's messages to
stdout ends with:

    ms(Dr?.type==="result"&&Dr?.is_error||ks?1:A.outputFormat==="stream-json"?0:Jio()??0)

The loop before it sets `Dr` to each message it writes, apart from
messages excluded by checks this note did not trace, so `Dr` holds the
last message kept. The same function calls `ms(1)` right after writing
"Error: No messages returned from query." to stderr. If `ms` sets the
exit code, a stream-json run exits 1 when its last message is a
`result` with `is_error` set, or when `ks` is true, and 0 otherwise.
**UNVERIFIED:** that `ks` means the transport closed for good, as the
message the function writes when it is set suggests. An interrupted
turn's `result` is `error_during_execution` with `is_error: true`. This
fits the `mcp` run of the
[mod versus stdout spike](2026-10-07-mod-vs-stdout-spike.md), which
exited 0 after an interrupt: a further stdin prompt ran as a turn after
the interrupted one, and that turn's `result` came last. The minified
names are those of this build.

### A shutdown pauses the task, and Resume continues the session

Under shutdown recovery, the daemon reports a turn a shutdown cut short
in the same form as a restart: `harness_exited` with exit code -1 and
an error text the server reads as a pause for the owner. In run 2 the
daemon exited 1.2 s after SIGTERM, and the task's events ended:

    seq 15 result error_during_execution session 545cffbf-3b22-4334-9808-2c6923d18abe
    seq 17 harness_exited {"exit_code":-1,"error":"daemon stopped during the turn"}

The server showed the task `paused`. After the daemon restarted, the
test sent Resume. The new process (seq 18) reported the same session id
in every `system/init` and `result` and ended:

    seq 48 result success session 545cffbf-3b22-4334-9808-2c6923d18abe: "DONE-3\n\nFINISHED"
    seq 50 harness_exited {"exit_code":0}

The resumed turn asked for all three pings, so the agent ran the
interrupted first ping again, four pings in total. In the marker
verification's kill run and in run 3 the resumed agent went on from the
second ping instead. Three runs cannot show whether the agent usually
repeats the interrupted step or skips it.

### The restarted daemon waited for the old harness to exit

Under shutdown recovery, the daemon records each harness's pid and its
start time, as `ps -o lstart=` reports it, in the task's record
(`internal/daemon/orphan.go`, `internal/daemon/serve.go`). In run 3 the
test sent SIGKILL to the daemon during the first ping and restarted it
at once. The old harness, pid 46729, was alive when the new daemon
started. The new daemon logged that the old harness exited 18.04 s
after the daemon first found it running, within its 30 s wait, so it
did not kill it. Only then did it journal the cut-short turn:

    seq 12 harness_exited {"exit_code":-1,"error":"daemon restarted during the turn"}

The server showed the task `paused` 18.2 s after the SIGKILL, and the
old pid was gone by then. On Resume, the new process continued session
`4343234a-d620-4849-b940-6a74b93845da`, asked for the second and third
pings, and ended with `"DONE-3\n\nFINISHED"` and exit code 0.

So in this run the old harness exited between 18.04 s and 18.2 s after
the SIGKILL; the marker verification measured 23.25 s.
**UNVERIFIED:** what the harness does in that time and why it then
exits. The daemon that read its stdout, and the gateway it would call,
had died.

## Open

- The owner's Interrupt command ends the turn the same way, and the
  daemon closes stdin once the interrupted turn's `result` arrives with
  nothing else outstanding (`handleOutput` in `internal/daemon/task.go`).
  **UNVERIFIED:** that the harness then exits 1 and the task fails; no
  run interrupted a task through the GUI.
- The guard against pid reuse compares start times with second
  resolution. A process that reuses the harness's pid within the same
  second as the harness started would be taken for it.
  **UNVERIFIED:** that `ps -o lstart=` gives the same output on Linux;
  it was run on macOS only.
- [Task lifetime](../adr/2026-10-08-task-lifetime.md), Consequences,
  still says that a turn a shutdown interrupts ends `finished`, and that
  a daemon that crashed mid-turn marks the task `failed`.
- The doc comment of `HarnessExited` in `internal/protocol/protocol.go`
  says exit code -1 means the process never started or was killed; it
  does not mention a turn the daemon reports cut short.
