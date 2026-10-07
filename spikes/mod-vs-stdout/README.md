# Spike: mod versus stdout as the harness adapter

Runs `claude -p --input-format stream-json --output-format stream-json
--verbose --model haiku` with and without a Claude Code mod, and records
what the mod sees next to what stdout carries. Findings are in
`docs/design/2026-10-07-mod-vs-stdout-spike.md` and, for the `pause-*`
scenarios, `docs/design/2026-10-07-graceful-pause-spike.md`.

## Contents

- `mod/`: the `spike-probe` mod. One hook on every event except
  `turn.step` (`on('!turn.step')`) and one generator hook on `turn.step`.
  Each hook POSTs the event, before and after `next(e)`, to
  `$SPIKE_RECEIVER_URL/event`. A `tool.check` hook asks
  `$SPIKE_RECEIVER_URL/decide` for a verdict. A 250 ms timer polls
  `$SPIKE_RECEIVER_URL/command` for commands: `abort` (`$.turn.abort`),
  `submit` (`$.prompt.submit`), `append` (`$.session.append` of a user
  row), `abort-after-tool` (abort the turn when the next tool call
  returns) and `snapshot`. With `SPIKE_QUIET=1` it does not report
  `tool.describe`, `command.describe`, `agent.offer` or other mods' `$`
  calls.
- `driver.py`: spawns `claude`, writes turns to its stdin, records each
  stdout line, and serves the receiver endpoints. All records go to one
  `events.jsonl` with timestamps from one clock.
- `perm_mcp.py`: a stdio MCP server exposing `approve`, for
  `--permission-prompt-tool`. It forwards to the driver's `/decide`.
- `timeline.py`: prints a one-line-per-record timeline of a run.
- `redact.py`: replaces e-mail addresses, account and organization ids,
  the telemetry device id and auth handles in run logs.
- `runs/<scenario>/`: the recorded runs (`events.jsonl`, `stdout.jsonl`;
  `debug.log` is kept locally and ignored).

## Scenarios

| Scenario | Mod | What it does |
| --- | --- | --- |
| `observe` | yes | `date` via Bash (allowed by `--allowedTools`), then a plain turn; `--include-partial-messages` |
| `control` | yes | `--permission-mode manual`; the receiver allows one `touch` and denies another; aborts a turn; submits a turn from the mod; one more stdin turn |
| `abort` | yes | aborts a turn 3 s into a foreground `ping -c 20 127.0.0.1`, then one more stdin turn |
| `mcp` | no | same permission prompt as `control` through `--permission-prompt-tool`; interrupts the `ping` with a stdin `control_request` |
| `pause-baseline` | yes, quiet | one turn of three sequential `ping -c 5 127.0.0.1` calls, uninterrupted |
| `pause-stdin` | yes, quiet | as `pause-baseline`; 1 s into the first ping, a pause request as a stdin user message; then a stdin resume turn |
| `pause-submit` | yes, quiet | the same pause request through `$.prompt.submit`; then resume |
| `pause-append` | yes, quiet | the same pause request through `$.session.append`; then resume |
| `pause-abort-after-tool` | yes, quiet | `$.turn.abort` from `tool.call` once the first ping returns; then resume |
| `pause-interrupt` | yes, quiet | a stdin `control_request` interrupt 1 s into the first ping; then resume |

Every run uses `--setting-sources project --strict-mcp-config` and an empty
temporary working directory, so the owner's installed plugins, settings
hooks, permission rules and MCP connectors stay out.

`runs/control-1` was produced by `control` when the long command was
`sleep 30`; `runs/control-2` by `control` as it is now. The engine refused
both commands in the foreground, which is why `abort` exists.

## Rerunning

Requires Claude Code 2.1.287 or later and Nix (the macOS `/usr/bin/python3`
shim needs the Xcode command line tools). Each run spends subscription
quota: two to four short Haiku turns.

    ./run.sh observe                  # writes runs/observe/
    ./run.sh control --out runs/control-3
    ./run.sh pause-stdin --out /tmp/pause-runs/pause-stdin
    nix shell nixpkgs#python3 -c python3 timeline.py runs/observe/events.jsonl
    nix shell nixpkgs#python3 -c python3 redact.py runs/*/events.jsonl runs/*/stdout.jsonl

Write runs outside the jj workspace (as in the last example) and redact
them before copying them in: the workspace auto-tracks new files, so any
jj command would snapshot unredacted logs.

Test the mod's permission path without a session:

    claude plugin validate ./mod
    claude plugin test ./mod

## Known artifacts of the probe

- The mod awaits each POST before calling `next`, so every event waits on
  the receiver. With about 60 `command.describe` events dispatched at once
  at start-up, some POSTs took 1 to 6 s. The likely cause is the Python
  receiver's listen backlog of 5 (not verified). In `runs/abort` this
  delayed the first turn by about 8 s. The driver now listens with a
  backlog of 128.
- Every run up to and including the `pause-*` runs was recorded with a
  `wait_result` that counted results again on each rescan of the log, so
  it could return early. Its visible effect: the driver closed stdin while
  the last turn of each `pause-*` run was still running; each such turn
  still finished and produced its `result` before the process exited. The
  driver now counts results without state.
- `runs/observe` was recorded before the mod skipped `engine.create`; there,
  that hook failed once (`$.env.get` on an empty `$`) and was passed on, as
  `debug.log` reports.
