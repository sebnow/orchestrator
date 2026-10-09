# Findings: the harness user mode with Claude Code itself (2026-10-09)

The [container check](2026-10-09-harness-user-container.md) of the
[harness user](../adr/2026-10-08-harness-user.md) mode ran with a stub
in place of `claude`, so it left open the checklist steps that need
Claude Code: logging the harness user in (step 4), what `claude` does
on the SIGTERM sudo relays (steps 10 and 11, and the daemon's `Kill`),
and the `PATH` the agent's tools get under sudo's `secure_path`. This
note records a run of the same container with Claude Code 2.1.289,
logged in with the owner's subscription.

**UNVERIFIED** marks a claim that was neither observed in the runs
below nor read from a source named here.

## Method

`test/harness-user/run.sh --real-claude` builds the Dockerfile's target
`real`, which is the stub image's setup with Claude Code's native
build at `/usr/local/bin/claude` in place of the stub. The binary
comes from `https://downloads.claude.ai/claude-code-releases/2.1.289/<platform>/claude`,
the URL `https://claude.ai/install.sh` uses, and is checked against
the SHA-256 in that release's `manifest.json`. The installer itself is
not run: it first downloads the latest release, then has that binary
install the requested one into the installing user's home, and the
check needs the binary where the harness user can run it.

The login is the owner's, from the macOS Keychain item
"Claude Code-credentials". run.sh reads it once with `security
find-generic-password -s "Claude Code-credentials" -w` into a shell
variable, refuses to run when the access token expires within 30
minutes, removes `refreshToken` and `refreshTokenExpiresAt` with `jq`,
and pipes the rest into `docker run --interactive`. check.sh, the
container's first process, writes standard input to
`/home/orch-agent/.claude/.credentials.json` with umask 077, owned by
`orch-agent`, then reopens its standard input on `/dev/null`.
`~orch-agent/.claude` is a tmpfs (`--tmpfs
/home/orch-agent/.claude:mode=0700`, chowned to `orch-agent` by
check.sh). run.sh and check.sh write the login only to that tmpfs,
not to the image or the container's disk, and neither passes the
token as a command argument, so no `ps` line shows it; check.sh prints only the file's mode, owner, file
system and key names; `finish` deletes the file and asserts it is
gone. After the container exits, run.sh greps everything under OUTDIR,
including `check.log`, the copy of the run's output, for the access
and refresh tokens, and fails if either appears.

The refresh token stays on the host because the container has no use
for it while the access token is valid, and because a renewal in the
container could rotate it and log the owner out on the host
(**UNVERIFIED** that Claude Code's OAuth server rotates refresh
tokens).

Everything else is as in the [earlier note](2026-10-09-harness-user-container.md#method):
the same users, sudoers rule, ssh remote, server with `-permissions
allow-all`, and daemon flags. check.sh runs the stub run's steps 1 to
3, the `known_hosts` part of step 4 and step 6, then sources
`real-claude.sh` in place of the stub's remaining checks.

Environment:

- Host: macOS (Darwin 25.4.0, arm64), Docker Desktop 28.3.2; the
  host's own `claude --version` is 2.1.289.
- Image: `debian:trixie-slim`, aarch64, kernel 6.10.14-linuxkit; sudo
  1.9.16p2, git 2.47.3, OpenSSH 10.0p2; Claude Code 2.1.289,
  linux-arm64, SHA-256 `d100d5e4...f84f28`.
- Binaries: built from the tree of commit `82c30978`, which adds
  `--real-claude`. It changes no Go code, so the server and daemon are
  those of its parent, `301536e0`.
- Debian's `/etc/sudoers` defaults, printed by the check:

      Defaults	env_reset
      Defaults	mail_badpass
      Defaults	secure_path="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
      Defaults	use_pty

- The daemon's `PATH`: `/usr/local/bin:/usr/bin:/bin`.
- Model: the server's default, `haiku`, which Claude Code reported as
  `claude-haiku-4-5-20251001`.

Five Claude Code runs. The task letters follow the earlier note's;
its task B, which tested step 10, has no counterpart here (see "Step
11 with claude" below).

| Run | How | Prompt | Ends by |
| --- | --- | --- | --- |
| step 4 | `sudo -u orch-agent -i claude -p ... --model haiku --output-format json`, as root | reply ok | its answer |
| A | task through the server | write, commit, print `PATH` | the turn's end |
| C | task through the server | run `tail -f /dev/null` | `kill -TERM <sudo pid>`, as `orchestrator` |
| D | task through the server | run `tail -f /dev/null` | a `stop` command through the server's API |
| E | task through the server | run `tail -f /dev/null` | the daemon killed with SIGKILL and started again |

Before D, the check adds `/etc/sudoers.d/orchestrator-path` with
`Defaults!/usr/local/bin/claude !secure_path`, which exempts `claude`
from `secure_path`. So C runs under the README's rule, and D and E
under the rule with that exemption.

The held turns run `tail -f /dev/null` because Claude Code's Bash tool
refuses a long `sleep`. The first run asked for `sleep 301`; the model
called Bash with that command, no `sleep` process appeared, and the
turn ended with exit code 0. The 2.1.289 binary contains the text
"Long leading \`sleep\` commands are blocked", so the refusal comes
from Claude Code; the first run did not print the tool result, so its
exact message was not seen.

## Results

All 49 assertions passed in the third run, which this note quotes.
Task ids are cut to their first four characters, and `.../` stands for
`/srv/orchestrator/workspaces/`. The first run failed the three held
tasks (the `sleep` above), and the second failed the three `PATH`
assertions because root in the container cannot read another user's
`/proc/<pid>/environ` (Docker drops `CAP_SYS_PTRACE`); the check now
reads it as `orch-agent`. Both failures were in the check script.
The second run's other results matched the third's, except task E's
`harness_exited` (see "A restarted daemon and the claude its previous
run left").

### Step 4: the login, found under sudo

    # login: 600 orch-agent /home/orch-agent/.claude/.credentials.json on tmpfs, keys ["accessToken","expiresAt","rateLimitTier","scopes","subscriptionType"]
    # as root: sudo -u orch-agent -i claude -p 'Reply with the single word ok.' --model haiku --output-format json
    #   {"type":"result","subtype":"success","is_error":false,"result":"ok","total_cost_usd":0.0023969,"models":["claude-haiku-4-5-20251001"]}
    # ~orch-agent after the first run: .bash_logout .bashrc .cache .claude .claude.json .profile .ssh
    # ~orch-agent/.claude.json: {"hasCompletedOnboarding":null,"oauthAccount":true,"keys":25}

So:

- The Linux build reads `~/.claude/.credentials.json` holding the
  macOS Keychain item's JSON, `{"claudeAiOauth": {...}}`, with the
  same field names. Without `refreshToken` it still used the access
  token. That file was the only login in the container, and the
  task's `system/init` line reports `"apiKeySource":"none"`.
- `claude -p` ran in a fresh home without any onboarding state: the image's
  `~orch-agent` holds only `.bash_logout`, `.bashrc` and `.profile`.
  Claude Code wrote a `~/.claude.json` with 25 keys, among them
  `oauthAccount`; `hasCompletedOnboarding` was absent or null.
- The login was found by `claude` started through `sudo -u orch-agent
  -i` and, in tasks A to E, through the daemon's `sudo -n -u orch-agent
  -D <workspace> --`, with sudo's reset environment. The checklist's
  step 5 asks the same thing on macOS, where the login is in the
  Keychain; this run does not answer it.

The checklist's step 4 logs in with `/login` in an interactive
`claude`; this run copies an existing login instead, so the
interactive `/login` as `orch-agent` is not covered.

### Step 7 with claude, and the permission gateway

Task A's prompt asked for one Bash command:

    printf "hello from the harness user\n" > hello.txt && git add hello.txt && git commit -q -m "Add hello.txt" && git log -1 --format=%H; echo "PATH=$PATH"; id -un; which git node rg; true

Events, from `GET /v1/tasks/{id}/events`:

    # harness_started: {"pid":289,"model":"haiku","workdir":".../PFRK..."}
    # init: {"claude_code_version":"2.1.289","model":"claude-haiku-4-5-20251001","permissionMode":"default","apiKeySource":"none","cwd":".../PFRK...","tools":34,"mcp_servers":[{"name":"orchestrator","status":"connected","source":"dynamic"}]}
    # permission_requested:
    #   {"request_id":"83b23c4d-...","tool":"Bash","input":"{\"command\":\"printf ..."}
    # tool results:
    #   is_error=false
    #   5ed403844900e1f73d29ae5e06f7ff16dcfb61a1
    #   PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
    #   orch-agent
    #   /usr/bin/git
    # result: {"subtype":"success","is_error":false,"num_turns":2,"duration_ms":5346,"total_cost_usd":0.0220224,"result":"done"}
    # branch_pushed: {"branch":"orchestrator/PFRK...","commit":"5ed40384...","ahead":1,"uncommitted":0,"error":""}
    # harness_exited: {"exit_code":0}

and in the bare repository, as `git`:

    refs/heads/orchestrator/PFRK... = 5ed40384...
    commit 5ed4038 by orchestrator <orchestrator@localhost>: Add hello.txt

The daemon's MCP gateway serves each task's permission tool, which
`claude` calls through `--permission-prompt-tool` for every tool call
that needs approval (README.md, "Running the harness as another
user"). Its server connected from the harness user's `claude`
(`"status":"connected"`). The Bash call went to that permission tool:
the daemon journaled `permission_requested` for `Bash`, the daemon log
shows the server's `answer_permission` command applied, and the tool
ran (`is_error=false`). `hello.txt` in the workspace is owned by
`orch-agent`. The daemon pushed the agent's commit as in the stub run.

`which git node rg` found only `/usr/bin/git`. The native build ran
without `node` installed. `rg` is not on the `PATH`; whether Claude Code's Grep tool
works without it was not tested (**UNVERIFIED** that the native build
carries its own ripgrep).

### The PATH under secure_path

The Bash tool's `echo "$PATH"` in task A printed Debian's
`secure_path`, not the daemon's `PATH`, although the README's rule
lists `PATH` in `env_keep`. The check reads `PATH` from
`/proc/<pid>/environ` of `claude` and of the Bash tool's command:

    task C, the README's rule:
    # PATH: claude /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin; tail -f /dev/null /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

    # /etc/sudoers.d/orchestrator-path: Defaults!/usr/local/bin/claude !secure_path
    task D, with that line:
    # PATH: claude /usr/local/bin:/usr/bin:/bin; tail -f /dev/null /usr/local/bin:/usr/bin:/bin

`visudo -c` accepted the line. With it, `claude` and the commands its
Bash tool runs get the daemon's `PATH`, as `env_keep` asks; the Bash
tool passes `claude`'s `PATH` on unchanged in both cases. So when
sudoers sets `secure_path`, it replaces the daemon's `PATH` for
`claude` and `env_keep` does not stop it, and a command-specific
`!secure_path` lifts that for `claude` alone. The run did not need
the exemption: `claude` is a native binary at an absolute path, and
`git` is in `/usr/bin`, which `secure_path` includes, so
`test/harness-user/sudoers` keeps the README's rule without it. A
`claude` installed through npm, which runs through `node`, or tools
installed outside `secure_path` would need it (**UNVERIFIED**: not
run).

The exemption names `claude` only. In a separate container of the
same image, as `orchestrator`, `sudo -n -u orch-agent -D / --
/usr/bin/git -c 'alias.p=!echo $PATH' p` printed
`/usr/lib/git-core:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`
both without and with the line: git keeps `secure_path`, after
prepending its own directory. That did not matter here, since git,
ssh and rm are all in `/usr/bin`.

### Step 8: the process tree

With task C held:

    USER           PID  PPID STAT COMMAND
    root           604   129 Ss   sudo -n -u orch-agent -D .../ZPEH... -- /usr/local/bin/claude -p --input-format stream
    orch-agent     607   604 Sl   /usr/local/bin/claude -p --input-format stream-json --output-format stream-json --verbose --forward-subagent-text --model haiku --
    orch-agent     693   607 Ss   /bin/bash -c source /home/orch-agent/.claude/shell-snapshots/snapshot-bash-1791550794708-uwnttr.sh 2>/dev/null || true && shopt -u
    orch-agent     694   693 S    tail -f /dev/null

`claude` runs as `orch-agent`, its parent the sudo pid that
`harness_started` reports. The Bash tool runs each command in a
`bash -c` child of `claude`, in a session of its own (`Ss`), after
sourcing a snapshot of the harness user's shell from
`~/.claude/shell-snapshots`.

### Step 11 with claude: SIGTERM to sudo from the daemon's user

    # as orchestrator: kill -TERM 604
    ok - step 11: claude (pid 607) exited, 1.0s after the signal
    ok - step 11: the Bash tool's tail -f /dev/null (pid 694) ended, 1.0s after the signal
    ok - step 11: sudo (pid 604) is gone
    # harness_exited: {"exit_code":143}

sudo relayed SIGTERM; `claude` exited with 143 within a second and its
Bash tool's command ended with it, although that command was in a
session of its own. sudo exited with `claude`'s status. The check
polls every 0.5 s, so the times are upper bounds to that resolution.
The second run measured 1.5 s.

In the check's output "after the signal" counts from the event that
ends the turn: the SIGTERM here, the stop command below, and the
daemon's SIGKILL in the restart case.

Step 10 sends `claude` SIGTERM from `orch-agent` rather than through
sudo. It was not run with `claude`. **UNVERIFIED:** that `claude`
ends the same way, since the signal it receives is the same; the stub
run showed that sudo exits when its command does.

### The daemon's stop

With the exemption in place, `POST /v1/tasks/{id}/commands
{"kind":"stop"}` for held task D:

    ok - stop: claude (pid 867) exited, 1.5s after the signal
    ok - stop: the Bash tool's tail -f /dev/null (pid 945) ended, 1.5s after the signal
    # stop to harness_exited event seen: 1.6s; harness_exited: {"exit_code":1}
    level=INFO msg="process ended" task=EKLT... exit_code=1 paused=false cut_short=false stopped=true

A stop sends the harness an interrupt and then closes its input
(`Task.Stop` in `internal/daemon/task.go`). The held stub read
neither and was killed after the 30 s timeout (the [earlier
note](2026-10-09-harness-user-container.md), "The daemon's Kill on
stop"). `claude` ended the Bash tool's command and exited within
1.5 s, here with exit code
1 and nothing on stderr (the `harness_exited` payload has no
`stderr` field). Which of the interrupt and the closed input made it
exit was not separated. The daemon's `Kill` after the 30 s shutdown
timeout was therefore not reached with `claude`; the SIGTERM that `Kill` sends
sudo is what step 11 sent. The daemon then deleted the workspace
through `rm` as `orch-agent`.

### A restarted daemon and the claude its previous run left

With task E held, the check killed the daemon with SIGKILL:

    # 2 s after the daemon's SIGKILL claude (pid 1123) still runs
    level=INFO msg="harness left by the previous daemon is still running; waiting for it to exit" task=2MQR... pid=1120 wait=30s
    level=WARN msg="harness left by the previous daemon did not exit; killed it" task=2MQR... pid=1120 after=30.005s
    ok - restart: claude (pid 1123) exited, 32.8s after the signal
    ok - restart: the Bash tool's tail -f /dev/null (pid 1203) ended, 32.8s after the signal
    # harness_exited: {"exit_code":143}

`claude` did not exit when the daemon died, although its input reached
its end and its output lost its reader, at least while a tool ran. It
ran on until the restarted daemon sent sudo SIGTERM 30 s later, then
ended with its tool's command. The second run's last `harness_exited`
for the same case was `{"exit_code":-1,"error":"daemon restarted during
the turn"}` rather than 143. The check prints only the last
`harness_exited` event, so it is **UNVERIFIED** whether each run
journaled both events and only their order differed.

## Spend

The `total_cost_usd` that `claude` reported, with the owner's
subscription login. **UNVERIFIED:** how this figure relates to what a
subscription is charged; the runs used the subscription's quota.
Why step 4's figure fell between the first and third runs was not
investigated.

| Run | step 4 | A | C | D | E | Recorded |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | 0.0157 | 0.0505 | not recorded | not recorded | not recorded | 0.0662 |
| 2 | 0.0156 | 0.0217 | no result | 0.0185 | no result | 0.0558 |
| 3 | 0.0024 | 0.0220 | no result | 0.0175 | no result | 0.0419 |

Fifteen `claude` runs in all. A turn ended by a signal reports no
result, so its cost is not shown; the first run's held turns ended
normally but the check did not record their cost then.

## Not covered

- Step 5 and the macOS forms of steps 1 and 3: the run is on Linux.
- An interactive `/login` as `orch-agent` (step 4 copies a login).
- Step 10 with `claude` itself (see "Step 11 with claude").
- The daemon's `Kill` after the shutdown timeout with `claude`, which
  exits on the stop before it.
- A refresh of the login in the container: the access token was valid
  for the whole run, and the container had no refresh token.
- A follow-up turn with `--resume`, mTLS, and the start-up deletion of
  unknown workspaces, which the earlier note does not cover either.

## Running it

From the repository root, with Docker running, on macOS with a Claude
Code login in the Keychain:

    test/harness-user/run.sh --real-claude [OUTDIR]

The first build downloads Claude Code (245 MB; 284 s on the host
above); later
builds use Docker's cache. In the quoted run the daemon's log spans
60 s, from its start to the end of the restart check, which waits
30 s.
