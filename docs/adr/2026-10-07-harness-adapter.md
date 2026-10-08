---
status: accepted
date: 2026-10-07
source: >-
  [mod versus stdout spike](../design/2026-10-07-mod-vs-stdout-spike.md),
  section "Recommendation"
---

# Harness adapter

## Context

The client daemon drives the harness, the Claude Code CLI, as `claude -p`
over stream-json, behind an adapter interface
([harness integration](2026-10-07-harness-integration.md)). That decision
was to be revisited if a spike showed that a Claude Code mod, JS/TS
function hooks running inside the Claude Code process, gives the daemon
more control over the session or more information than stdout does. A
spike on 2026-10-07 ran Claude Code 2.1.289 in `-p` with a probe mod
loaded, under a subscription login on a macOS workstation with managed
settings, and compared what the mod and stdout reported.

A mod can answer permission prompts and end turns in `-p`. The
[initial brainstorm](../design/2026-10-06-brainstorm.md) had found no
documented way to do either.

- A mod's `tool.check` hook saw Claude Code's own verdict, `ask`, and
  returned `allow` or `deny`, and Claude Code applied the mod's answer.
  The mod needed neither an MCP server nor `--permission-prompt-tool`.
  Answering through `--permission-prompt-tool` produced the same outcomes.
- The mod API call `$.turn.abort` ended a turn, but a running foreground
  command moved to the background and ran until the process exited. An
  interrupt written to stdin (`control_request` with subtype `interrupt`)
  stopped the running command. **UNVERIFIED:** that its format, which the
  headless documentation does not describe, is a supported interface.

A mod also sees the session id, usage, cumulative cost, and the full
content of the conversation, which the brainstorm had listed as available
only on stdout. Only the mod sees the context-window fill, the rate-limit
windows after every turn and on demand, the system reminders Claude Code
injects into the conversation, and Claude Code's own permission verdict
before a mod or the permission-prompt tool answers. Only stdout carries
the `result` metrics (`num_turns`, `duration_api_ms`, `ttft_ms`,
`modelUsage`), background-task messages, and thinking-token estimates.

A proposed scheduler would order tasks by priority against the
subscription's five-hour and weekly quota windows
([budget and pause](../design/2026-10-07-budget-and-pause.md)), and would
need quota figures for that. Stdout's `rate_limit_event` is undocumented
and arrived once per process in every run. The mod's `session.measure` is
documented and fired after every turn.

Reasons against adopting a mod now:

- The mod API is early access. The type declarations Claude Code 2.1.289
  writes for mods say "this surface may change between releases without
  notice".
- Hooks run on Claude Code's critical path. In one run, a hook that awaited
  its report to a slow receiver delayed the first prompt by 8.4 s.
- According to the mods reference
  (https://code.claude.com/docs/en/plugins/mods/reference.md), a
  built-in guard mod runs before user mods on a machine with managed
  settings or under a Team or Enterprise login, and withholds some events
  from them. On the test machine it withheld 13 event types. Agents are
  planned to run on cloud VPSes as well, and no VPS was tested, so what a
  mod sees depends on the machine and account. **UNVERIFIED:** that a VPS
  without managed settings, under a personal plan, does not run the guard.

## Decision

The harness adapter uses stream-json stdout and stdin only. No mod is
loaded into the harness.

Permission prompts stay on `--permission-prompt-tool`, as decided in
[human steering](2026-10-07-human-steering.md). The MCP server the daemon
hosts for that flag acts as the gateway between the harness and the
orchestrator server. This keeps permission policy out of the Claude Code
process and on a documented CLI flag.

Adding a mod is deferred. The intended mod is small: it only reports
`session.measure` to the daemon, without blocking.

## Consequences

- The adapter runs no code inside the harness. Its CLI flags are
  documented; the stdin interrupt and `rate_limit_event` are not.
- Until a mod is added, the daemon's only quota figures come from
  `rate_limit_event`. **UNVERIFIED:** that `rate_limit_event` is sent
  again within a process when the rate-limit status changes; in the
  spike, utilization rose only between processes.
- The daemon does not see the context-window fill, the injected system
  reminders, or Claude Code's own permission verdict.
- **UNVERIFIED:** that a non-blocking mod reporting `session.measure`
  gives the daemon the quota figures stdout lacks; no such mod was built.

Revisit if the scheduler needs quota figures that stdout does not give.
