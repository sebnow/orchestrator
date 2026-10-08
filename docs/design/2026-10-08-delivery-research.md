# How coding agents deliver their work: research (2026-10-08)

On 2026-10-08 a research agent looked at how hosted and self-hosted
coding agents get their work out of the environment they run in, ahead
of the [work delivery](../adr/2026-10-08-work-delivery.md) decision.
The facts below are the research agent's, with the sources it named;
they were not checked again for this note. Claims the research agent
drew from secondary sources carry **UNVERIFIED:**, as does the one
unsourced claim, on T3 Code.

## Common pattern

Across the eight agents checked:

- all run in a sandbox or VM;
- all push a prefixed feature branch and open a pull request;
- none pushes the default branch; GitHub Copilot and Claude Code's
  GitHub action block it outright;
- five of eight authenticate with a GitHub App or a service identity
  rather than the user's credentials;
- none documents what happens to partial work on interruption.

## Per agent

### GitHub Copilot coding agent

- Runs in an ephemeral GitHub-hosted sandbox.
- Branches are named `copilot/<issue>`, later semantic names such as
  `copilot/add-theme-switcher`.
- Authenticates with a GitHub App token.
- Cannot push outside `copilot/` branches.

Sources: github.blog changelog, 2025-10-16, "copilot coding agent uses
better branch names and pull request titles"; docs.github.com, "about
cloud and local sandboxes".

### Cursor cloud agents

- Run in an isolated VM or a self-hosted runtime.
- Branches carry the prefix `cursor/`, which can be customised but not
  removed.
- Push through the GitHub App only; the user's ssh keys and credential
  helpers are not available to the agent.

Sources: cursor.com/docs/cloud-agent/security;
cursor.com/help/ai-features/background-agents.

### Claude Code GitHub Actions

- Runs on the repository's Actions runner.
- Branches are named like `claude/issue-1-<timestamp>`.
- Authenticates with a GitHub App token, with Anthropic credentials held
  as secrets.
- Explicitly refuses `git push origin main`.
- Has known issues with auto-stash and orphaned worktrees on
  interruption.

Sources: code.claude.com/docs/en/github-actions;
github.com/anthropics/claude-code-action.

### Google Jules

- Runs in a Google Cloud VM, pushes a branch and opens a pull request.
- Google holds the credentials.

Source: blog.google/technology/google-labs/jules/.

### OpenAI Codex cloud agent

- Runs in an isolated VM, pushes a branch and opens a pull request.
- A GitHub installation token is passed at clone.
- **UNVERIFIED:** a command injection through branch names, from
  December 2025 to February 2026, exposed that token (secondary source:
  a zenml.io write-up).

### Devin

- **UNVERIFIED:** runs on cloud devboxes or self-hosted; authenticates
  as a service account with ssh keys or personal access tokens; uses a
  branch per task and opens a pull request (secondary source: fast.io).

### Grok Build (xAI)

- Runs in a local container or a remote runtime.
- Commits and pushes a branch, and opens a pull request.
- Authenticates with GitHub OAuth through Composio.

Source: github.com/xai-org/grok-build.

### HumanLayer

- **UNVERIFIED:** runs as a local or cloud daemon; documents an
  `agent/*` branch namespace; the user brings their own credentials
  (secondary sources: jimmysong.io; the ycombinator.com listing).

### T3 Code

- **UNVERIFIED:** an orchestration UI over other agents, not itself an
  agent; it delegates delivery to them (no source recorded).

## What we took from it

A task's work goes on a branch of its own under a fixed prefix,
`orchestrator/`, and the daemon refuses to push the default branch;
none of the agents checked pushes it either. We stop at pushing the
branch: opening a pull request needs each hosting service's API. Unlike
the five agents that push with a GitHub App or a service identity, the
daemon pushes with the credentials of the OS user that runs it, such as
its ssh key, until provisioning them is decided. None of the agents
documents what becomes of partial work on interruption, so the research
gave no model to follow there. The daemon pushes at the end of every
turn and before it deletes a workspace, so an interruption loses only
what was not yet pushed.
