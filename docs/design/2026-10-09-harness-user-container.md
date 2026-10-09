# Findings: the harness user mode in a two-user Linux container (2026-10-09)

The [harness user](../adr/2026-10-08-harness-user.md) mode
(`-harness-user`, `-workspace-dir`; `internal/runas`,
`internal/sshagent`, and the runner and orphan paths in
`internal/daemon`) had only Go tests that run as one OS user, and the checklist in
README.md, section "Running the harness as another user", had not been
run. This note records a run of that mode with two real OS users in a
Linux container, the checklist steps it asserts, and those it leaves to
a machine with a Claude Code login.

**UNVERIFIED** marks a claim that was neither observed in the run below
nor read from a source named here.

## Method

`test/harness-user/run.sh` cross-compiles `cmd/server` and
`cmd/daemon` with `CGO_ENABLED=0 GOOS=linux GOARCH=<the Docker server's
architecture>` in the Nix dev shell, builds the image in
`test/harness-user/Dockerfile`, and runs `check.sh` in it as root.
`check.sh` prints `ok` or `not ok` for each assertion and `#` lines of
evidence, and exits 1 when an assertion fails. `go test ./...` does not run it.

Environment of the run recorded here:

- Host: macOS (Darwin 25.4.0, arm64), Docker Desktop 28.3.2.
- Image: `debian:trixie-slim` (Debian 13), aarch64, kernel
  6.10.14-linuxkit; sudo 1.9.16p2, git 2.47.3, OpenSSH 10.0p2.
- Binaries: built from commit `671fda37`, Go 1.26.8, linux/arm64.
- Users: `orchestrator` (the daemon), `orch-agent` (the harness user,
  uid 1001), `git` (the remote's ssh login). `orchestrator`'s home is
  mode 0755, so that the checks of step 9 test the modes of the
  daemon's own files rather than a closed home.
- sudoers: `test/harness-user/sudoers`, the README's lines with `rm` at
  `/usr/bin/rm`, which `command -v rm` returns on Debian 13 with
  Debian's default `PATH` (`/usr/local/bin:/usr/bin:/bin`). The daemon
  runs with that `PATH`. Debian's own `/etc/sudoers` sets `secure_path`
  and `use_pty`.
- Remote: `sshd` on localhost; a bare repository
  `/home/git/repo.git` with one commit on `main`, reached as
  `git@localhost:repo.git`. `orchestrator`'s ed25519 key is in `git`'s
  `authorized_keys` and in an `ssh-agent` that `orchestrator` runs; the
  daemon starts with that agent's `SSH_AUTH_SOCK`. `orch-agent`'s
  `known_hosts` holds localhost's key from `ssh-keyscan`, run as
  `orch-agent`.
- Server: `server -insecure-loopback -permissions allow-all
  -slots-per-daemon 4`, run as root; the daemon's certificate and key
  in `/home/orchestrator/pki`, the key mode 0600. Tasks are created
  with `POST /v1/tasks` naming the daemon, the repository at `main`,
  and pause limits.
- Daemon: `daemon -harness-user orch-agent -workspace-dir
  /srv/orchestrator/workspaces -claude /usr/local/bin/claude`, run as
  `orchestrator` with umask 022.

The `claude` at `/usr/local/bin/claude` is `test/harness-user/claude-stub`,
a bash script. It answers `--version` with `0.0.0-stub (Claude Code)`,
writes a `system/init` line, and for each `user` line on stdin writes
an `assistant` line and a `result` that echoes the prompt's `uuid` as
`user_message_uuid`; it answers a `control_request` with a
`control_response`. It exits 0 when stdin closes, leaving `/tmp/stub-claude/<pid>.eof`,
and 143 on SIGTERM, leaving `/tmp/stub-claude/<pid>.term`. The
assistant text is a JSON object with what the stub sees: `id -un`,
`id -u`, its pid and parent pid, its cwd, `HOME`, `SSH_AUTH_SOCK`, and
the output of `ssh-add -l`. A run without `--resume` writes a file in
its cwd and commits it. A prompt with `hold:PATH` holds the turn after
the assistant line, not reading stdin, until PATH exists; the check
never creates PATH, so a held stub ends only by a signal.

Five tasks ran, each with the repository:

| Task | Prompt | Ends by |
| --- | --- | --- |
| A | report and commit | the turn's end; the daemon closes stdin |
| B | hold | `sudo -u orch-agent kill -TERM <stub pid>`, as root |
| C | hold | `kill -TERM <sudo pid>`, as `orchestrator` |
| D | hold | a `stop` command through the server's API |
| E | hold | the daemon killed with SIGKILL and started again |

## Results

All 54 assertions passed in four runs; this note quotes the third. Excerpts are from the recorded run's
output; task ids are shortened to their first four characters, and
`.../` stands for `/srv/orchestrator/workspaces/`.

### Step 1: the harness user

    # id orch-agent
    #   uid=1001(orch-agent) gid=1001(orch-agent) groups=1001(orch-agent)

The image creates the users with `useradd --create-home`.

### Step 2: the sudoers rule

`visudo -c` at image build and `visudo -cf /etc/sudoers.d/orchestrator`
in the check accepted the rule under sudo 1.9.16p2. As `orchestrator`:

    sudo -n -u orch-agent -D / -- /usr/local/bin/claude --version
      0.0.0-stub (Claude Code)
    sudo -n -u orch-agent -- /usr/bin/cat /etc/hostname
      sudo: a password is required
    sudo -n -u root -- /usr/bin/git --version
      sudo: a password is required
    sudo -n -u orch-agent -D / FOO=bar -- /usr/local/bin/claude --version
      sudo: sorry, you are not allowed to set the following environment variables: FOO

and `sudo -n -u orch-agent -D / GIT_TERMINAL_PROMPT=0
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 'GIT_SSH_COMMAND=ssh
-o BatchMode=yes' -- /usr/bin/git --version` printed a git version. The
rule (`test/harness-user/sudoers`) let `claude` and `git` run as
`orch-agent`, refused `cat` as `orch-agent` and `git` as root, and
refused `FOO`, which its `env_keep` does not list, as the README
describes. `rm` ran as `orch-agent` when the daemon deleted task D's
workspace, below.

### Step 3: the workspace directory

`install -d -o orch-agent -m 0755 /srv/orchestrator/workspaces` in the
image; the check asserts the owner. A clone the daemon made as
`orch-agent`:

    # workspace: drwx------ orch-agent .../UXMJ...
    # as orchestrator: ls .../UXMJ...
    #   ls: cannot open directory '.../UXMJ...': Permission denied

Every file under it is owned by `orch-agent` (`find ! -user orch-agent`
printed nothing), and its mode 0700 shows sudoers' `umask=0077` applied
over the daemon's 022. As `orchestrator`, `sudo -n -u orch-agent -D
.../UXMJ... -- /usr/bin/git rev-parse --show-toplevel` printed the
workspace: `-D` with `CWD=*` runs a command in a directory the daemon's
user cannot enter.

### Step 4: known_hosts

`ssh-keyscan -t ed25519 localhost` as `orch-agent` wrote
`~orch-agent/.ssh/known_hosts`; the clone and push below used it under
`GIT_SSH_COMMAND=ssh -o BatchMode=yes`. Logging `orch-agent` in to
Claude Code is not covered.

### Step 6: the daemon starts as the harness user's runner

The daemon logs the harness user, the paths it found for git and rm,
and that it has an ssh agent to relay:

    level=INFO msg="running tasks as the harness user" user=orch-agent git=/usr/bin/git rm=/usr/bin/rm workspace_dir=/srv/orchestrator/workspaces ssh_agent=true

The daemon relays its ssh agent through a socket of mode 0666 in a
directory of mode 0711, which `orch-agent` can pass through but not
list. `orch-agent` cannot open the socket of the daemon user's own
agent, `/home/orchestrator/.ssh/agent.sock`:

    # forwarder: drwx--x--x orchestrator /tmp/orchestrator-agent-3063950286 srw-rw-rw- orchestrator /tmp/orchestrator-agent-3063950286/agent-ZX7O....sock
    # as orch-agent: ls /tmp/orchestrator-agent-3063950286
    #   ls: cannot open directory '/tmp/orchestrator-agent-3063950286': Permission denied
    # as orch-agent: SSH_AUTH_SOCK=/home/orchestrator/.ssh/agent.sock ssh-add -l
    #   Error connecting to agent: Permission denied

### Step 7: a task commits, and the daemon pushes as the harness user

Task A's events, from `GET /v1/tasks/{id}/events`:

    harness_started: {"pid":231,"model":"haiku","workdir":".../UXMJ..."}
    stub report: {"user":"orch-agent","uid":1001,"pid":234,"ppid":231,"cwd":".../UXMJ...","home":"/home/orch-agent",
                  "ssh_auth_sock":"/tmp/orchestrator-agent-3063950286/agent-ZX7O....sock",
                  "ssh_add_l":"256 SHA256:KSLs... orchestrator@container (ED25519)","ssh_add_status":0,
                  "commit":"72123ee4...","commit_error":"",...}
    branch_pushed: {"branch":"orchestrator/UXMJ...","commit":"72123ee4...","ahead":1,"uncommitted":0,"error":""}
    harness_exited: {"exit_code":0}

The stub's parent pid is the pid `harness_started` reports, which is
sudo's. In the bare repository, as `git`:

    refs/heads/orchestrator/UXMJ... = 72123ee4...
    commit 72123ee by orchestrator <orchestrator@localhost>: stub: add stub-234.txt

The daemon's runner (`internal/daemon/runner.go`) runs the clone, the
branch setup, the status and history reads, `ls-remote` and the push
as `orch-agent` through sudo; they succeeded although the daemon's user
cannot enter the workspace. The author is the default
`-git-identity`, which the daemon set in the clone's local
configuration. The task page, `GET /tasks/{id}`, contains the branch
name; the check does not look at how the page shows it.

### Step 8: the process tree

With task B held, `ps -o user,pid,ppid,command -ax | grep claude`,
commands cut:

    orchest+   121   119 /opt/orchestrator/bin/daemon -server http://127.0.0.1:8080 ... -claude /usr/local/bin/claude
    root       468   121 sudo -n -u orch-agent -D .../XDWG... -- /usr/local/bin/claude -p --input-format stream-json ...
    orch-ag+   471   468 /bin/bash /usr/local/bin/claude -p --input-format stream-json ...

The check asserts with `ps -o user:32=,ppid=` that pid 471 runs as
`orch-agent` with parent 468, the pid in `harness_started`, and that
468's command starts with `sudo -n -u orch-agent -D <workspace> --
/usr/local/bin/claude`. sudo runs as root because it is setuid. The grep in
the README's command also matches the daemon, whose `-claude` flag
names the path. The sudo and stub command lines carry the whole
`--append-system-prompt` and the gateway URL
`http://127.0.0.1:<port>/tasks/<id>/mcp`, so any local user can read
them, as the ADR's Consequences say.

### Step 9: the daemon's files, as the harness user

Modes as the daemon left them:

    # daemon files: 600 orchestrator /home/orchestrator/pki/daemon.key 700 orchestrator /home/orchestrator/state
    #   700 orchestrator /home/orchestrator/state/journal 600 orchestrator /home/orchestrator/state/state.json
    # journal: 600 orchestrator /home/orchestrator/state/journal/XDWG....jsonl

In `sudo -u orch-agent -i`, each of these failed with "Permission
denied": `cat /home/orchestrator/pki/daemon.key`, `ls
/home/orchestrator/state`, `cat` of task B's journal while B ran, and
`cat /home/orchestrator/.ssh/id_ed25519`. The journal is checked while
its harness runs because the daemon deletes a journal once the server
holds its events and no process writes it (`dropJournal` in
`internal/daemon/state.go`); task A's was gone by the time A's turn
ended. The second half of step 9, `ssh-add -l` from the agent, is the
stub's `ssh_add_l` in step 7: it listed the daemon user's key through
the relayed socket.

### Step 10: SIGTERM to the harness from the harness user

    # as root: sudo -u orch-agent kill -TERM 471
    harness_exited: {"exit_code":143}

The stub left its SIGTERM file, and the stub and its sudo were gone
within 5 s. sudo exited with the stub's status, 143.

### Step 11: SIGTERM to sudo from the daemon's user

    # as orchestrator: kill -TERM 696
    harness_exited: {"exit_code":143}

sudo relayed SIGTERM to the stub (pid 699), which left its SIGTERM file;
both were gone within 5 s.

### The daemon's Kill on stop

Task D's stub was held, so it read neither the stop's interrupt nor
the end of its input. `POST /v1/tasks/{id}/commands {"kind":"stop"}`:

    # stop to harness_exited: 31s; harness_exited: {"exit_code":143}
    level=INFO msg="process ended" task=IJ3Z... exit_code=143 paused=false cut_short=false stopped=true

After the 30 s shutdown timeout (`defaultShutdownTimeout` in
`internal/daemon/serve.go`; `cmd/daemon` does not set it) the daemon's
`Kill` sent sudo SIGTERM,
sudo relayed it, and the stub left its SIGTERM file and no end-of-input
file. The daemon then deleted the stopped task's workspace through
`rm` as `orch-agent`: `.../IJ3Z...` was gone within 30 s.

### A restarted daemon and the harness its previous run left

With task E's stub held, the check killed the daemon with SIGKILL. The
stub kept running. The daemon, started again:

    level=INFO msg="harness left by the previous daemon is still running; waiting for it to exit" task=EJ5N... pid=1491 wait=30s
    level=WARN msg="harness left by the previous daemon did not exit; killed it" task=EJ5N... pid=1491 after=30.009s
    harness_exited: {"exit_code":-1,"error":"daemon restarted during the turn"}

pid 1491 was sudo's; the stub left its SIGTERM file and both were gone.

## Not covered

The run does not cover the following. The first four need a Claude
Code login or macOS and remain manual steps of the README checklist;
the rest are outside the checklist.

- Step 4, first half: logging `orch-agent` in to Claude Code with
  `sudo -u orch-agent -i` and `/login`.
- Step 5: where Claude Code keeps its login on macOS, the Keychain, and
  whether a process started through sudo reads it.
- Steps 1 and 3 on macOS (`sysadminctl`, `/Users/Shared`).
- What `claude` itself does on SIGTERM and on the end of its input: the
  stub's script exits on both, so the run shows nothing about
  `claude`. **UNVERIFIED:** that real
  `claude` exits on the SIGTERM sudo relays, and that the processes it
  starts for its tools, such as Bash, end with it.
- The agent's own tools as `orch-agent`: no tool call ran, so the
  permission gateway, the Bash tool's environment and its `PATH` under
  Debian's `secure_path` were not exercised. **UNVERIFIED:** that
  `claude` and its tools find what they need on the `PATH` sudo gives
  them when sudoers sets `secure_path`.
- A follow-up turn, which starts the harness with `--resume`.
- The daemon's mTLS: the server ran with `-insecure-loopback`.
- The daemon's start-up deletion of workspaces it does not know: the
  check puts no such workspace in `-workspace-dir`.

## Running it

From the repository root, with Docker running:

    test/harness-user/run.sh [OUTDIR]

OUTDIR, by default a new temporary directory, receives the binaries and
the server's and daemon's logs. One run of the container, with the
image built, took 66 s on the host above; the stop and restart checks
each wait 30 s for the daemon's timeout.
