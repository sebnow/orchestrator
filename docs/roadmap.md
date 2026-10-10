# Roadmap

Living document; revise in place. Decisions live in docs/adr, findings in docs/design.

## Built and recorded

- Client protocol and transcripts:
  [docs/adr/2026-10-07-client-protocol.md](adr/2026-10-07-client-protocol.md),
  [docs/adr/2026-10-07-transcripts.md](adr/2026-10-07-transcripts.md).
- Scheduling with priorities, filler and account budget:
  [docs/adr/2026-10-08-scheduling.md](adr/2026-10-08-scheduling.md).
- Shutdown recovery and orphan reaping:
  [docs/adr/2026-10-08-shutdown-recovery.md](adr/2026-10-08-shutdown-recovery.md)
  (supersedes
  [2026-10-08-restart-recovery.md](adr/2026-10-08-restart-recovery.md)).
- Daemon loss and task moves:
  [docs/adr/2026-10-08-daemon-loss.md](adr/2026-10-08-daemon-loss.md).
- Permission policy (allow-all for now, remote subagents denied under
  every policy):
  [docs/adr/2026-10-08-permission-policy.md](adr/2026-10-08-permission-policy.md).
- Work delivery by pushed branch:
  [docs/adr/2026-10-08-work-delivery.md](adr/2026-10-08-work-delivery.md).
- Harness user split (two-user mode, container-verified with stub and
  real binary):
  [docs/adr/2026-10-08-harness-user.md](adr/2026-10-08-harness-user.md),
  [docs/design/2026-10-09-harness-user-container.md](design/2026-10-09-harness-user-container.md),
  [docs/design/2026-10-09-harness-user-real-claude.md](design/2026-10-09-harness-user-real-claude.md).
- Agents and placement with labels and facts:
  [docs/adr/2026-10-09-agents-and-placement.md](adr/2026-10-09-agents-and-placement.md).
- Hand-back and native subagents (background subagents hold the turn):
  [docs/adr/2026-10-09-agents-and-placement.md](adr/2026-10-09-agents-and-placement.md)
  (hand-back),
  [docs/design/2026-10-09-background-subagent-turn.md](design/2026-10-09-background-subagent-turn.md)
  (turn-holding).
- State-directory lock:
  [docs/adr/2026-10-10-state-directory-lock.md](adr/2026-10-10-state-directory-lock.md),
  [README.md](../README.md), daemon flags, `-state-dir`.
- Work stays on the machine:
  [docs/adr/2026-10-09-work-stays-on-the-machine.md](adr/2026-10-09-work-stays-on-the-machine.md).
- Projects and lineage (purpose, tree):
  [docs/adr/2026-10-10-projects-and-lineage.md](adr/2026-10-10-projects-and-lineage.md).
- Daemon push identity and mirror:
  [docs/adr/2026-10-10-daemon-push-identity.md](adr/2026-10-10-daemon-push-identity.md).
- Task follow-ups and steering (resumable states, queued/now, context
  size, continue in a new task):
  [docs/adr/2026-10-10-task-follow-ups-and-steering.md](adr/2026-10-10-task-follow-ups-and-steering.md).
- Agent model lists, effort, tool classes, daemon-reported capacity:
  [docs/adr/2026-10-10-agent-models-and-capacity.md](adr/2026-10-10-agent-models-and-capacity.md).
- Harness login through the server (record and implementation,
  2026-10-10):
  [docs/adr/2026-10-10-harness-login.md](adr/2026-10-10-harness-login.md).

## Decided, not yet built

- Hetzner Cloud as the VPS provider, with server-driven provisioning and
  daemon enrolment. Record:
  [docs/adr/2026-10-10-vps-provisioning.md](adr/2026-10-10-vps-provisioning.md).
  Source: survey
  [docs/design/2026-10-10-vps-providers.md](design/2026-10-10-vps-providers.md),
  follow-up note
  [docs/design/2026-10-10-vps-providers-followup.md](design/2026-10-10-vps-providers-followup.md).
- SQLite backups to S3-compatible object storage, in-process (`VACUUM
  INTO` plus a stdlib SigV4 PUT), no external tool. Record:
  [docs/adr/2026-10-10-sqlite-backups.md](adr/2026-10-10-sqlite-backups.md).
- Recurring work: owner-defined schedules (bug triage, refactoring,
  performance) attached to a project; mechanism to be designed. Source:
  named as deferred in
  [docs/adr/2026-10-10-projects-and-lineage.md](adr/2026-10-10-projects-and-lineage.md).
- Project memory and shared files, a standing coordinator per project.
  Source: deferred in
  [docs/adr/2026-10-10-projects-and-lineage.md](adr/2026-10-10-projects-and-lineage.md);
  shape sketched 2026-10-10 (owner decision 2026-10-10, in
  docs/roadmap.md only): a bounded, owner-editable memory document agents
  can append to through a gateway tool, injected into every task's
  prompt; hand-back reports searchable through a tool; compaction as a
  job on the document.
- Credentials for tasks beyond git: deferred until a task needs one;
  preference order decided 2026-10-10 (owner decision 2026-10-10, in
  docs/roadmap.md only): minted short-lived scoped tokens (GitHub App,
  cloud STS) first, a tool runner as a third OS user for static secrets
  second, a TLS-intercepting proxy with a daemon CA only if those leave a
  tool uncovered; egress confinement a separate later decision; GPG
  signing via a gpg-agent relay deferred. Survey:
  [docs/design/2026-10-09-credential-substitution.md](design/2026-10-09-credential-substitution.md).
- Forge registration of daemon keys by the server (deploy key or machine
  user through the forge API): deferred in the push-identity record,
  [docs/adr/2026-10-10-daemon-push-identity.md](adr/2026-10-10-daemon-push-identity.md);
  today the owner registers the daemon's public key on an account.
- Nostr relay direction: deferred, owner decision 2026-10-09. Source:
  [docs/design/2026-10-09-nostr-direction.md](design/2026-10-09-nostr-direction.md).
- Transcript Markdown rendering and GUI aesthetics: deferred, owner wants
  an aesthetics pass later (owner decision 2026-10-10, in docs/roadmap.md
  only).
- Owner API task tree fields: built with
  [docs/adr/2026-10-10-projects-and-lineage.md](adr/2026-10-10-projects-and-lineage.md);
  nothing further.
- Per-task OS users: possible later step after the two-user mode,
  [docs/adr/2026-10-08-harness-user.md](adr/2026-10-08-harness-user.md)
  (owner decision 2026-10-10, in docs/roadmap.md only).

## Order as of 2026-10-10

Hetzner provisioning with enrolment ->
backups -> recurring work -> project memory -> credentials when needed.
