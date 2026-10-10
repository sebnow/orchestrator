---
status: accepted
date: 2026-10-10
supersedes: docs/adr/2026-10-07-harness-billing.md
source: >-
  owner decisions, 2026-10-10: no manual steps on client machines; each
  client has its own login; the server is the UI; quota is per account,
  not per deployment; [harness billing](2026-10-07-harness-billing.md),
  Decision, carried over
---

# Harness login through the server

## Context

[Harness billing](2026-10-07-harness-billing.md) decided the harness is
billed through a Claude subscription rather than an API key, ruled out
the Agent SDK for the reasons given there, and had the harness
authenticate with `CLAUDE_CODE_OAUTH_TOKEN`, produced by `claude
setup-token` on an interactive machine and handed to the daemon.

VPS daemons ([VPS provider survey](../design/2026-10-10-vps-providers.md))
have no browser, and the owner will not log in to client machines by
hand; the server is the owner's only interface to a daemon. `claude
setup-token` needs a browser on the machine it runs on, which a VPS
does not have.

Probed on 2026-10-10 with Claude Code 2.1.289 in an isolated
configuration directory: `claude auth login` without a browser prints
the authorisation URL and then waits on stdin with "Paste code here if
prompted >"; `claude auth status --json` reports `loggedIn` and
`authMethod`; `claude auth login` takes `--claudeai` (default),
`--console`, `--sso`, `--email`.

The [scheduling](2026-10-08-scheduling.md) record takes the newest
quota reading from any daemon as the account's budget, which is wrong
once a second account or provider is in use.

## Decision

Carried over from [harness billing](2026-10-07-harness-billing.md),
unchanged in substance: the harness is billed through a Claude
subscription, not an API key; the Agent SDK is ruled out for the
reasons given there; the daemon drives the unmodified CLI.

### Login belongs to the daemon

Each daemon has its own login, made and refreshed by the unmodified
`claude` binary in the harness user's configuration, never by the
server. `claude setup-token` is no longer the mechanism: the server
stores no login secret, and provisioning carries none.

### Logging in runs from the server

The owner starts a login on the daemon's page. The server sends a
login command over the command stream; the daemon runs `claude auth
login` as the harness user and reports the authorisation URL as an
event; the owner authorises in their own browser and pastes the code
the callback page shows into the server; the server sends the code as
a command; the daemon writes it to the login process's input and
reports the outcome. The same path serves a lapsed login.

### Login status as facts

The daemon reports login status as facts from `claude auth status
--json`, at connect and after each login: whether it is logged in, the
method, and the account identity when the harness reports one. The
dashboard shows daemons that need a login, and the scheduler does not
place turns on a daemon that is not logged in.

### Budget per account

Budget is per account: quota readings are keyed by the account
identity the daemon reports, together with the harness name; the
scheduler keeps one budget per account. A daemon that reports no
identity has a budget of its own, keyed by daemon id. This amends the
[scheduling](2026-10-08-scheduling.md) record's rule that the newest
`quota_observed` from any daemon is the account's reading, by
superseding that one point only; nothing else in that record changes.

### Protocol additions

Each addition is the only way to log a client in without a shell on
it: commands to start a login and to supply the code; events carrying
the authorisation URL and the outcome; the three facts above.

## Consequences

- The harness process can read its own login, as on the owner's
  machine today; keeping it out of the agent's reach needs the
  deferred credential proxy ([work stays on the
  machine](2026-10-09-work-stays-on-the-machine.md)).
- One browser authorisation per daemon, and again whenever a login
  lapses; the lifetime of a login is the harness's, not the
  orchestrator's.
- In two-user mode, `claude auth login` and `claude auth status` run
  through the same sudoers rule as the harness
  ([harness user](2026-10-08-harness-user.md)).
- A daemon whose login lapses mid-task reports it through the
  harness's own errors, and the task fails or pauses as those errors
  dictate; the login status fact turns the dashboard flag on.
- Nothing changes for a daemon on the owner's own machine already
  logged in; the status facts cover it too.

Revisit if the credential proxy ships and changes where the login
lives, or if the scheduling record's budget rules change again.
