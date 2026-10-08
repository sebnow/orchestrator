# Checking the ADRs' unverified claims: findings (2026-10-08)

The owner ruled that every claim marked **UNVERIFIED** in the ADRs and
the README is either verified or removed. This note records what was
established for the claims that needed new evidence: three live runs of
the daemon against Claude Code, the log of an earlier live run, and the
Claude Code and Anthropic documentation as published on 2026-10-08.
The ADRs cite it where they now state these facts.

Markers: **UNVERIFIED** flags a claim not observed in the runs below and
not confirmed in a primary source, including inferred causes.
Everything else was observed in the runs below, quoted from the pages
named where it is used, or read from the source files named.

Terms follow the [daemon live findings](2026-10-07-daemon-live-findings.md):
a "turn" runs from a prompt to the `result` Claude Code writes for it,
the "harness" is the `claude` process, the "gateway" is the daemon's MCP
server with the `permission` tool, and "seq" numbers a task's events
from 1.

## Method

Environment: the project owner's macOS workstation (Darwin 25.4.0,
arm64), Claude Code 2.1.289 logged in with a Claude subscription, model
`haiku`, Go 1.26.8 from the Nix dev shell. The harness ran with the
daemon's flags, built by `arguments` in
`internal/harness/claude/process.go`; they give the gateway a per-server
`timeout` of 100,000,000 ms.

Three live tests were added for this check, each in its own commit:

| Test | Package | Sessions |
| --- | --- | --- |
| `TestLiveGivenPermissionRequestUnansweredForSixMinutesWhenTheOwnerAllowsItThenTheHarnessWasStillWaitingAndTheTurnCompletes` | `internal/daemon` | 1 |
| `TestLiveGivenDaemonKilledMidTurnWhenItRestartsAndTheOwnerResumesThenTheTurnContinuesTheSessionAndFinishes` | `cmd/daemon` | 2 |
| `TestLiveGivenDaemonShutDownCleanlyMidTurnWhenItRestartsAndTheOwnerFollowsUpThenANewProcessContinuesTheSession` | `cmd/daemon` | 1 |

Run with, for example:

    nix develop -c go test -tags live -count=1 -timeout 20m -v \
      -run TestLiveGivenDaemonKilledMidTurn ./cmd/daemon/

The two `cmd/daemon` tests use the three-step task of the
[graceful pause spike](2026-10-07-graceful-pause-spike.md): three
sequential `ping -c 5 127.0.0.1` calls through the Bash tool, about 4 s
each, writing `DONE-1` to `DONE-3`, then `FINISHED`. Each acts one
second after the test allowed the first ping, so during that ping.

The runs used four model sessions: one for the permission test, two for
the kill test, and one for the clean-shutdown test, which failed before
its second. A first run of the kill test failed on a bug in the test,
a state check that did not accept `queued`, before any session started.
The excerpts below are from the test logs, kept in `/tmp` on the
owner's workstation and not committed; the tests' temporary
directories, with the journals, were removed.

## Findings

### A permission request waited six minutes

The agent asked to run `touch held.txt` (seq 7). The test held the
request unanswered and checked the task every 30 s:

    after 30s: running true, turns ended 0, pending permissions 1
    …
    after 6m0s: running true, turns ended 0, pending permissions 1

It then allowed the request. The file was created, the turn ended with
`result/success` "DONE" 6m1s after the request, and the harness exited
with code 0. Between the request and the answer, the harness wrote one
stdout line, a `rate_limit_event`. The request outlasted the 60 s and
5-minute timeouts that the [MCP page](https://code.claude.com/docs/en/mcp.md)
gives an HTTP server without a per-server `timeout`.

This shows a six-minute wait with the gateway's `timeout` set as the
daemon sets it, about 28 hours. **UNVERIFIED:** any wait longer than six
minutes, and the wait without that `timeout`.

### The harness outlived a killed daemon by 23 seconds

The kill test sent SIGKILL to the daemon during the first ping. Five
seconds later the harness was alive, adopted by `launchd`, with no
child process; its ping had had time to finish:

      PID  PPID STAT ELAPSED COMM
    26425     1        00:09 claude

It exited on its own 23.25 s after the SIGKILL, polled every 250 ms.
**UNVERIFIED:** what it did in those 23 s and why it exited when it
did; its stdout went nowhere, and the gateway it would ask for
permission had died with the daemon. The daemon does nothing about a
harness that outlives it; the test waits up to a minute for the harness
to exit before restarting the daemon, so the resumed process had the
session to itself.

### A turn cut off by the daemon's death resumed in the same session

The restarted daemon closed the cut-off turn as
[restart recovery](../adr/2026-10-08-restart-recovery.md) decides:

    seq 13 harness_exited {"exit_code":-1,"error":"daemon restarted during the turn"}

The server marked the task `paused`. On Resume, the daemon started a
new process (seq 14). It reported the first process's session id,
`706334b6-7e0a-49ad-9328-2da7e35574ea`, in every `system/init` and
`result`, asked for the second and third pings only, and ended:

    seq 38 result success session 706334b6-7e0a-49ad-9328-2da7e35574ea cost 0.08695370000000001: "DONE-3\n\nFINISHED"

So Claude Code resumed a session whose turn had been cut off, and the
agent carried on from the step after the one that had run. The harness
had exited before the resume, so the session file held whatever the
orphaned process wrote in its last 23 s.

### A turn interrupted by a clean shutdown ended `failed`

The clean-shutdown test sent SIGTERM to the daemon during the first
ping. The daemon exited 1.2 s later with status 0. Its log:

    17:59:59.560 level=INFO msg="shutting down: stopping tasks"
    18:00:00.753 level=INFO msg="process ended" task=OGRIE7AA7CV6SFYTJEN6WNMCTE exit_code=1 paused=false stopped=false

The harness exited with code 1, so the server marked the task `failed`,
which is terminal. The test could not send the follow-up prompt, and
the claim that such a task resumes after the restart was not tested.
[Task lifetime](../adr/2026-10-08-task-lifetime.md) expects a clean exit
and a `finished` task here; [restart recovery](../adr/2026-10-08-restart-recovery.md)
expects `finished` only "when the harness exits cleanly". The run did
not keep the harness's stderr. **UNVERIFIED:** why the harness exited
with 1, and whether it always does after the daemon's interrupt and
closed stdin; this was one run. The test now logs the `harness_exited`
event, which carries the end of stderr, before it checks the state.

### `rate_limit_event` repeats within a process

The scheduler's live yield test,
`TestLiveGivenRunningFillerOnTheOnlySlotWhenANormalTaskArrivesThenTheFillerYieldsTheNormalTaskRunsAndTheFillerFinishes`
in `cmd/daemon`, ran on 2026-10-08 at about 12:17 CEST; its log is
`/tmp/live1.log` on the owner's workstation. The filler task's resumed
process, from `harness_started` at seq 39 to `harness_exited` at seq
66, journaled two readings:

    seq 49 quota_observed {"status":"allowed","windows":[{"name":"five_hour","utilization":0.44,…},{"name":"seven_day","utilization":0.39,…}]}
    seq 63 quota_observed {"status":"allowed","windows":[{"name":"five_hour","utilization":0.45,…},{"name":"seven_day","utilization":0.39,…}]}

The daemon journals one `quota_observed` after each `rate_limit_event`
(`internal/harness/claude/process.go`). The
permission test's single process also sent two: the five-hour
utilization was 0.12 at seq 9 and 0.13 at seq 14. In both processes the
status stayed `allowed` and the five-hour utilization rose by 0.01
between the two events. The [mod versus stdout spike](2026-10-07-mod-vs-stdout-spike.md)
saw one per process. **UNVERIFIED:** what makes Claude Code send the
event again, such as a change in utilization or each model request;
the processes that sent it twice also made more than one model
request.

## Documentation, as published on 2026-10-08

### stream-json

The [CLI reference](https://code.claude.com/docs/en/cli-reference.md),
table of CLI flags:

- `--input-format`: "Specify input format for print mode (options:
  `text`, `stream-json`)"
- `--output-format`: "Specify output format for print mode (options:
  `text`, `json`, `stream-json`)"

The [headless page](https://code.claude.com/docs/en/headless.md) lists
"`stream-json`: newline-delimited JSON for real-time streaming". The
[Agent SDK streaming input page](https://code.claude.com/docs/en/agent-sdk/streaming-input.md)
shows a user message as `{"type": "user", "message": {"role": "user",
"content": …}}` in its Python example; it describes the SDK's input,
not the CLI's stdin.

None of the CLI reference, the headless page, the streaming input page
or the Agent SDK overview contains "subject to change", "may change",
"unstable", "experimental" or "early access". The "may change between
releases without notice" that the
[mod versus stdout spike](2026-10-07-mod-vs-stdout-spike.md) quotes is
from the type declarations for mods, not for stream-json.

### The stdin interrupt

The [Agent SDK TypeScript reference](https://code.claude.com/docs/en/agent-sdk/typescript.md)
documents the SDK's `interrupt()` and, under the capability
`interrupt_cancel_queued_v1`: "The `interrupt` control request honors
`cancel_queued: true`". It describes a `control_request` envelope as
"`{ type: "control_request", request_id, request }`", and lets an
application send the `control_response` to a permission request
"outside the SDK". No page checked says that a program other than the
SDK may write an interrupt `control_request` to the CLI's stdin, or
calls that format a supported interface.

### Subscription use

The [Consumer Terms](https://www.anthropic.com/legal/consumer-terms),
effective October 8, 2025, forbid "Except when you are accessing our
Services via an Anthropic API Key or where we otherwise explicitly
permit it, to access the Services through automated or non-human means,
whether through a bot, script, or otherwise." and say "You may not
share your Account login information, Anthropic API key, or Account
credentials with anyone else or make your Account available to anyone
else."

The [authentication page](https://code.claude.com/docs/en/authentication.md):
"For CI pipelines, scripts, or other environments where interactive
browser login isn't available, generate a one-year OAuth token with
`claude setup-token`".

The [legal and compliance page](https://code.claude.com/docs/en/legal-and-compliance.md),
"Authentication and credential use", after directing developers of
products to API keys: "Nor does it prevent an end user from signing in
to the unmodified Claude Code binary with their own Claude
subscription". Under "Acceptable use": "Advertised usage limits for Pro
and Max plans assume ordinary, individual usage of Claude Code and the
Agent SDK."

The support article
[Use the Claude Agent SDK with your Claude plan](https://support.claude.com/en/articles/15036540-use-the-claude-agent-sdk-with-your-claude-plan),
update of October 7, 2026: "You can still use the Claude Agent SDK,
`claude -p`, and third-party apps with your subscription limits."

## Taken from earlier notes

The other claims the sweep verified already had evidence on record:

- The `user_message_uuid` echo:
  [daemon live findings](2026-10-07-daemon-live-findings.md), "The
  `user_message_uuid` echo".
- Pause acknowledgement and compliance: the same note, "Acknowledgement
  through the gateway tool" (6 of 6 runs) and "Pause compliance" (3 of
  3 graceful runs).
- Resuming a pause with `--resume` in a new process:
  [resume spike](2026-10-08-resume-spike.md), Answers 5.
- The session cookie on a typed navigation:
  [browser check](2026-10-08-browser-check.md), finding 5.

## Open

- **UNVERIFIED:** why the harness exited with code 1 after a clean
  shutdown's interrupt; rerunning the clean-shutdown test captures the
  stderr.
- A daemon restarted within the 23 s the harness outlived it would
  resume the session while the old process still ran. Nothing prevents
  that today.
