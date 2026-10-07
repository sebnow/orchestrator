# MVP smoke test through the GUI: findings (2026-10-08)

The server (`cmd/server`) and the client daemon (`cmd/daemon`) were run
together on one machine against the real Claude Code CLI. Every action
went through the owner's GUI routes, served by `internal/server/gui.go`
and `internal/server/gui_task.go`, as a browser with JavaScript off
would send it. Four tasks covered a single turn, a permission prompt that
the owner denies, a pause and resume, and a task that clones a
repository. This note records what the pages showed, the one fix the
run needed, and where the system differed from what the GUI and the
[README](../../README.md) describe.

Markers: **UNVERIFIED** flags a claim not observed in this run and not
confirmed in a primary source, including inferred causes. Everything
else was observed in the run below or read from the source files named
where it is used.

Terms:

- The "owner" is the person who runs the server and uses the GUI: they
  start tasks, answer permission requests, and pause, resume or stop
  tasks.
- The "harness" is the agent CLI the daemon runs for each task, here
  Claude Code. A "model session" is one `claude` process started for a
  task.
- A "message" is one line of Claude Code's stream-json stdout, named by
  its `type` and `subtype`, such as `system/thinking_tokens`. The
  daemon sends each one to the server as a `harness_output` event.
- An "event" is what the daemon reports about a task, numbered by its
  "seq", counting from 1 per task. The daemon also keeps them in a
  per-task "journal" until the server has acknowledged them.
- A "command" is what the owner issues to a task: start, answer a
  permission request, pause, resume, prompt, interrupt or stop.
  Commands are numbered across all tasks.
- The "gateway" is the daemon's MCP server. It serves the `permission`
  tool, which Claude Code calls to ask whether it may run a tool, and
  the `acknowledge_pause` tool, which the agent calls with a "stop
  note" saying where it stopped.
- A pause has two limits, set by the new-task form's `acknowledge` and
  `cleanup` fields: the "acknowledgement limit", from the pause request
  to the agent's `acknowledge_pause` call, and the "cleanup limit", from
  that call to the end of the turn. When a limit expires, the daemon
  interrupts the turn.
- A "task page" is `GET /tasks/{task}`; the "dashboard" is `GET /`; the
  "raw page" is `GET /tasks/{task}/raw`, the task's stored events.
- A "badge" is the task state the header and the dashboard show:
  `running`, `awaiting permission`, `pausing`, `paused`, `finished`,
  `stopped` or `failed`.
- An "entry" is one item of a task page's transcript.
- The "stream" is `GET /tasks/{task}/stream`, the server-sent events
  that keep a task page live. Each event has an `id:` of the form
  `<seq>-<command id>`: the highest event seq and command id the page
  has seen, so a new command raises the second number under the same
  seq. An event's data holds new entries and, as an htmx out-of-band
  swap, a fresh task header (a "header swap").
- `soxmknwn` and `vrlzmopn` are jujutsu change ids in this repository.

## Method

Environment: the project owner's macOS workstation (Darwin 25.4.0,
arm64), Claude Code 2.1.289 logged in with a Claude subscription, git
2.55.0. Every task used the server's default model, `haiku`.

The binaries were built from change `soxmknwn` with
`nix develop -c go build -o /tmp/m7smoke.YRe0/bin/<name> ./cmd/<name>`
and started as:

    server -listen 127.0.0.1:18080 -db /tmp/m7smoke.YRe0/db/orchestrator.db
    daemon -server http://127.0.0.1:18080 -id laptop \
        -state-dir /tmp/m7smoke.YRe0/state \
        -claude /tmp/m7smoke.YRe0/bin/claude-counting

`claude-counting` is a shell script that appends the time and its
arguments to `/tmp/m7smoke.YRe0/claude-invocations.log` and then runs
the real `claude`. The log shows 6 invocations: 2 `claude --version`,
one per daemon start, and 4 model sessions. A `claude --version` run by
hand before the daemon started, outside the wrapper, is not in the log.

Each request was made with curl as a plain form post or get, without
the `HX-Request` header, so every successful POST answered
`303 See Other` with a redirect to the page showing its outcome. New
tasks were posted to `POST /tasks` with the fields of the new-task form
(`prompt`, `repo`, `ref`, `model`, `daemon`, `acknowledge`, `cleanup`);
answers, pauses and resumes were posted to `POST /tasks/{task}/commands`
with the fields of the task page's forms. The scripts polled the task
page every one or two seconds until the expected entry appeared.

The pause scenario used the three-step task of the
[graceful pause spike](2026-10-07-graceful-pause-spike.md), Method:

> Use the Bash tool to run `ping -c 5 127.0.0.1` three times, one call
> after another, never in parallel and never in the background. After
> each call finishes, write the single word DONE-1, DONE-2 or DONE-3
> before starting the next call. When all three are done, reply with
> exactly FINISHED.

| Scenario | Prompt | Owner actions |
| --- | --- | --- |
| one turn | "Reply with the single word MARMALADE." | none |
| permission | "Use the Bash tool to run `touch smoke-denied.txt`, then reply with one line saying whether it succeeded and why." | deny, with the reason "Denied by the owner during the smoke test." |
| pause | the three-step task above; acknowledgement limit 90 s, cleanup limit 1 m | allow the first ping; pause 1 s later; resume once paused; allow the remaining pings |
| workspace | "Use the Read tool to read the file README in the current directory, then reply with its first line only.", repository `https://github.com/octocat/Hello-World`, ref `master` | allow any permission request (none came) |

The daemon was rebuilt from change `vrlzmopn` and restarted once,
between the first and second workspace attempts, to pick up the fix
described under "Workspace clones read the owner's git configuration".
Each daemon was shut down with SIGTERM while no turn was running.

The pages, the stream capture, the logs, the invocation log and the
server's SQLite database are in `/tmp/m7smoke.YRe0`, outside the
repository; quotes below come from there. The daemon had deleted its
journals by the end, so message counts come from the server's `events`
table. Times in this note are UTC; the pages render local time, UTC+2,
so the stream sample below shows `00:19:11` for 22:19:11 UTC.

## Findings

### One turn

Task `DKCOLJKO2KLCPXUKUNFJFPQ7YD`. Once the turn had ended, the task page
showed an "Agent" entry with `MARMALADE`, an entry "Turn ended: success"
with "stop reason end_turn, 1 turns, 3.455s, $0.0161 so far", and the
header cost `$0.0161`. The badge read `running` (see "A task stays
running between turns"). The dashboard row showed `running`, `laptop`,
`haiku`, `$0.0161` and the last activity, 22:18:16. The raw page listed
8 `harness_output`, 1 `harness_started` and 1 `quota_observed` events.

### Permission prompt and denial

Task `GZXGBZ23HXFD6IWZXFZ5X2IASK`. At 22:18:40 the page showed the
`awaiting permission` badge and a card "Permission requested", "The
agent asks to run `Bash`", with the input
`"command": "touch smoke-denied.txt"`. The dashboard's "Needs attention"
list named the task with the reason "asks to run Bash".

After the denial was posted at 22:18:47, the card was gone and the
transcript showed "Owner denied request eacc0006-…" with the reason, a
"Tool failed" entry with the same text, and the agent's reply: "The
command was denied by the owner during the smoke test—your permission
settings explicitly block this file creation." The turn ended at
22:18:49 with "2 turns, 13.627s, $0.0196 so far". The attention list
went back to "Nothing needs attention." The task's working directory,
`/tmp/m7smoke.YRe0/state/workspaces/GZXGBZ23HXFD6IWZXFZ5X2IASK`, held
no `smoke-denied.txt`, and neither did the rest of the state directory.

### Pause and resume

Task `5GXOIGCKI43V2PL5HR3BPFG6PJ`. The first ping was allowed at
22:19:17.6 and the pause posted at 22:19:18.6. The next page showed the
`pausing` badge, and the follow-up form's button was disabled with "The
task is pausing; prompts open again once it has paused."

The agent called `ToolSearch` at 22:19:24, then
`mcp__orchestrator__acknowledge_pause` at 22:19:26, 7.5 s after the
pause. Its stop note: "Completed first ping command; need to run second
and third ping calls sequentially before finishing." It wrote `DONE-1`,
the turn ended at 22:19:27.9 ("4 turns, 16.411s, $0.0282 so far"), and
an entry "Pause took effect" followed. The badge read `paused` and the
header offered Resume. The attention list named the task with "paused:
Completed first ping command; …".

The agent ended its turn within both limits, without an interrupt: the
server stored no `control_response` for the task until the daemon's
shutdown at 22:23:39.

The resume was posted at 22:19:41. The transcript showed "Owner resumed
the task", two more ping requests, allowed at 22:19:45 and 22:19:51,
`DONE-2`, then `DONE-3` and `FINISHED`, and the turn ended at 22:19:57
with "$0.0388 so far".

The stream, read with `curl -N` from `after=0-0` for the whole task,
carried 55 events and 3 `: keepalive` comments. The ids ran `1-4`,
`2-4`, `3-4`, `3-4`, `5-4` … `11-4`, `11-5`, `11-6` … `53-9`. The
command part rose with each command: 4 the start, 5 the first answer, 6
the pause, 7 the resume, 8 and 9 the later answers. Counting the badges
in successive header swaps: `running` (9), `awaiting permission` (1),
`running` (1), `pausing` (19), `paused` (2), `running` (6), `awaiting
permission` (1), `running` (7), `awaiting permission` (1), `running`
(8). The first event's data begins:

    id: 1-4
    data: <li class="entry from-owner"><header><time datetime="2026-10-07T22:19:11.264517Z">2026-10-08 00:19:11</time> Owner prompted</header><pre>
    data: Use the Bash tool to run `ping -c 5 127.0.0.1` three times, …

The curl ended on its own when the server shut down.

### Workspace clones read the owner's git configuration

The first workspace task, `VUJSCQHAASERZRYCXBUNFYALHS`, failed before
any `claude` process started. Its page showed the `failed` badge and
"Harness exited with code -1", "prepare workspace: git clone: exit
status 128: git@github.com: Permission denied (publickey)." The
attention list gave the same reason.

The daemon ran git with the environment it inherited, so git read the
owner's configuration. `~/.config/git/config.local` holds
`url.ssh://git@github.com/.insteadof=https://github.com/`, which turned
the https URL into an ssh one, and the ssh login failed. The system
configuration holds `credential.helper=osxkeychain`. **UNVERIFIED:**
that this helper would have supplied the owner's keychain credentials to
a clone that asked for them; no clone in this run did. Either setting
contradicts the comment on `prepareWorkspace` in
`internal/daemon/workspace.go`, which says git runs "with no credentials
of the daemon's", citing the
[task credentials](../adr/2026-10-07-task-credentials.md) ADR.

The fix, change `vrlzmopn` ("internal/daemon: clone workspaces without
the owner's git config"), adds `GIT_CONFIG_GLOBAL=/dev/null` and
`GIT_CONFIG_NOSYSTEM=1` to git's environment, with a test that a global
`insteadOf` rule no longer redirects the clone. `GIT_CONFIG_NOSYSTEM`
makes git skip the system configuration file entirely
([git(1)](https://git-scm.com/docs/git#Documentation/git.txt-GITCONFIGNOSYSTEM)),
so a setting a clone needs there, such as a CA bundle in
`http.sslCAInfo`, is skipped too. This machine's system configuration
held only the credential helper, and
`GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git ls-remote https://github.com/octocat/Hello-World`
succeeded.

**UNVERIFIED:** whether a repository given by an explicit `ssh://` URL
still authenticates with the owner's ssh agent or keys. The fix leaves
`GIT_SSH_COMMAND` and the ssh configuration alone.

### Workspace task

After the fix, task `ABGLZ4HC37I4TMEGI6654HMPD7` started in
`/tmp/m7smoke.YRe0/state/workspaces/ABGLZ4HC37I4TMEGI6654HMPD7`, a
shallow clone whose `git log -1` reads `7fd1a60 (grafted, HEAD ->
master, origin/master) Merge pull request #6 from Spaceghost/patch-1`
and whose origin is `https://github.com/octocat/Hello-World`. The agent
called Read, which returned `1	Hello World!` without asking for
permission, and replied `Hello World!`. The turn cost $0.0196.

### A task stays running between turns

After its turn ended, each task kept the `running` badge, and its
harness did not exit until the daemon shut down. The daemon closes
Claude Code's stdin only when it stops the task (`Task.Stop` in
`internal/daemon/task.go`). The server's state machine (`afterEvent` in
`internal/server/state.go`) marks a task `finished` when the harness
exits with code 0 and no stop was issued, and `stopped` when a stop was
issued; no stop was issued in this run.

At the first daemon SIGTERM, 22:23:39, the three tasks then idle each
got "Harness exited with code 0" and the `finished` badge; the workspace
task did the same at the second, 22:24:28. Both daemon runs exited with
code 0, and so did the server at its SIGTERM, 22:24:38. The final
dashboard listed four `finished` tasks, costing $0.0161, $0.0196,
$0.0388 and $0.0196, and the `failed` clone attempt at $0.0000.

### Messages the transcript does not recognise

Some messages render as "Unrecognised harness_output of type …"
entries, each with the raw JSON folded under "Payload". Counted on the
last page saved for each task:

| Task | `command_lifecycle` | `system/thinking_tokens` | `system/task_started` | `system/task_notification` |
| --- | ---: | ---: | ---: | ---: |
| one turn | 3 | 0 | 0 | 0 |
| permission | 3 | 0 | 0 | 0 |
| pause | 9 | 6 | 3 | 3 |
| workspace | 3 | 0 | 0 | 0 |

The `command_lifecycle` messages report `queued`, `started` and
`completed` for each stdin prompt. The `system/task_started` messages
name a Bash call, such as "First ping to localhost". Besides these, the
server stored one `control_response` per task, each at its daemon's
shutdown. `Task.Stop` interrupts the turn before it closes stdin, so
these are the interrupt receipts. No page was saved after they arrived.

### Repeated stream ids

Two ids came twice in a row: `3-4` and `35-7`. Each first event carried
a `command_lifecycle` entry; each repeat carried only a header swap. The
event after each, seq 4 and seq 36, was `system/init`, which adds no
entry. **UNVERIFIED:** that a stored event which adds no entry causes a
header-only update under the unchanged cursor; the stream code was not
traced.

### ToolSearch before `acknowledge_pause`

Before acknowledging the pause the agent called `ToolSearch`, which
returned `[tool_reference]`. **UNVERIFIED:** that Claude Code 2.1.289
defers the gateway's tools, so that the agent must search for
`acknowledge_pause` before it can call it. The earlier run recorded in
[daemon live findings](2026-10-07-daemon-live-findings.md) records no
such call.

### The database's directory must exist

Started with `-db` in a directory that did not exist, the server exited
with code 1 and "open database: unable to open database file (14)". The
flag's help says the file is "created when missing"; its directory is
not. Change `znnzwqnp` added this to the README's "Running" section.

### The daemon's harness before its first event

On the dashboard, the Harness cell of daemon `laptop` was empty after
the daemon connected and before its first task. The server records a
daemon's harness from the latest event in each batch it receives
(`appendEvents` in `internal/server/store.go`). The final dashboard
showed `claude-code 2.1.289`.

## Open

- Whether a task should end once its turn ends rather than wait for a
  follow-up: the [harness integration](../adr/2026-10-07-harness-integration.md)
  ADR relies on follow-ups over stdin in the same process, and each idle
  task keeps its `claude` process until it is stopped or the daemon
  shuts down.
- Whether the transcript should render or hide `command_lifecycle`,
  `system/thinking_tokens`, `system/task_started` and
  `system/task_notification`.
- **UNVERIFIED:** the cause of the repeated stream ids; see "Repeated
  stream ids".
- **UNVERIFIED:** whether an explicit `ssh://` repository URL clones
  with the owner's ssh credentials; see "Workspace clones read the
  owner's git configuration".
- **UNVERIFIED:** why the agent searched for `acknowledge_pause` before
  calling it.
- The `stopped` path was not exercised: no task was stopped from its
  page.
