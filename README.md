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

The browser tests, behind the `browser` build tag, load the GUI in a
headless browser and check what htmx does with it. They run offline
against an in-process server and a fake daemon:

    nix develop -c go test -tags browser ./internal/server/

They use `$ORCHESTRATOR_BROWSER` when it is set, or else the first of
`chromium`, `google-chrome-stable` and `google-chrome` on `PATH`. The
dev shell provides `chromium` on Linux and Google Chrome on macOS, as
`google-chrome-stable` and `google-chrome`, because nixpkgs builds
Chromium only for Linux. A failing step saves a screenshot, which
`go test` keeps when given `-artifacts`.

## Running

The server stores daemons, tasks and their events, and serves the GUI.
The daemon runs the tasks with Claude Code, so `claude` must be
installed and logged in for the user who runs it: the user who starts
the daemon, or the harness user with `-harness-user` (see [Running the
harness as another user](#running-the-harness-as-another-user)). The daemon
mirrors a task's repository, clones the task's workspace from the
mirror, and pushes the task's branch to the repository, with the `git`
on the daemon's `PATH`, so
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
`ca.crt`. Scripts call the owner API, `/v1/tasks`, `/v1/agents` and
`/v1/projects`, with the header `Authorization: Bearer <token>`.

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

### Running the harness as another user

By default the daemon runs `claude`, and with it the agent's tools, as
its own OS user, so the agent can read the daemon's key, its state and
its journals. With `-harness-user NAME` the daemon runs `claude`, and
every command that touches a workspace, as the unprivileged user NAME
through `sudo`, and file permissions keep the agent from reading the
daemon's key, state and journals
([harness user](docs/adr/2026-10-08-harness-user.md)). The agent can
still read its own Claude Code login, and every task's workspace on
that daemon, since all tasks share the one harness user. It can also
see in the process list the URL of the daemon's MCP gateway, which
serves every task's `spawn_task`, `send_message` and permission tools
on loopback without authentication. A task's system prompt is not on
the command line: the daemon writes it to a file that `claude` reads,
in a directory under `/tmp` that every user may enter but not list,
and deletes the file when `claude` exits. The process list shows the
file's path, so any local user who reads it can read the prompt while
the task runs.

The daemon starts each command as `sudo -n -u NAME -D DIR VAR=value...
-- COMMAND ARG...`, in a session of its own so that sudo has no
terminal. As the harness user it runs:

- `claude`, in the task's workspace;
- `git`, to clone the workspace from the daemon's mirror, to set up
  the task's branch, its pre-push hook and the clone's local
  configuration, to read the clone's status and history, and, as `git
  upload-pack`, to hand the task's branch to the daemon's fetch into
  the mirror;
- `rm -rf`, to delete a workspace.

The daemon does not run git in a workspace as itself, because git runs
commands that a repository's configuration and hooks name (`git help
git`, section SECURITY), and the agent can write every workspace. The
harness user creates and owns the workspaces.

The daemon runs every git command against a task's repository as its
own user, in its mirror of the repository and with its own key (see
[Tasks](#tasks)), so delivery needs no credential from the harness
user, and the harness user cannot read the daemon's key. If the daemon
has an ssh agent, the harness user can still use it through the relay
described below. With a harness user the files are laid out as
follows:

- `<state-dir>` has mode 0711: the harness user can pass through it to
  the mirrors by name, but cannot list it. The daemon's state
  (`state.json`), journals (`journal/`), private key (`ssh_ed25519`)
  and lock are readable by the daemon's user only; its public key,
  `ssh_ed25519.pub`, by every user.
- `<state-dir>/mirrors/<SHA-256 of the repository URL>.git` is the
  bare mirror of a repository, owned by the daemon's user, readable by
  every user and writable by the daemon's user only.
- `<state-dir>/workspace-repos/<task id>` records the repository a
  task's workspace was cloned for. The daemon pushes the task's branch
  to the repository recorded there rather than to one named in the
  workspace's configuration, which the agent can change.
- `<workspace-dir>/<task id>` is the task's workspace, a clone owned
  by the harness user with mode 0700, whose `origin` is the mirror.

The harness user can therefore read every mirror on the daemon, every
task's workspace and the daemon's public key, but not the daemon's
ssh key, state or journals. git refuses a repository that another
user owns unless `safe.directory` names it, and does not pass `-c`
settings on to the `upload-pack` it runs for a local repository, so
the workspace's `remote.origin.uploadpack` is `git -c
safe.directory=<mirror> upload-pack`. The agent's own `git fetch` from
`origin` works the same way; a push to `origin` fails, since the
harness user cannot write the mirror. The daemon fetches the task's
branch from the workspace with `git upload-pack` run as the harness
user through sudo, which the sudoers rule for `git` below allows.

`-workspace-dir` is required with `-harness-user`, because the harness
user cannot write in the state directory, where workspaces go by
default.
The directory must exist and be owned by the harness user. When it
starts, the daemon lists this directory and deletes the workspaces of
tasks it does not know, so the daemon's user needs permission to read
it. `-claude` must be an absolute path that the harness user can run;
the daemon runs `claude --version` as that user through sudo when it
starts, so a missing sudoers rule stops it there. When it starts, the
daemon also looks up `git` and `rm` on its `PATH`, logs their absolute
paths, and runs them by those paths.

The sudoers rule lets the daemon's user run those three paths as the
harness user only, without a password, and nothing as root. With the
daemon running as `orchestrator`, the harness user `orch-agent`, and
the paths below, write it with `sudo visudo -f
/etc/sudoers.d/orchestrator`:

    Cmnd_Alias ORCH_CLAUDE = /usr/local/bin/claude
    Cmnd_Alias ORCH_GIT = /usr/bin/git
    Cmnd_Alias ORCH_RM = /bin/rm
    Cmnd_Alias ORCH_HARNESS = ORCH_CLAUDE, ORCH_GIT, ORCH_RM
    Defaults!ORCH_HARNESS !requiretty, umask=0077
    Defaults!ORCH_CLAUDE env_keep += "PATH SSH_AUTH_SOCK"
    Defaults!ORCH_GIT env_keep += "PATH SSH_AUTH_SOCK GIT_TERMINAL_PROMPT GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM GIT_SSH_COMMAND"
    orchestrator ALL = (orch-agent) CWD=* NOPASSWD: ORCH_HARNESS

Replace the three paths with the value of `-claude` and the output of
`command -v git rm` run as the daemon's user. `CWD=*` lets the daemon
choose the directory with `-D`, since the daemon's user may not be able
to enter a workspace the harness user owns; it needs sudo 1.9.3 or
later (sudoers(5), "Chdir_Spec"). `umask=0077` makes the workspaces
readable by the harness user only: sudo runs the command with the union
of the daemon's umask and this one (sudoers(5), "umask"). sudo
1.9.17p2's `visudo -c` accepted these lines, and sudo 1.9.16p2 ran
them, with `rm` at `/usr/bin/rm`, in the container check below.

sudo resets the environment and sets `HOME`, `MAIL`, `SHELL`, `LOGNAME`
and `USER` for the harness user (sudoers(5), "Command environment"). The
daemon passes the rest as follows:

- `PATH`: the daemon's, in sudo's own environment. sudo keeps it
  because `env_keep` lists it, unless sudoers sets `secure_path`: sudo
  then replaces `PATH` with that value whatever `env_keep` lists
  (sudoers(5), "env_reset"). Debian's `/etc/sudoers` sets it, and in
  the container check below `claude` and its Bash tool got
  `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`
  rather than the daemon's `PATH`. The daemon does not change sudoers;
  decide when you deploy whether `claude` should get the daemon's
  `PATH`. To let it through to `claude` and the agent's tools, add this
  line to `/etc/sudoers.d/orchestrator`, with `/usr/local/bin/claude`
  replaced by the value of `-claude`:

      Defaults!/usr/local/bin/claude !secure_path

  This would be needed, for example, when `claude` is the npm package
  and `node` is installed outside `secure_path` (not tested). The line
  covers `claude` only: `git` and `rm`, which the daemon runs through
  sudo directly, still get `secure_path`, which includes `/usr/bin`.
- `SSH_AUTH_SOCK`: when the daemon has an ssh agent, the path of the
  socket the daemon relays to it (below), in sudo's own environment.
  sudo keeps it because `env_keep` lists it.
- `GIT_TERMINAL_PROMPT=0`, `GIT_CONFIG_GLOBAL=/dev/null`,
  `GIT_CONFIG_NOSYSTEM=1` and `GIT_SSH_COMMAND=ssh -o BatchMode=yes`,
  for `git` only: on sudo's command line, as `VAR=value` before `--`.
  sudo refuses to run a command given a variable that sudoers does not
  allow, with "sorry, you are not allowed to set the following
  environment variables" (sudoers(5), "Denied command log entries"),
  so `env_keep` lists them for `git`, and a rule without them stops
  git rather than letting it read the harness user's git
  configuration.

With a harness user, delivery does not use the daemon's ssh agent: the
daemon fetches and pushes from its mirror as its own user, with its
own key. The relay below gives the agent the daemon's ssh agent for its
own use, such as reaching another repository over ssh.
The daemon's ssh agent socket is open only to the daemon's user. When
the daemon has `SSH_AUTH_SOCK`, it listens on a socket of its own,
`/tmp/orchestrator-agent-*/agent-*.sock`, for as long as it runs, and
relays each connection to its agent byte for byte. The socket has mode
0666 and a random name, in a directory of mode 0711, so the harness
user can connect without a group in common with the daemon's user. Any
local user who learns the socket's path can use the daemon's ssh keys;
the directory's mode keeps others from listing it to learn the name.
Without `SSH_AUTH_SOCK` the harness gets no ssh agent. The daemon's
own ssh runs as the daemon's user and, with a harness user, offers the
remote only the daemon's key, not the keys in its ssh agent;
`known_hosts` must list the repository's host in the daemon user's
`~/.ssh` or in the system's `/etc/ssh/ssh_known_hosts`.

The harness user logs in to Claude Code once on each machine, and tasks
spend that account's quota; the daemon does not manage the login. On
Linux Claude Code keeps it under the user's home, in
`~/.claude/.credentials.json`. On macOS, see step 5 of the checklist
below.

sudo relays SIGTERM to the command it runs but not SIGKILL (sudo(8),
"Signal handling"), and the daemon's user cannot signal the harness
user's processes. When the daemon kills a harness in this mode, as it
does when a stopping harness has not exited within 30 seconds, it sends
sudo SIGTERM and closes the harness's input instead; a harness that
ignores both keeps running. A daemon that restarts and finds a harness
its previous run left sends that harness's sudo SIGTERM.

Checklist for a machine with a daemon user `orchestrator` and a harness
user `orch-agent`. A container check runs steps 1 to 3, the
`known_hosts` part of step 4, and steps 6 to 11 on Linux, with a stub
in place of `claude`, and asserts each. It also checks that the daemon
sends sudo SIGTERM when a stopped harness outlasts the 30 s timeout,
and that a restarted daemon terminates the harness its previous run
left. It checks the private key's mode and that the harness user
cannot read it, that the daemon's page shows the public key, and that
the workspace's `origin` is the mirror. From the logs of sshd, sudo and
an ssh wrapper, it checks that the push left the mirror as the daemon's
user offering only the daemon's key, and that the fetch into the
mirror ran `upload-pack` as the harness user. The test remote accepts
only the daemon's key, so a successful push shows the daemon used it.
With Docker running, from the repository root:

    test/harness-user/run.sh

It prints `ok` or `not ok` for each check and exits 1 if one fails.
The [findings](docs/design/2026-10-09-harness-user-container.md)
record a run.

On macOS with a Claude Code login in the Keychain, `run.sh
--real-claude` runs the same container with Claude Code itself. It
copies the login, without its refresh token, into `orch-agent`'s
`~/.claude/.credentials.json` and spends a few short haiku turns of
that account's quota. It covers step 4's login check (`claude -p` as
`orch-agent` finds a copied login under sudo), steps 7, 8 and 11 with
`claude`, the permission gateway, the `PATH` with and without the
`!secure_path` line above, and a stop and a daemon restart with
`claude` running a tool. Step 10 is not run with `claude`: it sends
`claude` the same SIGTERM from `orch-agent` instead of through sudo,
so step 11's result is expected to carry over (not tested). Its
[findings](docs/design/2026-10-09-harness-user-real-claude.md) record
a run. The interactive `/login` of step 4, step 5, and the macOS forms
of steps 1 and 3 stay manual.

1. Create the harness user, such as with `sudo useradd --create-home
   orch-agent` on Linux or `sudo sysadminctl -addUser orch-agent` on
   macOS.
2. Install `claude` where `orch-agent` can run it, write the sudoers
   rule above with that path, and check it as `orchestrator` with
   `sudo -n -u orch-agent -D / -- /usr/local/bin/claude --version`.
3. Create the workspace directory, such as with `sudo install -d -o
   orch-agent -m 0755 /srv/orchestrator/workspaces` on Linux. macOS's
   system volume is read-only; use a directory such as
   `/Users/Shared/orchestrator-workspaces` there.
4. Log `orch-agent` in to Claude Code: `sudo -u orch-agent -i`, then
   `claude` and `/login`. As `orchestrator`, add each repository host's
   key to `~/.ssh/known_hosts`, such as with `ssh-keyscan HOST >>
   ~/.ssh/known_hosts`, after comparing its fingerprint with the one
   the host publishes.
5. On macOS, confirm where Claude Code keeps its login and that a
   process started through sudo can read it. As `orchestrator`,
   `sudo -n -u orch-agent -D / -- /usr/local/bin/claude -p 'Reply OK'`
   replies rather than asking to log in.
6. As `orchestrator`, with its ssh agent running, start the daemon with
   `-harness-user orch-agent -workspace-dir
   /srv/orchestrator/workspaces -claude /usr/local/bin/claude` and the
   usual flags. The log has a line "running tasks as the harness user"
   with the user, the git and rm paths, and `ssh_agent=true`. The
   daemon's page shows its push key; register it at the forge (see
   [Tasks](#tasks)).
7. Start a task with a repository and a prompt that commits a file.
   The task page shows the branch pushed. In the workspace, as
   `orch-agent`, `git config remote.origin.url` names the daemon's
   mirror under `<state-dir>/mirrors/`.
8. `ps -o user,pid,ppid,command -ax | grep claude` shows `claude` run
   by `orch-agent`, its parent a `sudo` process.
9. In `sudo -u orch-agent -i`, each of these fails with "Permission
   denied": `cat` on the daemon's TLS key (`-key`), `cat
   <state-dir>/ssh_ed25519`, and `ls <state-dir>`. Ask the agent to run
   `ssh-add -l`: it lists the daemon's keys.
10. With a task running, `sudo -u orch-agent kill -TERM <claude pid>`
    ends `claude`, and the task page shows the harness's exit.
11. With another task running, `kill -TERM <sudo pid>` as
    `orchestrator` ends both sudo and `claude`.

### Using the GUI

The dashboard lists the tasks that need attention, every task with the
agent it was started as, the account's quota reading, and the daemons
with their labels. A task with a purpose is listed by its purpose, and
any other by the start of its prompt. Its form starts a task with:

- the project it belongs to, if any, once a project exists (see
  [Projects](#projects));
- the agent to start it as, if any (see [Agents](#agents));
- an optional purpose, one line on why the task exists, which the task
  reads at the top of its prompt;
- its prompt, and an optional repository and ref;
- the model;
- the daemon to run it on, or any connected daemon, and the labels its
  daemon must have (see [Placement](#placement));
- its priority, and whether it is filler (see [Scheduling](#scheduling));
- its pause limits, to acknowledge and to clean up.

A field left blank takes the agent's value, or else the default: the
server's model, no labels, `normal` priority, and pause limits of 1
minute to acknowledge and 5 to clean up. The filler box can only make a
task filler; an agent whose tasks are filler makes the task filler
whether it is ticked or not, and only the owner API's `"filler": false`
overrides that.

Each task page:

- shows the transcript;
- asks for permission when the agent wants to run a tool and the server
  runs with `-permissions ask`;
- shows the branch the daemon pushed the task's work to (see
  [Tasks](#tasks));
- shows its purpose and its project, if it has them;
- lists the tasks it spawned, with each one's purpose, agent, state and
  latest report, and links its parent;
- shows the tree of tasks under it, each with its purpose, state,
  agent, cost and branch;
- has buttons to pause, resume, interrupt or stop the task.

Claude Code can run subagents of its own within a task's turn, with its
`Agent` tool, which Claude Code 2.1.289 listed as `Task`. The daemon runs `claude` with `--forward-subagent-text`,
so a subagent's text and thinking reach the transcript as well as its
tool calls, and the task page nests them under the tool call that
started the subagent, in a list titled by the call's description that
can be folded away. Such subagents run inside the task's own process:
the scheduler does not see them, and they share the task's slot and
daemon (see the [agent model](docs/design/2026-10-09-agent-model.md)
note). When Claude Code starts a subagent in the background, it writes
a `result` at once; the daemon does not treat that as the end of the
turn, which ends when the subagent finishes and Claude Code's follow-up
turn ends. Until then the task is busy and holds its slot. If Claude
Code reports a subagent started and never reports it finished, the task
runs until the owner stops it (see the
[background subagent](docs/design/2026-10-09-background-subagent-turn.md)
note).

The Projects and Agents pages, linked from the top of every page, list
the projects and the agents and create, edit and delete them; a
project's page also lists the tasks started in it and starts more.
Each daemon's row on the dashboard links to the daemon's page, which
shows the facts it reported and sets the labels the owner gives it (see
[Placement](#placement)). Scripts read the same with `GET /v1/daemons`
and `GET /v1/daemons/{daemon}`: each daemon's id, labels and facts, its
ssh key as an `authorized_keys` line (`ssh_public_key`) and as the bare
key (`ssh_public_key_blob`), when it was last seen, whether it is
connected, whether it is lost and since when, its slots, and how many
tasks hold one.

A `stopped` or `failed` task's page, and a `failed` task among those
that need attention, have a Dismiss button. A dismissed task no longer
needs attention and is left out of the dashboard's list of every task,
which says how many dismissed tasks it leaves out; `/?dismissed=show`
includes them, marked as dismissed. Scripts dismiss a task with
`POST /v1/tasks/{task}/dismiss`, which answers 409 unless the task is
`stopped` or `failed`. Dismissal is the only act that ends a task for
good: a dismissed task takes no follow-up, and its daemon deletes its
record, journal and workspace.

### Flags

Server flags:

- `-db` (required): the SQLite database file, created with its directory
  when missing. It holds every daemon, task, event and command, the
  agents and projects, the owner token's hash and the login sessions.
  The server brings an older database to its schema, version 15, when it
  starts, and refuses a database of a later version.
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
- `-filler-threshold`: the utilization of the account's five-hour quota
  window, from 0 to 1, below which filler tasks run; 0.5 by default
  (see [Scheduling](#scheduling)).
- `-low-threshold`: the same for low-priority tasks; 0.85 by default.
- `-daemon-timeout`: how long a daemon may go unseen, with no command
  stream open, before it is lost and its tasks move to other daemons
  (see [Lost daemons](#lost-daemons)); 10 minutes by default, and at
  least a minute, as a daemon retries every 30 seconds at most.
- `-permissions`: who answers the agents' requests to run a tool
  ([permission policy](docs/adr/2026-10-08-permission-policy.md)).
  `allow-all`, the default, has the server allow every request as soon
  as it arrives, and the task's transcript says it was allowed by
  policy. With `ask`, each request waits, without limit, for the owner
  to answer it on the task page or through the owner API. Changing it
  does not answer requests already waiting. Under either setting the
  server denies an `Agent` call that sets `isolation` to `"remote"`,
  which would run Claude Code's subagent in a cloud environment, and
  tells the agent to run it locally; some other paths off the machine
  are not covered
  ([remote subagents](docs/design/2026-10-09-remote-subagents.md)).

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
  under `journal/`, without `-harness-user` each running harness's
  system prompt under `harness/`, which the daemon empties when it
  starts, and, unless
  `-workspace-dir` is given, each task's working directory under
  `workspaces/<task>/`. Claude Code keeps its sessions outside it:
  Claude Code 2.1.289 on macOS kept them under `~/.claude/projects/` of
  the user running it ([resume spike](docs/design/2026-10-08-resume-spike.md)).
  The daemon holds a lock file in the state directory; it waits up to the
  timeout for another daemon to release it, then refuses to start.
- `-workspace-dir`: the directory holding each task's workspace,
  `<task>/`; `workspaces/` under `-state-dir` by default. Required
  with `-harness-user`; it must then exist, be owned by the harness
  user, and be readable by the daemon's user.
- `-harness-user`: the OS user that runs `claude` and every workspace
  command, through `sudo`. Empty, the default, runs them as the
  daemon's own user (see [Running the harness as another
  user](#running-the-harness-as-another-user)).
- `-claude`: the `claude` executable, `claude` on `PATH` by default;
  with `-harness-user`, the absolute path the sudoers rule names.
  The daemon runs `claude --version` when it starts, as the user who
  runs tasks, and exits if that fails. Tasks use the Claude Code login
  of that user, the daemon's own or the harness user, and spend that
  account's quota.
- `-git-identity`: name and email for the commits an agent makes, as
  `Name <email>`; `orchestrator <orchestrator@localhost>` by default.

### Tasks

Without a repository, a task starts in an empty directory. With one, it
starts in a clone on the branch `orchestrator/<task id>`, created at
the ref, or checked out from the repository when it has that branch
already. The ref may name a branch, a tag, or a commit that one of
them holds. The daemon accepts a repository given as an `https://` or
`ssh://` URL with a host, or as an ssh address such as
`git@github.com:owner/repo.git`. A local path, `file://` or `git://`
fails the task, as does a ref that starts with `-`.

The daemon delivers a task's work by pushing its branch to the
repository; nothing of the work goes to the server
([work delivery](docs/adr/2026-10-08-work-delivery.md), [daemon push
identity](docs/adr/2026-10-10-daemon-push-identity.md)). The daemon
keeps a bare mirror of each repository its tasks use, under
`<state-dir>/mirrors/`, and runs every git command against the
repository there, as its own user. When a task starts, the daemon
fetches the repository's branches and tags into the mirror, and the
task's workspace is cloned from the mirror, which is the workspace's
`origin`. At the end of every turn, when the branch holds commits that
the repository's copy of it lacks, the daemon fetches the branch from
the workspace into the mirror and pushes it from there with a plain
`git push`, never forced. A push the repository refuses, such as after
the agent rewrote the branch's history, is reported as the push's
error. After a push, and after any turn that leaves files uncommitted,
pushed or not, the task page shows the branch, its commit, how many commits
the branch holds beyond the ref, how many files the agent left
uncommitted, and any push error. The dashboard's task list shows each
task's branch and marks a failed push. The daemon adds to the task's
system prompt that the agent must commit its work on the branch and
never push. A `pre-push` hook in the clone refuses to push any other
ref, and the daemon refuses to push the ref the task started from or
the repository's default branch. Work the agent did not commit is not
delivered. A child task gets a branch of its own, starting at its
parent's ref.

Each daemon has an ed25519 key of its own, its push key, which it
generates when it starts and finds no `ssh_ed25519` in its
`-state-dir`: `ssh_ed25519` and `ssh_ed25519.pub`, the private key
readable by its user only. It reports the public key to the server as
the fact `ssh_public_key`, and the daemon's page in the GUI shows it as
an `authorized_keys` line, `ssh-ed25519 <key> orchestrator@<daemon
id>`. Registering the key at the forge is the owner's job: as a deploy
key with write access on a repository the daemon's tasks use, or on a
machine user with access to them. GitHub accepts a deploy key on one
repository only, so a daemon whose tasks use several GitHub
repositories needs a machine user there. Delete both files to have the daemon generate a new
key at its next start; a private key without its public key stops the
daemon, so that a registered key is not replaced by accident.

The daemon's `git` runs with prompts off and ignores the user's and the
system's git configuration, credential helpers and URL rewrites
included (`GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
GIT_TERMINAL_PROMPT=0`), and runs ssh in batch mode with the daemon's
key, `ssh -i <state-dir>/ssh_ed25519 -o BatchMode=yes`. Without
`-harness-user`, ssh also offers the keys of the daemon's ssh agent,
such as the owner's, when `SSH_AUTH_SOCK` is set; with it, ssh adds
`-o IdentitiesOnly=yes` and offers the daemon's key alone. ssh
otherwise reads the daemon user's `~/.ssh`, and `known_hosts` there
must already list the host, since batch mode fails the fetch or the
push on an unknown host key rather than waiting. A repository over
https that needs credentials fails to fetch, as no credential helper
applies.

What credentials the agent can reach depends on `-harness-user`.
Without it, the harness and the agent it runs inherit the daemon's
environment, the ssh agent included, and the agent can read the
daemon's key as it can the rest of the daemon's files. With it, the
agent gets the daemon's ssh agent through a socket the daemon relays
(see [Running the harness as another
user](#running-the-harness-as-another-user)), and cannot read the
daemon's key or write the mirror. Either way the agent is told not to
push, its `origin` is the mirror, and a `pre-push` hook in the clone
refuses any ref but the task branch; without `-harness-user` the agent
can get around these. The agent's own `git
commit` uses the clone's local configuration instead of the daemon
user's: identity from `-git-identity` (`orchestrator
<orchestrator@localhost>` by default) and signing off, so the daemon
user's own signing settings do not apply to agent commits; other
user-level git settings still apply to the agent's git.

Each turn of a task runs in its own `claude` process, which exits when
the turn ends. The task is then `finished`, `paused` if the owner paused
it during the turn, or `yielded` if the scheduler did. A follow-up
prompt, or Resume on a paused or yielded task, starts a new process that continues the same Claude Code session in the same
working directory. A `stopped` or `failed` task takes a follow-up
prompt, or Resume, in the same way, until the owner dismisses it; its
daemon keeps its session and workspace until then. A task whose harness
never started, because its workspace could not be prepared or its
harness could not be started, says so, as "workspace could not be
prepared: ..." or "harness could not be started: ...", in its header
and its transcript. It has no session to continue: its page
offers Retry instead of Resume, and Retry or a follow-up starts it
afresh, as a new start placed on whichever daemon fits, with its first
prompt followed by the follow-up, if any.

Interrupt, on the task page, ends the running turn at once. Claude Code
2.1.289 then exited with code 1. Unless the exit code is 0, the task
becomes `paused`: its page and the dashboard's tasks that need
attention say it was interrupted by the owner, and Resume continues its
session, telling the agent that the owner interrupted its last turn
([interrupt findings](docs/design/2026-10-08-interrupt-findings.md)).
A follow-up prompt sent after the interrupt runs as the next turn, and
the task ends as that turn does.

While a turn runs, the task page's follow-up form sends a prompt in one
of two ways. "Send after this turn" has the daemon hold the prompt
until the turn ends and then send it as the next turn's prompt, in the
same `claude` process; until then the task page lists it under Queued
prompts, where Withdraw drops it. The daemon holds it rather than write
it to `claude`, which would queue it where it cannot be withdrawn. A
turn that ends with the task pausing, or a process that exits, drops
the prompts held, and the transcript says so. "Send now" steers the
task: the daemon interrupts the running turn and sends the prompt as
the next one in the same session, and the transcript shows the owner's
steering rather than an interrupt that pauses the task. Prompts waiting
for the scheduler are listed and withdrawn the same way. Scripts send a
`prompt` command with `"steer": true` to steer, withdraw a held prompt
with a `withdraw` command naming it as `{"prompt": <command id>}`, and
withdraw a waiting one with `DELETE /v1/tasks/{task}/turns/{turn}`; the
task's `queued` field lists both.

A task's page gives the size of its session's context as of its latest
turn: the input, cache read and cache write tokens of that turn's
`result`, out of the model's context window when the `result` names
it. Between turns the follow-up form says how much context resuming
reads; on a Claude subscription, within the plan's usage, Claude Code
caches the conversation for an hour after its last request
([prompt caching](https://code.claude.com/docs/en/prompt-caching#resuming-a-session)).
"Continue in a new task", on the page of a task between turns, starts a
new task in the same project, as the same agent, whose prompt is the
task's final reply as a hand-back would carry it: a fresh session that
reads none of the old one's context. The new task names the old one
under Continues, and the old one lists it under Continued by; the old
task is not its parent. Scripts do the same with
`POST /v1/tasks/{task}/continue`.

The daemon deletes a task's working directory, with everything in it,
when the task ends for good on that daemon: when the owner dismisses
it, and when its first start fails, since a follow-up then starts it
afresh. A stopped or failed task keeps its working directory until it
is dismissed. A daemon
starting on its `-state-dir` also deletes every working directory under
`workspaces/` of a task it no longer knows or cannot run again. A task
a shutdown or restart paused keeps its working directory. Before it
deletes a clone, the daemon pushes the task's branch if it holds
commits the repository lacks; if that push fails, the daemon keeps the
clone, logs the failure, and tries again the next time it starts. When
`<state-dir>/workspace-repos/<task id>` is missing for a clone, as for
one an older daemon made, the daemon does not push it and keeps it;
recover its commits and delete it by hand.

An agent can start child tasks and message other tasks with two tools
the daemon gives it, `spawn_task(purpose, prompt, agent?, model?,
requires?)` and `send_message(to, text)`
([inbox delivery](docs/adr/2026-10-08-inbox-delivery.md)), unless its
agent allows it fewer (see [Agents](#agents)). The server explains the
tools a task may use in a system prompt it gives the task, ahead of the
agent's system prompt, the project's instructions and any given through
the owner API, in that order; a task that may spawn is also told the
name and description of every agent, and what the purpose is for.

A spawn must give a purpose: one line saying why the child exists and
what the parent expects back
([projects and lineage](docs/adr/2026-10-10-projects-and-lineage.md)).
The tool's schema requires it, and the server refuses a spawn without
one, telling the agent why. The child keeps the purpose, with its line
breaks made spaces, and its prompt starts with `Purpose: <purpose>` as
a paragraph of its own. A task the owner starts may carry a purpose
too, in `POST /v1/tasks` as `purpose` or in the new-task form, and its
prompt starts the same way. A child belongs to its parent's project. A
child runs on its parent's daemon when that has a free slot and the labels
the child requires (see [Placement](#placement)), in a fresh clone of
the parent's repository if it has one. A child started as an agent has
that agent's priority, filler flag and labels, its model and pause
limits, or else the parent's, and those of the agent's tools that the
parent may call too. A child started as no agent may call the tools its
parent may call, requires no labels, and takes its parent's model,
pause limits, priority and filler flag. A model or labels given to
`spawn_task` win over both. A message to a `finished` task becomes its
next turn; one to a running task waits until its turn ends, and one to
a `paused` task until the owner resumes it and that turn ends. Messages
to `stopped` or `failed` tasks are refused, and a parent is told when
its child stops or fails. When a task stops or fails with messages
still waiting in its inbox, each sender's next prompt is a notice that
those messages were not delivered. The task page links a task's parent,
lists its children with each one's purpose, branch and latest report,
shows the tree of tasks under it, and shows the messages it sent and
received.

A task's JSON in the owner API carries `parent_id`, `project` and
`purpose` when it has them. `GET /v1/tasks/{task}/tree` returns the
tree of tasks rooted at a task: each node has the task's `id`,
`state`, `agent` and `purpose` when it has them, `cost_usd`, `branch`
as the daemon last reported it, if any, and `children`, oldest first,
in the same shape:

    {"id": "R4…", "state": "finished", "agent": "brain", "cost_usd": 0.1,
     "children": [{"id": "W6…", "state": "finished", "agent": "worker",
       "purpose": "Fix the README's process listing.", "cost_usd": 0.05,
       "branch": {"branch": "orchestrator/W6…", "commit": "eb69b7b…",
         "ahead": 1, "uncommitted": 0, "error": ""},
       "children": []}]}

A child reports to its parent with `send_message`, or by ending its
turn: when a child's turn ends with the task `finished` and the child
sent no message during that turn, the server sends the turn's last
text, from the child's main conversation rather than a subagent's, to
the parent as a hand-back
([agents and placement](docs/adr/2026-10-09-agents-and-placement.md)).
A turn that wrote no text hands back a notice that it wrote none. The
hand-back ends with the child's branch and its latest commit, as the
daemon last reported them, or says that no branch was pushed. The
parent receives it as a prompt like any message, marked as a hand-back,
and its transcript shows it as "Report from child" with the child's
id. A
child that did send a message during the turn hands back nothing, and
the owner's tasks, which have no parent, never do.

### Agents

An agent is a named definition that tasks are started as: its
description, system prompt, model, which of the agent tools
`spawn_task` and `send_message` it allows, pause limits, priority, filler flag,
and the labels its daemon must have
([agents and placement](docs/adr/2026-10-09-agents-and-placement.md)).
The permission and pause tools are always available. An agent created
without tools allows neither agent tool, and an agent without pause
limits gives its tasks the default limits. The owner creates and edits agents on
the Agents page; an existing instructions file becomes an agent by
pasting it into the system prompt field. Agents cannot be renamed,
and the server refuses to delete an agent that any task names. Editing an agent changes only the tasks started afterwards.

Scripts use the owner API: `GET /v1/agents` lists them, `POST
/v1/agents` creates one (409 when the name is taken), `GET` and `PUT
/v1/agents/{agent}` read and replace one, and `DELETE
/v1/agents/{agent}` deletes one (409 while a task names it, or a
project names it as its default agent). An agent
is JSON such as:

    {"name": "reviewer", "description": "Reviews a change.",
     "system_prompt": "You review code.", "model": "sonnet",
     "tools": ["send_message"],
     "pause_limits": {"acknowledge": "1m", "cleanup": "5m"},
     "priority": "normal", "filler": false, "requires": {"os": "linux"}}

`POST /v1/tasks` takes `agent`, and `requires` as such an object; a
field left out takes the agent's value. It also takes `tools`, a list
that replaces the agent's.

### Projects

A project is a reusable container for tasks the owner defines: a name,
instructions, an optional repository and ref, and an optional default
agent ([projects and lineage](docs/adr/2026-10-10-projects-and-lineage.md)).
A task belongs to at most one project, chosen when the owner starts it;
a child belongs to its parent's. A task in a project:

- works in the project's repository, starting at the project's ref, or
  in an empty workspace when the project names no repository; a
  request naming a repository of its own is refused either way;
- is started as the project's default agent unless it names an agent
  of its own; a child the agent spawns naming no agent still has none;
- has the project's instructions in its system prompt, after its
  agent's system prompt and before what the owner's request adds, so
  the agent's role comes before the project's conventions.

A task without a project takes none of this. Editing a project changes
only the tasks started afterwards, and the server refuses to delete a
project that any task belongs to. A project's name is unique and can
change; the server gives each project an id that does not.

On the Projects page the owner creates projects; each project's page
edits it, lists the tasks the owner started in it with each one's
state, agent, cost and branch, each linking to the tree on its task's
page, and has a form that starts a task in it.

Scripts use the owner API: `GET /v1/projects` lists them, `POST
/v1/projects` creates one and answers 201 with it, id included (409
when the name is taken, 422 when the default agent does not exist),
`GET` and `PUT /v1/projects/{project}` read and replace one by id, and
`DELETE /v1/projects/{project}` deletes one (409 while a task belongs
to it). A project is JSON such as:

    {"name": "orchestrator", "instructions": "Scope commit subjects by path.",
     "repo": "ssh://git@host/orchestrator.git", "ref": "main",
     "default_agent": "brain"}

`repo` and `ref` go together; both are left out for no repository.
`POST /v1/tasks` takes `project`, a project's id.

SIGINT or SIGTERM shuts either program down. The daemon first
interrupts each running turn and closes the input of the task's
`claude` process, killing it if it has not exited within 30 seconds.
A task whose turn this cuts short becomes `paused`, and its page and
the dashboard's tasks that need attention say that the daemon stopped
during its turn; it runs again only when the owner resumes it
([shutdown recovery](docs/adr/2026-10-08-shutdown-recovery.md)). A turn
that ended before the interrupt arrived leaves its task as the turn
left it. The daemon then sends the server the events it has not sent
yet, for up to 30 more seconds. A second signal makes it exit at once.

A daemon started again with the same `-state-dir` finds the tasks that
were in the middle of a turn when its previous run ended, by a crash, a
kill, or a second signal. Each of those tasks becomes `paused`, and its
page and the dashboard's tasks that need attention say that the daemon
restarted. A task's `claude` process may outlive the daemon that
started it; before it pauses the task, the new daemon waits up to 30
seconds for that process to exit and kills it if it is still running.
It recognises the process by its pid and its start time, which it reads
with `ps`; without `ps` it leaves such a process alone. A task whose
workspace was still being cloned becomes `failed`. A task that was
between turns keeps its state.

Resuming a task paused by a shutdown or a restart continues the task's
Claude Code session and tells the agent that its last turn was cut
short; when the process died before Claude Code reported a session,
Resume starts a new session with the task's first prompt instead.

### Scheduling

A task's start, a follow-up prompt, a resume and a message delivery
each become a turn in a single queue shared by all daemons, and a
scheduler decides when each one starts
([scheduling](docs/adr/2026-10-08-scheduling.md)). `POST /v1/tasks`
answers 201 with the queued start; a prompt or resume posted to
`/v1/tasks/{task}/commands` answers 202 with the queued turn. Pause,
interrupt, stop and permission answers are sent to the daemon at once,
without queueing.

- **Slots.** Each daemon runs at most as many tasks at once as its
  capacity (see [Placement](#placement)). A task holds a slot from the
  admission of its turn until its process exits. A task started without a daemon goes to the connected daemon
  with the most free slots, and stays on that daemon, where its
  workspace is. Turns for a daemon that is not connected wait until it
  reconnects, or until it is lost (see [Lost daemons](#lost-daemons)).
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

### Placement

A daemon has labels, `key=value` pairs, from two sources
([agents and placement](docs/adr/2026-10-09-agents-and-placement.md)).
Each time it opens its command stream the daemon reports facts about
its machine with `PUT /v1/daemons/{daemon}/facts`: `os` and `arch` as
Go names them, such as `darwin` and `arm64`; `cpus`; `memory` in bytes,
on Linux and macOS; `harness` and `harness_version`, such as
`claude-code` and `2.1.289`; `gpu`, `nvidia` when `nvidia-smi` is on
its `PATH` or `apple` on darwin/arm64, and absent otherwise; and
`ssh_public_key`, the base64 of its push key's public key, the `<key>`
of the line shown in [Tasks](#tasks). The owner sets labels on the daemon's page; where a
label and a fact share a key, the label wins. Keys are letters, digits, `.`, `_` and `-`;
values are printable characters other than space, `,` and `=`.

A task requires the labels of its agent, or those given when it is
created or spawned, which replace the agent's. A task starts only on a
connected daemon whose labels hold every required pair, even when the
owner named the daemon, and a child goes to its parent's daemon only
when that one has them. Matching is exact, so `memory=34359738368`
matches only that size; set a label such as `size=large` to place by
size. If no connected daemon qualifies, the task waits with the reason "no
daemon has" followed by the pairs that no connected daemon has. A task
bound to a daemon that lacks them says what that daemon lacks.

### Lost daemons

A daemon is lost once it has had no command stream open, and made no
request, for `-daemon-timeout`; time before the server started does not
count ([daemon loss](docs/adr/2026-10-08-daemon-loss.md)). The
dashboard's list of daemons says since when each lost daemon has been
lost, until it connects again.

A task on a lost daemon moves to another daemon: its workspace and its
Claude Code session stay on the lost machine, so the task starts again
in a fresh clone and a new session, with its first prompt and a note
that its earlier work is gone, carrying the owner's queued prompts or,
with none queued, quoting the owner's latest prompt to it.
For a task with a repository, the new daemon fetches the task's branch,
as the lost daemon last pushed it, into its mirror and checks it out in
the fresh clone, which needs the new daemon's push key registered at
the forge too; the note says that only
the work not pushed is gone
([work delivery](docs/adr/2026-10-08-work-delivery.md)).
A task in the middle of a turn, or with a prompt or resume waiting,
moves when the daemon is declared lost; a `finished` task with a
message waiting does too. A `finished`, `paused` or `yielded` task with
nothing waiting moves when its next turn is queued, so a daemon that
returns first keeps it. A moved task goes to the connected daemon with
the most free slots, or a child to its parent's daemon if that has one
free, and never to a daemon it ran on before; while only such daemons
are connected it waits. Its page shows the move. Every prompt the owner
queued for it is carried in the note, oldest first, each labelled with
its place, so none is dropped. Prompts already sent to the lost daemon,
as those to a running task are, are not carried; the note quotes the
latest of them only when none is queued.
A task waiting for its start on a lost daemon the owner named is placed
as if the owner had named none.

When a lost daemon connects again and sends events of a task that has
moved, the server refuses them, and the daemon kills that task's
`claude` process and deletes the task's record, journal and workspace.
A daemon that has nothing left to send for a moved task keeps them.

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
  the need they answer. Each starts with YAML frontmatter with the keys
  `status` (`proposed`, `accepted` or `superseded`), `date` (the day the
  decision was made), `source` (where the decision came from: a session,
  a spike or a design note), and, when another record has replaced it,
  `superseded-by` (the newer record's path); the newer record carries
  `supersedes`. Sections: Context, Decision, Consequences, and at most one
  `Revisit if`. There are no amendments: an accepted record is immutable
  except for marking it superseded; a change of mind is a new record
  that supersedes the old one.
- `spikes/<name>/`: throwaway experiments with a README saying how to
  rerun them. A spike that commits logs carries a redaction script
  (`spikes/mod-vs-stdout/redact.py` is the first) and runs it before any
  log is committed; it removes e-mail addresses, account and organisation
  ids, device ids, and credential handles (`auth_...` values).

In every document, a claim that was not checked against a primary source
or an experiment is prefixed `**UNVERIFIED:**`. A summary of someone
else's findings keeps their markers.
