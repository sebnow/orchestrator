# Working conventions

Living document; revise in place. Decisions live in docs/adr, findings in docs/design.

Standing rules from the working sessions of 2026-10-07 to 2026-10-10.

## Records

- Every decision that changes behaviour gets an ADR under `docs/adr/` with
  YAML frontmatter `status`, `date`, `source`.
- An accepted ADR is immutable: there are no amendments, only superseding
  records, carrying `supersedes:`/`superseded-by:` in frontmatter. A record
  may amend one point of another, but only by saying so explicitly.
- "Going with something for now" is a deferral, not an ADR.
- Design notes under `docs/design/` are dated and never revised; a new
  finding gets a new note.
- `**UNVERIFIED:**` markers are allowed in design notes and the README's
  findings, never in an ADR's Decision — unverified claims there are
  either verified or removed.

## Protocol

- Anything added to the daemon-server protocol must earn its keep: prefer
  carrying new meaning in existing events and commands.

## Dependencies

- No Go dependencies beyond `modernc.org/sqlite`,
  `github.com/modelcontextprotocol/go-sdk` and the vendored HTMX; stdlib
  otherwise.

## Spend

- The Claude subscription is the only spend. Live tests use haiku, short
  prompts; no API keys.

## Verification

- A worker's report shows the command and its output; "tests should pass"
  counts as not run.
- `go build ./... && go test -count=1 ./... && gofmt -l .` clean before a
  report.
- Run the container checks (`test/harness-user/run.sh`, `--real-claude`)
  when harness-user mode or delivery is touched.
- Run the live tests (`-tags live`) when harness behaviour is touched.

## Version control

- Jujutsu, small commits with messages in the form `scope: imperative
  description` (lowercase, no type prefix).
- Plan the commit sequence before multi-commit work.

## Security stance

- Work stays on the daemon's machine
  (ADR 2026-10-09-work-stays-on-the-machine).
- Credentials are held by the daemon outside the agent's process; the
  server orchestrates credentials and provisioning.
- Nothing is done by hand on a client machine; the server GUI is the only
  interface.
- A public key may cross the wire; anything secret only over mTLS, and
  only when unavoidable.

## Owner interaction

- Batch questions; number them with running identifiers (Q1, Q2, ...),
  never reused across turns.
- Lead with what needs the owner.
- Recommendation after the question, starting "Recommend:".
- Plain words, not ids.
- Don't present inferred content as fact.

## Harness

- The unmodified Claude Code binary, driven over stream-json.
- Harness-specific names stay inside `internal/harness/claude`.
- The orchestrator composes every prompt paragraph that describes its own
  mechanics.
- Agent definitions are the owner's and live in the server, not the
  repository.

## macOS

- macOS is a client for attaching a local Claude instance in single-user
  mode with the owner's environment; it is never a VPS target.
