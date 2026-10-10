# Open items

Living document; revise in place. Decisions live in docs/adr, findings in docs/design.

Known gaps and UNVERIFIED behaviours, from the workers' reports, grouped by area.

## Harness

- Claude Code `Agent` calls never reach the permission tool; remote
  subagents are denied through an `ask` rule on
  `Agent(isolation:remote)`/`Task(isolation:remote)`.
- A repository's own `.claude/agents/` definition with `isolation:
  remote` would bypass it. **UNVERIFIED** that the frontmatter accepts
  it.
- `RemoteTrigger` tool unexamined.
- Whether the ask rule applies to a nested subagent's call is
  **UNVERIFIED**.
- The tool-class refusal interrupts a model call already in flight; a
  tool call landing first is possible.
- A restricted parent may spawn an unrestricted agent (by design, not
  narrowed).
- An owner interrupt may not stop a background subagent, leaving the
  turn open. **UNVERIFIED**.
- A prompt sent while a background subagent runs is **UNVERIFIED**.
- `--tools ""` leaves no built-in tools.

## Two-user mode

- The system prompt file is 0644 in a 0711 `/tmp` directory (path
  visible in `ps`).
- A crash can leave it.
- `secure_path` replaces `PATH` unless the sudoers exemption is added.
- `rg` is absent in the container image.
- `Kill` is `SIGTERM`; a harness ignoring it is waited on.
- macOS forms of the checklist and the interactive `/login` step stay
  manual.

## Delivery

- Workspaces cloned before the mirror change have no repository record
  (unpushed commits reported, kept for manual recovery).
- A daemon serving several GitHub repositories needs its key on an
  account, not as a deploy key.
- A moved task needs the new daemon's key registered.

## Tasks

- Tasks ended before the follow-ups change cannot be followed up.
- Messages dropped at a stop are delivered if the task is resumed and
  finishes.
- A held prompt is written from the output-reading goroutine (very large
  prompts untested).
- Grandchildren changes do not refresh the root's tree until the root or
  a child changes.
- Project instructions are copied into the prompt at start.
- Starts queued under an older server lack the workspace paragraph.

## Scheduling

- Slots rule adds running harnesses back (reported number rises by about
  one while a light harness runs).
- `macOS` memory estimate from `vm_stat` is **UNVERIFIED** against
  Linux `MemAvailable`.
- Quota between readings cannot see use outside the orchestrator.
- Cost not backfilled for moves before schema 14.

## Server

- Gateway unauthenticated on loopback and the gateway URL visible in
  `ps`.
- The `ssh_public_key` fact widens the dashboard's label column.
- `parent_id` kept as the field name.
- Flaky test noted at `internal/server/gui_schedule_internal_test.go:114`.

## Daemon

- A daemon state leak if it dies during a stop.
- Placement can stall when every connected daemon is one the moved task
  ran on.
- `requires` matches `memory`/`cpus` exactly.

## Docs

- Several accepted records are amended by later ones rather than
  superseded (task lifetime, shutdown recovery, daemon loss, harness
  integration, agents and placement, scheduling).
- The harness-user record still says the checklist has not been run.
- The work-delivery record says `branch_pushed` reports each push while
  it also reports uncommitted-only turns.
