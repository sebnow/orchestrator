---
status: accepted
date: 2026-10-06
source: [initial brainstorm](../design/2026-10-06-brainstorm.md), decision 8
---

# Harness billing

## Context

The harness, the Claude Code CLI, runs headless on unattended machines.
It can be billed through a Claude subscription or through an API key. The
orchestrator has a single user, the owner
([tenancy](2026-10-07-tenancy.md)).

- `claude setup-token` runs a browser OAuth flow on an interactive machine
  and prints a token valid for one year. Headless machines receive it
  through `CLAUDE_CODE_OAUTH_TOKEN`. It requires a Pro, Max, Team, or
  Enterprise plan (https://code.claude.com/docs/en/authentication).
- Headless and CI use with that token is documented, with GitHub Actions
  and GitLab CI examples. Stream-json output in `-p` mode is documented as
  working under subscription authentication
  (https://code.claude.com/docs/en/headless,
  https://code.claude.com/docs/en/github-actions).
- The Claude Agent SDK overview and quickstart state: "Unless previously
  approved, Anthropic does not allow third party developers to offer
  claude.ai login or rate limits for their products, including agents
  built on the Claude Agent SDK. Use the API key authentication methods."
  (https://code.claude.com/docs/en/agent-sdk/overview.md,
  https://code.claude.com/docs/en/agent-sdk/quickstart.md). The Legal and
  compliance page says developers "including those using the Agent SDK,
  should use API key authentication"
  (https://code.claude.com/docs/en/legal-and-compliance).
- `--bare` mode requires `ANTHROPIC_API_KEY` and does not read
  `CLAUDE_CODE_OAUTH_TOKEN`
  (https://code.claude.com/docs/en/cli-reference.md).

## Decision

The harness is billed through a Claude subscription rather than an API
key. It authenticates with `CLAUDE_CODE_OAUTH_TOKEN`, produced by
`claude setup-token`.

The Claude Agent SDK is ruled out because its documentation directs
developers to API key authentication. The daemon drives the CLI directly
instead ([harness integration](2026-10-07-harness-integration.md)). The
SDK prohibitions are worded around offering claude.ai login to other
users; **UNVERIFIED:** whether personal SDK use on one's own plan is
tolerated.

## Consequences

- The harness does not run in `--bare` mode.
- The consumer terms forbid sharing account credentials with other people
  and forbid third parties from intermediating claude.ai credentials for
  their users (https://www.anthropic.com/legal/consumer-terms).
  **UNVERIFIED:** a single-owner setup appears to be within these terms;
  collaborators would each need their own token.
- Usage is limited by token quotas per five-hour and weekly window on the
  Pro and Max plans. No cap on concurrent sessions per account is
  documented. **UNVERIFIED:** that no such cap exists; the
  documentation's silence is the only evidence. Whether running many
  agents in parallel fits in one subscription's quota is unknown
  (https://support.claude.com/en/articles/11647753-how-do-usage-and-length-limits-work).
