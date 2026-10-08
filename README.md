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

## Running

The server stores daemons, tasks and their events, and serves the GUI.
The daemon runs the tasks with Claude Code, so `claude` must be
installed and logged in for the user who starts the daemon. Build both
from the repository root:

    nix develop -c go build ./cmd/server
    nix develop -c go build ./cmd/daemon

The paths below are examples; use any writable location.

### On a server reachable from the internet

The server serves TLS. Daemons authenticate with client certificates
from the server's own certificate authority, and the owner with a token
([daemon authentication](docs/adr/2026-10-08-daemon-authentication.md),
[owner authentication](docs/adr/2026-10-08-owner-authentication.md)).
On the server's machine,
create the CA, the server's certificate for every name and address that
daemons and browsers reach it by, and the owner token:

    ./server init-ca -pki-dir ~/.local/state/orchestrator/pki
    ./server issue-server-cert -pki-dir ~/.local/state/orchestrator/pki \
        -host orchestrator.example -host 203.0.113.7
    ./server issue-owner-token -db ~/.local/state/orchestrator/server.db

`issue-owner-token` prints the token once; keep it somewhere
retrievable, such as a password manager. Running it again replaces the
token and logs out every browser. Start the server:

    ./server -listen :8443 -db ~/.local/state/orchestrator/server.db \
        -tls-cert ~/.local/state/orchestrator/pki/server.crt \
        -tls-key ~/.local/state/orchestrator/pki/server.key \
        -client-ca ~/.local/state/orchestrator/pki/ca.crt

For each daemon, issue a certificate named after the daemon's id:

    ./server issue-daemon-cert -pki-dir ~/.local/state/orchestrator/pki -id vps-1

It writes `daemon.crt`, `daemon.key` and `ca.crt` to
`daemons/vps-1/` under the `-pki-dir`. Copy the three files and a
`daemon` binary built for that machine to one directory on it, with
`daemon.key` readable only by the user who runs the daemon, and start
the daemon from that directory:

    ./daemon -server https://orchestrator.example:8443 \
        -cert daemon.crt -key daemon.key -ca ca.crt \
        -state-dir ~/.local/state/orchestrator/daemon

Open https://orchestrator.example:8443/ and log in with the owner
token. The browser warns about the server's certificate until it trusts
`ca.crt`. Scripts call the owner API under `/v1/tasks` with the header
`Authorization: Bearer <token>`.

Whoever holds `ca.key` can issue any daemon's certificate, and a server
certificate that the daemons trust. The running server does not read
it, so it can be kept elsewhere between issuing certificates.

Server and daemon certificates last a year, the CA ten. To renew a
certificate, delete its `.crt` and `.key`, issue it again, and restart
the program that uses it, after copying a daemon's new files to its
machine. The server does not check revocation. To shut out a daemon
before its certificate expires, run `init-ca` with a new `-pki-dir`,
issue the server's and every remaining daemon's certificate from it,
restart the server and those daemons with the new files, and have
browsers trust the new `ca.crt`.

### On one machine, for development

Run the server and the daemon in separate terminals:

    ./server init-ca -pki-dir ~/.local/state/orchestrator/pki
    ./server issue-daemon-cert -pki-dir ~/.local/state/orchestrator/pki -id laptop
    ./server -insecure-loopback -db ~/.local/state/orchestrator/server.db
    ./daemon -server http://127.0.0.1:8080 \
        -cert ~/.local/state/orchestrator/pki/daemons/laptop/daemon.crt \
        -key ~/.local/state/orchestrator/pki/daemons/laptop/daemon.key \
        -state-dir ~/.local/state/orchestrator/daemon

Then open http://127.0.0.1:8080/. With `-insecure-loopback` the server
serves plain HTTP and authenticates nobody, so anyone who can reach it
can start tasks that run commands on the daemon's machine. It refuses to
start unless `-listen` is a loopback IP address. The daemon still needs
its certificate, which names it, but not `-ca`.

### Using the GUI

The dashboard lists the tasks that need attention, every task, and the
connected daemons, and has the form that starts a task: its prompt, an
optional repository and ref, the model, the daemon to run it on, and its
pause limits. Each task page shows the transcript, asks for permission
when the agent wants to run a tool, and has buttons to pause, resume,
interrupt or stop the task.

### Flags

Server flags:

- `-db` (required): the SQLite database file, created with its directory
  when missing. It holds every daemon, task, event and command, the
  owner token's hash and the login sessions.
- `-listen`: the address to serve on, `127.0.0.1:8080` by default.
- `-tls-cert`, `-tls-key` (required unless `-insecure-loopback`): the
  server's certificate and key, from `issue-server-cert`.
- `-client-ca` (required unless `-insecure-loopback`): the CA
  certificate, `ca.crt` from `init-ca`, that daemons' certificates are
  verified against.
- `-insecure-loopback`: serve plain HTTP without authentication; only
  with a loopback IP address as `-listen`, and without `-tls-cert`,
  `-tls-key` and `-client-ca`.
- `-default-model`: the model of a task started without one, `haiku`
  by default.

Without `-insecure-loopback`, the server also refuses to start until an
owner token has been issued into its database.

Server subcommands, each printing its usage with `-h`:

- `init-ca -pki-dir DIR`: create the CA as `ca.crt` and `ca.key`.
- `issue-server-cert -pki-dir DIR -host NAME_OR_IP...`: issue
  `server.crt` and `server.key`, valid for every `-host`.
- `issue-daemon-cert -pki-dir DIR -id DAEMON`: issue a daemon's
  certificate into `daemons/DAEMON/`, with a copy of `ca.crt`. The id is
  up to 128 letters, digits, `.`, `_` and `-`, other than `.` or `..`;
  once the daemon has connected, the new-task form offers it by this id.
- `issue-owner-token -db FILE`: print a new owner token and keep its
  hash in place of the previous token's.

The first three refuse to replace an existing certificate or key.

Daemon flags:

- `-server` (required): the server's base URL. Plain `http://` is
  accepted only for a loopback IP address.
- `-cert`, `-key` (required): the daemon's certificate and key from
  `issue-daemon-cert`. The certificate's name is the daemon's id.
- `-ca` (required for `https://`): the CA certificate to verify the
  server with.
- `-state-dir` (required): created when missing. It holds `state.json`,
  which records what each task needs to be resumed, one journal per task
  under `journal/`, and each task's working directory under
  `workspaces/<task>/`. Claude Code keeps the sessions themselves under
  the OS user's `~/.claude/projects/`.
- `-claude`: the `claude` executable, `claude` on `PATH` by default.
  The daemon runs `claude --version` when it starts and exits if that
  fails. Tasks use the Claude Code login of the OS user who starts the
  daemon and spend that account's quota.

### Tasks

Without a repository, a task starts in an empty directory. With one, it
starts in a clone checked out at the ref. The repository must be a
public one given by its `https://` URL; the daemon refuses any other.
git clones without the user's or the system's git configuration, so its
credential helpers and URL rewrites are not used.

Each turn of a task runs in its own `claude` process, which exits when
the turn ends. The task is then `finished`, or `paused` if it was paused
during the turn. A follow-up prompt, or Resume on a paused task, starts
a new process that continues the same Claude Code session in the same
working directory. A task ends for good as `stopped` when stopped from
its page, or as `failed`; the task page then takes no more prompts.

SIGINT or SIGTERM shuts either program down. The daemon stops its
running turns first; a second signal makes it exit at once. A daemon
started again with the same `-state-dir` marks as `failed` every task
whose turn its previous run did not end; a task that was between turns
can still be resumed.

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
