# Remote subagents (2026-10-09)

The owner requires that an agent's work stay on the daemon's machine.
Claude Code 2.1.289's `Agent` tool, which starts one of Claude Code's
own subagents, has an `isolation` parameter whose value `"remote"` runs
the subagent in a cloud environment. The tool's schema says:

> Isolation mode. "worktree" creates a temporary git worktree so the
> agent works on an isolated copy of the repo. "remote" launches the
> agent in a remote cloud environment (always runs in background;
> availability is gated).

This note records how Claude Code handles `Agent` calls, the rule the
orchestrator applies to remote ones, and the live run that checked it.

Markers: **UNVERIFIED** flags a claim not checked against a run, the
code, or a source named here.

The public documentation does not describe `"remote"`. The permissions
page (<https://code.claude.com/docs/en/permissions.md>, "Match by input
parameter") gives `Agent(isolation:worktree)` as an example of a rule
that matches an input parameter, the syntax the rule below uses.
`TaskStarted` in `internal/harness/claude/message.go` names a
`remote_agent` task type. **UNVERIFIED:** that the Agent SDK TypeScript
reference (<https://code.claude.com/docs/en/agent-sdk/typescript.md>)
lists it; this was not checked against the reference.

## What the binary shows

These points come from strings in the minified JavaScript bundled in
the Claude Code 2.1.289 executable (`bin/.claude-wrapped` in its Nix
store path). Its function names are minified, so the reading of each
is an inference. **UNVERIFIED** as behaviour; no run exercised them.

- The schema declares `isolation` as an optional enum of `"worktree"`
  and `"remote"`.
- The isolation a subagent gets is the call's `isolation`, or, when the
  call has none, the `isolation` of the agent definition it names.
- A gate decides whether `"remote"` is available. It is off when
  `CLAUDE_CODE_EVAL_CONFINED` or `CLAUDE_CODE_REMOTE` is set, when a
  runtime flag `remoteAgentIsolationDisabled` is set, when either
  `hasUsedRemoteSession` or `hasRemoteEnvironment` is unset (the code
  reads them from two different configuration objects), and unless the
  feature flag `tengu_neapolitan` is on. No setting or CLI flag sets
  `remoteAgentIsolationDisabled` (only `claude mcp serve` does), so the
  daemon cannot turn the gate off.
- With the gate off, Claude Code runs the subagent on the machine
  instead of failing the call. It writes a debug log line,
  "[remote agent] isolation:'remote' is unavailable", followed by
  "(no claude.ai login or feature gate off); falling back to
  isolation:'worktree'" when the session has a git root, and otherwise
  by "and no git root; running as a local agent".
- A forked subagent refuses `"remote"`: "Fork cannot use isolation:
  \"remote\" — a remote session cannot inherit the conversation
  context."

On 2026-10-09 the developer's `~/.claude.json` held neither
`hasUsedRemoteSession` nor `hasRemoteEnvironment`. **UNVERIFIED:** that
the gate was off for that login; which files the two configuration
objects read was not checked.

## Without an ask rule, Agent calls do not reach the permission tool

The daemon starts Claude Code with `--permission-mode default` and the
gateway's permission tool as `--permission-prompt-tool`
(`internal/harness/claude/process.go`). Under that mode Claude Code
starts a subagent without asking. A run on 2026-10-09, before the rule
below existed, of
`TestLiveGivenPromptToUseASubagentWhenItRunsThenTheSubagentsMessagesCarryTheAgentCallAsTheirParent`
(`internal/daemon/live_test.go`, haiku, $0.0707 by `total_cost_usd`)
logged `permission requests by tool, in order: []`. Neither the `Agent`
call nor the subagent's read-only Bash call made a permission request.
The `system/init` line listed the tool as `Task`; the `tool_use` block
named it `Agent`.

Without an ask rule, then, the server's permission policy does not see
`Agent` calls, under `allow-all` or `ask`.

## The rule

**The ask rule sends remote `Agent` calls to the permission tool, so
the server sees them.** The daemon passes
`--settings '{"permissions":{"ask":["Agent(isolation:remote)","Task(isolation:remote)"]}}'`.
The permissions page documents ask rules that match a top-level input
parameter by its exact value, and evaluates ask rules before allow
rules, so an allow rule in the project's settings does not bypass the
question. In the live run below the permission request named the tool
`Agent` and the `result`'s `permission_denials` named it `Task`. Both
names are listed because which name the rule matcher uses was not
checked (**UNVERIFIED**). `--setting-sources project` limits which
settings files load; the live run showed that settings passed with
`--settings` still apply.

The ask rule does not affect local subagents: it matches only a call
that sets `isolation` to `"remote"`. A run of the subagent live test
above with the ask rule in place again logged no permission request,
and its background subagent ran and finished as before.

**The server denies remote subagents under every policy.**
`IsRemoteSubagent` in `internal/harness/claude/remote.go` reports
whether a permission request is an `Agent` or `Task` call with
`isolation` `"remote"`. `decidePermission` in
`internal/server/policy.go` looks up the event's harness in
`offMachine`, a map keyed by harness name (the same pattern as
`normalisers` in `internal/server/transcript.go`), and denies a
matching request before it consults the policy. The denial carries
`claude.RemoteSubagentDenial`, which tells the agent to call the tool
again without `isolation` or with `"worktree"`. This holds for
`allow-all`; for `ask`, where the owner never sees the request; and for
a server built with no policy (`server.Options.Permissions` nil), which
otherwise asks the owner. Only the harness package names the tool and
the parameter.

The ask rule lets the server, not Claude Code, make the decision, so it
is kept in the command log and the task page shows "denied by policy",
in line with `docs/adr/2026-10-08-permission-policy.md`. A deny rule in
`--disallowedTools` would refuse the call inside Claude Code, with no
record on the server.

## The live run

`TestLiveGivenAllowAllWhenTheAgentAsksForARemoteSubagentThenTheServerDeniesItAndNoRemoteAgentStarts`
in `cmd/daemon/live_test.go` runs the server with `-permissions
allow-all` and the daemon, with a prompt asking for one `Agent` call
with `isolation` `"remote"` and a subagent prompt of "Reply with exactly
the word PONG." It ran once on 2026-10-09, with
`go test -count=1 -tags live -v ./cmd/daemon/ -run TestLiveGivenAllowAllWhenTheAgentAsksForARemoteSubagent`,
on Claude Code 2.1.289 and haiku, and passed in 8.8 s for $0.0210.

- Haiku's first tool call was `Agent` with
  `{"description":"Test remote subagent","prompt":"Reply with exactly the word PONG.","isolation":"remote"}`.
- The daemon recorded `permission_requested` with tool `Agent` and the
  same input. The server denied it with `RemoteSubagentDenial`, and the
  task page labels the answer "denied by policy".
- The call's `tool_result` was an error carrying the denial message.
  No `task_started` message of any type appeared in the stream, so
  Claude Code reported neither a remote agent nor a local fallback
  starting.
- Haiku made no further tool call, and replied: "The Agent tool with
  `isolation: "remote"` returned an error: subagents cannot run in a
  cloud environment in this context and must use the local machine
  instead." The `result` listed one permission denial, tool `Task`.

The gate's fallback log line is a debug message, and the run did not
enable debug output. **UNVERIFIED:** that the denial comes before
Claude Code checks the gate; the run says nothing about the gate on
this login. The prompt asked for one call, so whether an agent retries
locally after the denial was not tested.

## Not covered

- **Agent definitions.** **UNVERIFIED:** that a subagent whose
  definition sets `isolation: remote` runs remotely from a call without
  the parameter, as the binary reading above suggests, and that a
  definition's frontmatter accepts that field. Neither the ask rule nor
  `IsRemoteSubagent` would match such a call. **UNVERIFIED:** that the
  daemon, which loads project settings, also loads agent definitions
  from a repository's `.claude/agents/`.
- **`RemoteTrigger`.** The tool appears in the `system/init` tool list.
  **UNVERIFIED:** whether it runs work off the machine; it was not
  examined.
- **Nested subagents.** **UNVERIFIED:** that the ask rule applies to an
  `Agent` call a subagent makes. The hooks reference
  (<https://code.claude.com/docs/en/hooks.md>, "Hook locations") says
  hooks fire in subagents; no run checked permission rules there.
