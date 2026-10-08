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
installed and logged in for the user who starts the daemon. The daemon
clones a task's repository with the `git` on the daemon's `PATH`, so
`git` must be installed on every daemon's machine. A missing `git`
shows only when a task with a repository starts, and fails it. Build both
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

The dashboard lists the tasks that need attention, every task, the
account's quota reading and the daemons, and has the form that starts a
task: its prompt, an optional repository and ref, the model, the daemon
to run it on or any connected daemon, its priority, whether it is filler
(see [Scheduling](#scheduling)), and its pause limits. Each task page shows the transcript, asks for permission when
the agent wants to run a tool, and has buttons to pause, resume,
interrupt or stop the task.

A `stopped` or `failed` task's page, and a `failed` task among those
that need attention, have a Dismiss button. A dismissed task no longer
needs attention and is left out of the dashboard's list of every task,
which says how many dismissed tasks it leaves out; `/?dismissed=show`
includes them, marked as dismissed. Scripts dismiss a task with
`POST /v1/tasks/{task}/dismiss`, which answers 409 unless the task is
`stopped` or `failed`.

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
- `-slots-per-daemon`: how many tasks each daemon runs at once, 2 by
  default.
- `-filler-threshold`: the utilization of the account's five-hour quota
  window, from 0 to 1, below which filler tasks run; 0.5 by default
  (see [Scheduling](#scheduling)).
- `-low-threshold`: the same for low-priority tasks; 0.85 by default.

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
  `workspaces/<task>/`. Claude Code keeps its sessions outside it:
  Claude Code 2.1.289 on macOS kept them under `~/.claude/projects/` of
  the user running it ([resume spike](docs/design/2026-10-08-resume-spike.md)).
  **UNVERIFIED:** that other versions and platforms keep them there.
- `-claude`: the `claude` executable, `claude` on `PATH` by default.
  The daemon runs `claude --version` when it starts and exits if that
  fails. Tasks use the Claude Code login of the OS user who starts the
  daemon and spend that account's quota.

### Tasks

Without a repository, a task starts in an empty directory. With one, it
starts in a clone checked out at the ref. The daemon accepts only a
repository given as an `https://` URL with a host, so an ssh URL or a
local path fails the task, as does a ref that starts with `-`. `git`
runs with credential prompts off and ignores the user's and the
system's git configuration, credential helpers and URL rewrites
included, so a repository that needs credentials fails to clone and
the task fails.

Each turn of a task runs in its own `claude` process, which exits when
the turn ends. The task is then `finished`, `paused` if the owner paused
it during the turn, or `yielded` if the scheduler did. A follow-up
prompt, or Resume on a paused or yielded task, starts a new process that continues the same Claude Code session in the same
working directory. A task ends for good as `stopped` when stopped from
its page, or as `failed`; the task page then takes no more prompts.

An agent can start child tasks and message other tasks with two tools
the daemon gives it, `spawn_task` and `send_message`
([inbox delivery](docs/adr/2026-10-08-inbox-delivery.md)). The server
explains them in a system prompt it gives every task, ahead of any
system prompt given through the owner API. A child runs on its parent's
daemon, in a fresh clone of the parent's repository if it has one,
with the parent's priority and filler flag, and the parent's model
unless the agent names another, and is told to send its result to its
parent. When the parent's daemon has no free slot, the child goes to the
connected daemon with the most free slots instead. A message to a
`finished` task becomes its next turn; one to a running task waits until
its turn ends, and one to a `paused` task until the owner resumes it and
that turn ends. Messages to `stopped` or `failed` tasks are refused, and
a parent is told when its child stops or fails. When a task stops or
fails with messages still waiting in its inbox, each sender's next
prompt is a notice that those messages were not delivered. The
task page links a task's parent and children and shows the messages it
sent and received.

SIGINT or SIGTERM shuts either program down. The daemon first
interrupts each running turn and closes the input of the task's
`claude` process. A task whose process then exits with code 0 is
`finished`, even though its turn was interrupted; a task whose process
exits otherwise, or is killed for not exiting within 30 seconds of the
interrupt, is `failed`. The daemon then sends the server the events it
has not sent yet, for up to 30 more seconds. A second signal makes it
exit at once.

A daemon started again with the same `-state-dir` finds the tasks that
were in the middle of a turn when its previous run ended, by a crash, a
kill, or a second signal
([restart recovery](docs/adr/2026-10-08-restart-recovery.md)). Each
such task becomes `paused`, and its page and the dashboard's tasks that
need attention say that the daemon restarted. Resume continues the
task's Claude Code session and tells the agent that its last turn was
cut short; when the process died before Claude Code reported a session,
Resume starts a new session with the task's first prompt instead. A
task whose workspace was still being cloned becomes `failed`. A task
that was between turns keeps its state.

### Scheduling

A task's start, a follow-up prompt, a resume and a message delivery
each become a turn in a single queue shared by all daemons, and a
scheduler decides when each one starts
([scheduling](docs/adr/2026-10-08-scheduling.md)). `POST /v1/tasks`
answers 201 with the queued start; a prompt or resume posted to
`/v1/tasks/{task}/commands` answers 202 with the queued turn. Pause,
interrupt, stop and permission answers are sent to the daemon at once,
without queueing.

- **Slots.** Each daemon runs at most `-slots-per-daemon` tasks at once.
  A task holds a slot from the admission of its turn until its process
  exits. A task started without a daemon goes to the connected daemon
  with the most free slots, and stays on that daemon, where its
  workspace is. Turns for a daemon that is not connected wait until it
  reconnects; a task bound to a daemon that never returns stays queued
  until the owner stops it.
- **Priority.** A task is `low`, `normal` or `high`, `normal` by
  default. Waiting turns are admitted highest priority first, oldest
  first within a priority. A turn that cannot run yet does not hold up
  those behind it.
- **Filler.** A filler task runs only when other work leaves slots and
  budget spare: its turns wait while any non-filler turn waits for a
  slot. When a non-filler turn waits for a slot on a daemon running
  filler, the scheduler pauses the filler task there whose current turn
  started last. That task shows as `yielded`, and the scheduler queues
  its resume as a filler turn. An owner's resume of a `yielded` task is
  a non-filler turn.
- **Budget.** Daemons report the account's quota, a status and each
  window's utilization, from an undocumented Claude Code event seen once
  per process, so the newest reading from any daemon is only as fresh
  as the newest turn. Filler runs only while the five-hour window's
  utilization is below `-filler-threshold`, and low priority below
  `-low-threshold`. With no reading, or once the reading's five-hour
  window has reset, filler waits and everything else runs. A reading
  with status `rejected` holds every turn until the earliest of its
  windows resets. The dashboard shows the reading and its age.

A task whose turn waits shows a `queued` badge with its place in the
queue and the reason it waits.

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
