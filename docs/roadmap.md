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
- VPS provisioning and daemon enrolment (offline-tested; the live
  Hetzner lifecycle test awaits a token), records
  [docs/adr/2026-10-10-vps-provisioning.md](adr/2026-10-10-vps-provisioning.md),
  [docs/adr/2026-10-10-daemon-enrolment.md](adr/2026-10-10-daemon-enrolment.md).
- SQLite backups, locally and to S3-compatible object storage, with
  restore (`VACUUM INTO` plus a stdlib SigV4 PUT; the live round trip
  against a real bucket awaits credentials):
  [docs/adr/2026-10-10-sqlite-backups.md](adr/2026-10-10-sqlite-backups.md),
  amended by
  [docs/adr/2026-10-10-server-loss.md](adr/2026-10-10-server-loss.md):
  gzipped uploads, backups after a harness exit and at shutdown, restore
  at start when the database is missing, and a chain of epochs that
  command ids name, so that daemons tell a restored server's commands
  from those they applied.
- Forge host keys distributed by the server (owner decision 2026-10-10,
  trust on first use rejected): the owner's keys as a setting plus
  GitHub's from its meta API, sent to each daemon as a `host_keys`
  command and written to `<state-dir>/known_hosts`; [README.md](../README.md),
  "Forge host keys". Record:
  [docs/adr/2026-10-10-forge-host-keys.md](adr/2026-10-10-forge-host-keys.md).
- Daemon binaries served by the server (`-daemon-binaries-dir`, owner
  decision 2026-10-10): [README.md](../README.md), "Provisioning a VPS".

## Decided, not yet built

- The live Hetzner lifecycle test (`go test -tags live -run Live
  ./internal/hetzner/`), once a token exists.
- Harnesses per daemon: the daemon as the host and each harness an
  entity it hosts (owner direction 2026-10-10, no decision yet). Note:
  [docs/design/2026-10-10-harnesses-per-daemon.md](design/2026-10-10-harnesses-per-daemon.md).
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

the move of the server to a VPS (README: Running the server on a VPS),
then the live Hetzner and bucket tests; recurring work -> project memory
-> credentials when needed.
