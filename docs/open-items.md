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
- Login events travel on a daemon route of their own, unsequenced and
  unjournaled, which the client-protocol record does not foresee (a
  lost event costs one new login).
- The `account` fact can be absent until `claude` has run once.
  **UNVERIFIED** whether `claude auth login` sets it at once.
- The real-claude container pass's daemon log contains the owner's
  account email.
- The budget key assumes Team-plan limits are per seat. **UNVERIFIED**.
- The browser authorisation and the success path of the login are
  untested against the real binary.

## Daemon

- A daemon state leak if it dies during a stop.
- Placement can stall when every connected daemon is one the moved task
  ran on.
- `requires` matches `memory`/`cpus` exactly.

## Provisioning

- A daemon on a fresh VPS gets the forges' host keys from the server at
  connect; the full VPS path (provision, enrol, push to GitHub) has not
  run live. **UNVERIFIED**.
- Serving the daemon binaries from the server (`-daemon-binaries-dir`)
  is tested over TLS in Go only; a VPS's `curl --cacert` download
  against it has not run. **UNVERIFIED**.
- The live Hetzner lifecycle test has not run: the image name
  `debian-13`, `cx23` availability in `fsn1`, cloud-init ordering,
  `ssh_pwauth`, and metadata exposure of user data are all
  **UNVERIFIED**.
- A daemon that crashes between the server signing its certificate and
  the daemon writing it is stuck until the owner issues a new token.
- Destroying a VPS straight after creating it may hit Hetzner's 423.
  **UNVERIFIED**.
- The enrolment token is world-readable in the systemd unit file until
  spent.
- How Hetzner bills, per minute or per started hour, is **UNVERIFIED**.

## Host keys

- A daemon connected before the server learnt of host keys, with a
  `start_task` issued before its first `host_keys` command, may fetch
  before it has a `known_hosts`; the race exists only on the first
  connect after an upgrade, as later connects keep the file from the
  previous one.
- A daemon older than the `host_keys` command skips it with a warning;
  once upgraded it gets the keys at its next connect.
- The daemon's ssh still reads the system's
  `/etc/ssh/ssh_known_hosts` (not overridden).
- GitHub's keys come from `api.github.com/meta` over the system's TLS
  roots; the server does not pin them, and GitHub rotating its keys
  reaches daemons within 24 hours (or an hour after a failed fetch).

## Backups

- Path-style request signing against Hetzner Object Storage is
  **UNVERIFIED** until the live round trip runs.
- A backup taken during a daemon login may hold that login's code; the
  server blanks codes in its command log only once the login ends.
- `restore -force` against a running server would lose data: the
  command does not check that the server is stopped.
- The dashboard prints the backup interval in Go's duration form, such
  as `6h0m0s`.
- A copy's name holds its time to the second, so two backups in one
  second, as a burst of harness exits can make, share a name, and the
  later replaces the earlier locally and in the bucket.
- Restore at start, gzipped uploads and the R2 bucket are tested only
  against the in-process fake bucket until the live round trip runs.
- A daemon refused with `unknown_lineage` needs its `state.json` edited
  by hand; the server offers no reset.

## Docs

- Several accepted records are amended by later ones rather than
  superseded (task lifetime, shutdown recovery, daemon loss, harness
  integration, agents and placement, scheduling).
- The harness-user record still says the checklist has not been run.
- The work-delivery record says `branch_pushed` reports each push while
  it also reports uncommitted-only turns.
