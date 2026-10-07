# Orchestrator

Runs Claude Code agents unattended, with one server to command them,
watch them live, and keep what they produce. The design is in
`docs/design/2026-10-06-brainstorm.md`; the decisions are in `docs/adr/`.

## Building

The Nix flake dev shell provides the Go toolchain, so Go need not be
installed on the host:

    nix develop -c go build ./...
    nix develop -c go vet ./...

Module path: `github.com/sebnow/orchestrator`. Binaries live under `cmd/`
(`cmd/server`, `cmd/daemon`). Create a package only when it has code.

`nix develop -c go test ./...` runs offline. The live tests, behind the
`live` build tag, run the real `claude` on `PATH` under the owner's login
and spend subscription quota:

    nix develop -c go test -tags live ./...

## Version control

The repository uses [jujutsu](https://jj-vcs.github.io/). Commit messages
follow [Scoped Commits](https://scopedcommits.com/):

    <scope>: <description>

- `scope` is the area touched, written as a path or file name: `cmd`,
  `flake`, `go.mod`, `docs/adr`, `docs/design`, `spikes/mod-vs-stdout`.
  A change touching several areas uses a more general scope, or scopes
  separated by commas.
- `description` is lowercase and imperative: `add`, `record`, `require`.
- No type prefix (`feat:`, `fix:`) and no trailing period.
- The body says why; the diff says what.

One logical change per commit. Mechanical changes (renames, moves,
reformatting) are committed apart from behaviour changes.

## Documents

Documents come in three kinds, each under its own directory:

- `docs/design/<date>-<topic>.md`: notes, brainstorms and spike findings,
  dated. A note records what was known on its date and is not revised
  afterwards; later knowledge goes in a new note or an ADR.
- `docs/adr/<date>-<need>.md`: architecture decision records, named after
  the need (`server-storage`), not the solution (`use-sqlite`). Header
  lines: `Status`, `Decided`, `Source`, and `Amended by` when a later
  record supersedes or amends it. Sections: Context, Decision, Consequences,
  and at most one closing `Revisit if` line. Do not edit an accepted
  record beyond adding an `Amended by` pointer; a change of mind is a new
  record.
- `spikes/<name>/`: throwaway experiments with a README saying how to
  rerun them. A spike that commits logs carries a redaction script
  (`spikes/mod-vs-stdout/redact.py` is the first) and runs it before any
  log is committed; it removes e-mail addresses, account and organisation
  ids, device ids, and credential handles (`auth_...` values).

In every document, a claim that was not checked against a primary source
or an experiment is prefixed `**UNVERIFIED:**`. A summary of someone
else's findings keeps their markers.
