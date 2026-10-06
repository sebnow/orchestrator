# Orchestrator: initial brainstorm (2026-10-06)

Brainstorm notes for the orchestrator project, dated 2026-10-06. Later
decisions belong in architecture decision records (ADRs) under `docs/adr/`;
this file is the source they draw from and is not updated after the fact.

Markers: **UNVERIFIED** flags a claim that was not confirmed against a
primary source. Other claims come from the sources cited in each section or
from the experiment recorded below.

Terms used throughout:

- **Owner**: the single person who runs the server and the client machines.
- **Harness**: the Claude Code CLI process (`claude`) that runs an agent.
- **MCP**: Model Context Protocol, the mechanism by which Claude Code calls
  external tools.
- **Mod**: a Claude Code plugin written as JS/TS function hooks that run
  inside the Claude Code process. Described under Open questions.

## Problem

Run Claude Code agents unattended on machines nobody has to watch (cloud
VPSes, and the owner's own laptop), with one place to command them, watch
them live, and keep what they produce.

Goals:

- Safety by machine isolation. The whole VPS is the sandbox, so the agent can
  run with broad permissions and no in-process sandboxing.
- Agents in the cloud rather than tied to a workstation.
- Centralised steering: one exposed web interface for the human, one
  protocol for the clients, no NAT or per-machine access problems.
- Centralised transcripts, kept as a record and later fed back into agent
  sessions (see shared memory, next item).
- Later: shared memory across agents (context and knowledge gained from each
  session), and the server provisioning machines itself.

The server is the brain. It owns the system prompt, picks model settings when
the task does not, places agents on machines, and routes messages between
them. Coordination logic lives in the server, not in an agent.

## Shape of the system

Three components, Go, in one monorepo:

- **Server.** Runs on a VPS. Accepts tasks, stores sessions and transcripts,
  serves a web interface and API for the human, speaks one protocol to
  clients. SQLite for storage.
- **Client daemon.** Runs on each machine that executes agents. Dials out to
  the server. Prepares a workspace (optionally cloning a repository at a
  ref; otherwise an empty directory), spawns the harness, adapts its output
  into the client protocol, streams the session live, and buffers when
  disconnected.
- **Harness.** `claude -p --input-format stream-json --output-format
  stream-json`, driven by the daemon. The Claude Agent SDK is available only
  for TypeScript and Python, so the daemon drives the CLI process directly.

A task is a prompt plus an optional workspace specification plus optional
model settings. Agents can ask the server to spawn further agents; placement
is the server's decision and invisible to the requesting agent. Agents
communicate asynchronously through per-agent inboxes.

## Decisions

Each entry: the decision, then why.

1. **Single user.** Multi-user collaboration is deferred; auth and tenancy
   work would otherwise come before the core exists.

2. **Client dials server; one protocol, one web interface.** Removes NAT and
   per-machine exposure problems. The owner's laptop is a client too, so a
   VPS is not required to participate.

3. **Bring-your-own machine first.** The operator provisions a machine with
   the daemon installed and points it at the server. Server-driven
   provisioning comes later.

4. **Agent survives a lost connection.** The daemon keeps the harness
   running, buffers transcript events, and replays them on reconnect.

5. **Task interface is a prompt.** Model settings optional and otherwise
   chosen by the server. System prompt owned by the server. Workspace
   specification (repository, ref) explicit in the task and optional; most
   tasks will use a repository but it is not required.

6. **Steering is limited to human-in-the-middle moments.** Permission
   prompts and similar. The goal is hands-off operation. Permission answers
   cannot be delivered over stdin in `-p` mode (see Research notes: Claude
   Code headless interface); they must go through `--permission-prompt-tool`,
   an MCP tool the harness calls and waits on. The daemon therefore hosts an
   MCP server for the harness.

7. **Asynchronous messages with per-agent inboxes.** Parents are not blocked
   on children. Placement does not matter to the parent. How the inbox is
   exposed to the agent (MCP tool, mod, or both) is open.

8. **Subscription billing, not API key.** The harness authenticates with
   `CLAUDE_CODE_OAUTH_TOKEN`, a one-year token produced by `claude
   setup-token` on an interactive machine. This is documented for headless
   and CI use. The Claude Agent SDK is ruled out because its documentation
   requires API key authentication (see Research notes: Claude Agent SDK and
   subscription billing).

9. **Go daemon drives the CLI directly.** Follow-up turns over stdin
   mid-session were confirmed working empirically (see Research notes:
   Experiment: follow-up turns over stdin mid-session). The adapter should
   be isolated behind an interface because the stream-json format is
   undocumented in places and, **UNVERIFIED**, declared subject to change.

10. **Task credentials must not be easily reachable by the agent.** Git
    tokens, cloud keys, anything a task needs. A vault-style mechanism is
    preferred. The Anthropic OAuth token is out of scope: the agent's Bash
    tool runs as the same OS user as `claude`, which can read the token, so
    the daemon does not try to hide it.

11. **SQLite until it becomes limiting.** One user, a handful of agents, one
    binary on a VPS. Postgres when SQLite is the bottleneck.

12. **Repository.** Jujutsu for version control, monorepo, Go. A Nix flake
    dev shell pins Go and tooling so every machine builds the same way.
    Decisions are recorded as ADRs rather than in a single specification
    document.

13. **Transcripts are a record and an input.** Cross-agent memory is
    deferred.

## Open questions

Not decided. Facts gathered so far are listed under each.

### Mod versus stdout as the harness adapter

Claude Code mods are a candidate adapter: the daemon spawns `claude -p` with
a mod loaded, and the mod talks to the daemon instead of the daemon parsing
stdout. To be adopted if it is more powerful or more informative than
stdout. Needs an empirical spike. Facts:

- Mods are JS/TS function hooks living in a plugin's `hooks/` directory,
  listed under `modules` in `hooks/hooks.json`, loaded with `--plugin-dir`
  or plugin install. They run inside Claude Code's hooks worker, unlike
  shell hooks which run as external processes.
- Hooks fire in `claude -p` runs; UI rendering is silently ignored there.
- Event surface includes `tool.call`, `tool.check`, `turn.start`,
  `turn.step`, `turn.complete`, `session.start`, `session.end`,
  `session.compact`, `session.send`, `session.receive`, `agent.spawn`,
  `prompt.submit`, and `classic.<Event>` for compatibility with settings
  hooks.
- Outbound: `$.http.fetch` (HTTP/HTTPS, one request/response) and
  `$.process.spawn`. **UNVERIFIED:** no documented WebSocket or persistent
  connection facility.
- Inbound: `$.prompt.submit({ text, asUser })` enqueues a prompt for when
  the session is idle. No documented way to answer a permission prompt or
  interrupt a running turn from a mod.
- Minimum version 2.1.287 for the terminal client. **UNVERIFIED:**
  stability status; not labelled stable or experimental, was behind an
  environment flag during early access.
- What stdout offers that a mod does not: `result` messages with
  `session_id`, `subtype`, `usage`, `total_cost_usd`, and the full assistant
  message stream in one place.

Sources: https://code.claude.com/docs/en/plugins/mods/overview.md,
https://code.claude.com/docs/en/plugins/mods/reference.md,
https://code.claude.com/docs/en/plugins/mods/api.md,
https://code.claude.com/docs/en/plugins/mods/create.md

### Client/server authentication and exposure

Leaning: certificate based, since the server will later build the VPS and
can issue the certificate at that point. A VPN or private network would be
good but should not be required.

### How agents reach the server

Spawning agents, reading and writing inboxes, answering permission prompts.
Candidates: an MCP server hosted by the daemon (already required by
decision 6), a mod, or both.

### Credential isolation mechanism

How task secrets stay out of the agent's reach while the harness and the
daemon can use them. Ideas, not evaluated: a credential helper or proxy in
the daemon so the agent never holds long-lived tokens.

### Subscription concurrency

No documented cap on concurrent sessions per account. Limits are token
quotas per five-hour and weekly window on the Pro and Max subscription
plans. **UNVERIFIED:** the documentation does not mention a cap; its
silence is the only evidence. Whether running many agents in parallel fits
in one subscription's quota is unknown.

### Task submission interface

A web API is decided. Whether tasks are also submitted via a CLI, the web
UI, or both is open.

### Cross-machine session resume

Possible but not free: the `.jsonl` transcript must be copied to
`~/.claude/projects/<project>/` on the target and all original flags
(`--mcp-config`, `--settings`, `--add-dir`, `--model`, `--agent`) re-passed
with `--resume <session-id>`. Whether this is needed at all is undecided.
Source: https://code.claude.com/docs/en/sessions.md

## Research notes

### Claude Code headless interface

Source: https://code.claude.com/docs/en/headless.md,
https://code.claude.com/docs/en/cli-reference.md,
https://code.claude.com/docs/en/sessions.md,
https://code.claude.com/docs/en/mcp.md

- Input over stdin with `--input-format stream-json` is newline-delimited
  JSON. **UNVERIFIED** in documentation; confirmed by experiment below:
  `{"type":"user","message":{"role":"user","content":"..."}}` works.
- Output message types documented: `system` (subtypes `init`, `api_retry`,
  `plugin_install`), `assistant`, `user` (with `--replay-user-messages`),
  `result`, `stream_event` (with `--include-partial-messages`),
  `permission_denied`. Observed but undocumented in the same run:
  `rate_limit_event`, and `system` subtypes `hook_started`, `hook_response`,
  `thinking_tokens`, `commands_changed`.
- `result` carries `subtype` (`success`, `error_during_execution`,
  `error_max_turns`, ...), `duration_ms`, `total_cost_usd`, `usage`,
  `session_id`, `result`.
- Permissions: `--permission-mode` (`default`, `manual`, `acceptEdits`,
  `plan`, `auto`, `dontAsk`, `bypassPermissions`), `--permission-prompts`
  (`host` or `none` in `-p`), `--permission-prompt-tool <mcp_tool_name>`,
  `--allowedTools`, `--disallowedTools`. Permission decisions are only
  deliverable via the MCP permission tool, not stdin.
- MCP: `--mcp-config <path|json>` accepts `http`, `stdio`, `sse`, `ws`
  servers; `--strict-mcp-config` ignores all other MCP configuration.
- System prompt: `--system-prompt`, `--system-prompt-file`,
  `--append-system-prompt`, `--append-system-prompt-file`; all work in `-p`.
- `--bare` mode requires `ANTHROPIC_API_KEY` and does not read
  `CLAUDE_CODE_OAUTH_TOKEN`.

### Experiment: follow-up turns over stdin mid-session

Run on 2026-10-06 on the owner's machine, Claude Code 2.1.289, logged in
with a Claude subscription:

    ( printf '%s\n' '{"type":"user","message":{"role":"user","content":"Reply with exactly the word ONE and nothing else."}}'
      sleep 25
      printf '%s\n' '{"type":"user","message":{"role":"user","content":"Now reply with exactly the word TWO and nothing else."}}'
      sleep 25 ) \
    | claude -p --input-format stream-json --output-format stream-json --verbose --model haiku

Observed timeline:

- 22:56:35 process started, first message written
- 22:56:37 `system` subtype `init`
- 22:56:41 `assistant` text `ONE`, then `result` subtype `success`, text
  `ONE`
- 22:57:00 second message written after the 25 s pause; process had been
  idle waiting on stdin; `system` subtype `init` emitted again
- 22:57:02 `assistant` text `TWO`
- 22:57:05 `result` subtype `success`, text `TWO`, same `session_id`
- process exited when stdin closed

Conclusion: a single `claude -p` process accepts multiple user turns over
stdin, stays alive between them, and emits a `system/init` and a `result`
per turn. Steering by sending follow-up prompts is feasible without
restarting the harness.

### Subscription billing and headless use

Sources: https://code.claude.com/docs/en/authentication,
https://code.claude.com/docs/en/headless,
https://code.claude.com/docs/en/github-actions,
https://code.claude.com/docs/en/legal-and-compliance,
https://www.anthropic.com/legal/consumer-terms,
https://support.claude.com/en/articles/11647753-how-do-usage-and-length-limits-work

- `claude setup-token` runs a browser OAuth flow on an interactive machine
  and prints a token valid for one year. It is supplied to headless
  machines via `CLAUDE_CODE_OAUTH_TOKEN`. Requires Pro, Max, Team, or
  Enterprise.
- Headless and CI use with that token is documented (GitHub Actions, GitLab
  CI examples).
- Consumer terms forbid sharing account credentials with other people and
  forbid third parties from intermediating claude.ai credentials for their
  users. **UNVERIFIED:** a single-owner setup appears to be within the
  terms as quoted; collaborators would each need their own token.
- Stream-json output in `-p` mode is documented as working under
  subscription auth (headless docs, "Get structured output"), and worked
  in the experiment above.

### Claude Agent SDK and subscription billing

Sources: https://code.claude.com/docs/en/agent-sdk/overview.md,
https://code.claude.com/docs/en/agent-sdk/quickstart.md,
https://code.claude.com/docs/en/legal-and-compliance.md,
https://support.claude.com/en/articles/15036540-use-the-claude-agent-sdk-with-your-claude-plan,
https://github.com/anthropics/claude-agent-sdk-python/issues/559

- SDK overview and quickstart: "Unless previously approved, Anthropic does
  not allow third party developers to offer claude.ai login or rate limits
  for their products, including agents built on the Claude Agent SDK. Use
  the API key authentication methods."
- Legal page: developers "including those using the Agent SDK, should use
  API key authentication"; Anthropic "does not permit third-party
  developers to ... route requests through Free, Pro, or Max plan
  credentials on behalf of their users."
- A support article describes a June 2026 plan for separate Agent SDK
  credits on subscriptions, then says the rollout was paused and "Agent SDK
  usage still draws from your subscription's usage limits." **UNVERIFIED:**
  whether personal SDK use on one's own plan is tolerated; the prohibitions
  are worded around offering it to other users.
- The SDK (TypeScript `@anthropic-ai/claude-agent-sdk`, Python
  `claude-agent-sdk`) wraps the CLI as a subprocess and offers typed
  messages, a `canUseTool` permission callback, lifecycle hooks, and a
  pluggable session store. There is no Go SDK.

See decision 9.

### Multi-agent coordination patterns surveyed

Sources: https://www.anthropic.com/engineering/multi-agent-research-system,
https://code.claude.com/docs/en/agent-teams,
https://developers.openai.com/api/docs/guides/agents/orchestration,
https://microsoft.github.io/autogen/stable/user-guide/core-user-guide/index.html,
https://docs.langchain.com/oss/python/langgraph/use-graph-api,
https://github.com/ruvnet/claude-flow

Two families:

- Blocking supervisor (Anthropic research system, OpenAI agents-as-tools,
  LangGraph supervisor): parent waits, result returns to parent. Single
  process; no cross-machine placement documented.
- Asynchronous message passing (AutoGen actor model, Claude Code agent
  teams, claude-flow): children addressed by name or ID, results arrive as
  messages or via shared state, parent not blocked. Of the systems
  surveyed, only these support multiple machines, and each needs explicit
  infrastructure for it (gRPC runtime and message queues in AutoGen; JSON
  mailbox files under `~/.claude/teams/` in Claude Code teams, same machine
  only as documented; SQLite plus JSON messaging in claude-flow).

Failure handling: per-agent timeouts and isolated failure domains in the
async systems, plus a shutdown handshake in Claude Code teams; retries and
letting the parent adapt in the blocking ones.

This survey informed decision 7 (asynchronous inboxes).

## Environment at time of writing

- Owner's machine: macOS, `jj` 0.45.1, Claude Code 2.1.289.
- Other repositories by the same owner use `flake.nix`; this one does too.
