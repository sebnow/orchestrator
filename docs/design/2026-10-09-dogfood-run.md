# Dogfood run: a brain delegates a README fix to a worker (2026-10-09)

This note records a run in which the orchestrator's own server, daemon
and GUI carried out a task on this repository. A `brain` agent took the
owner's prompt, spawned a `worker` agent, received its hand-back and
reported. The owner is the person who starts tasks and reads their
results ([owner authentication](../adr/2026-10-08-owner-authentication.md)).
The live tests in `cmd/daemon/live_test.go` give the agents one-line
prompts with fixed answers; this task asked for a change to the README.
The note records the setup, what happened, and what broke, confused or
looked wrong; it leaves the fixes to later changes.

Markers: **UNVERIFIED** flags a claim not checked against the run's
events, logs, pages or the code. Code references are at commit
`27de064d`. The run's database, event dumps, screenshots and scratch
clone were kept under `/tmp/dogfood/` on the owner's workstation, not
in the repository; seq numbers below are the `seq` of each task's
events from `GET /v1/tasks/{id}/events`.

## Setup

- Host: macOS 26.4.1 on arm64, the owner's workstation, single-user
  mode (no `-harness-user`). Claude Code 2.1.289 at
  `/Users/sebnow/.nix-profile/bin/claude`; the model `sonnet` resolved
  to `claude-sonnet-5-5`. Go 1.26.8 from the dev shell, git 2.55.0,
  Docker 28.3.2.
- Binaries: `cmd/server` and `cmd/daemon` built from commit `27de064d`
  ("docs/design: record the credential substitution research").
- Server: `-insecure-loopback -listen 127.0.0.1:8091 -db
  /tmp/dogfood/server.db -permissions allow-all -default-model sonnet`,
  on port 8091 because the owner's own server and daemon were using
  8080.
- Daemon: id `dogfood`, `-state-dir /tmp/dogfood/daemon-state -claude
  /Users/sebnow/.nix-profile/bin/claude`, with the shell's
  `SSH_AUTH_SOCK`. `-claude` puts `claude` on the daemon's command
  line, which reproduces the problem the task's prompt describes.
- Git remote: a Debian trixie container that `test/dogfood/remote.sh`
  builds and starts, with sshd and a bare repository at
  `/orchestrator.git`, port 22 on `127.0.0.1:2222`, URL
  `ssh://git@localhost:2222/orchestrator.git`. The `git` user's
  `authorized_keys` holds the first public key that `ssh-add -L` lists.
  `main` was seeded with `27de064d`. The container's ed25519 host key
  went into `~/.ssh/known_hosts` as `[localhost]:2222`, fingerprint
  `SHA256:fvmR0kVrFxqCQXXqUCTnNknXBSHTTsNxViJgoX9l/To`, checked against
  `ssh-keygen -lf` inside the container.
- Agents: `test/dogfood/agents/brain.json` and `worker.json`, created
  with `POST /v1/agents`, both `model: sonnet`. Their system prompts
  adapt the owner's Claude Code agent files from the `sebnow` plugin
  0.2.0, under
  `~/.claude/plugins/marketplaces/sebnow/plugins/skills/agents/`:
  - `brain`, tools `spawn_task` and `send_message`, from `brain.md`. It
    keeps the parts on deciding, protecting the brain's context,
    briefing with pointers and judging reports. The changes:
    - an interactive user became an owner who reads only the final
      reply;
    - Claude Code's subagents became child tasks started with
      `spawn_task`, as an agent from the list in its instructions;
    - waiting for a worker became ending the turn;
    - resuming a worker with Claude Code's `SendMessage` became
      `send_message` to the child's task id;
    - the reporting section was cut to one paragraph.
  - `worker`, no tools, from `senior.md`, with three additions: from
    `junior.md`, that a line number in the brief only helps find the
    text it names; that it commits on the branch it is on and does not
    push; and that its report gives the commit id. It reports as
    Changed, Decisions, Verification, Open.
- Task: started through the GUI form (`POST /tasks`), agent `brain`,
  repository as above, ref `main`, every other field blank, with this
  prompt: "README.md, section 'Running the harness as another user',
  checklist step 8: the command `ps -o user,pid,ppid,command -ax | grep
  claude` also matches the daemon itself because of its `-claude` flag.
  Have a worker change the step so the command matches only the harness
  process, verify the command on this machine, and commit. Report what
  was changed."
- Capture: `/v1/tasks/{id}` and `/v1/tasks/{id}/events` with curl, and
  screenshots with the dev shell's headless Google Chrome 154, each
  beside the page's HTML.

## Timeline

One attempt; nothing was interrupted. Times are local (UTC+2). The
brain is task `ZNXRYD54S7DAIT5KBAVHJJ7VKT`, the worker
`W6XWZZSZG5QZFTD6BSV36GWHAK`.

| Time | Task | Event |
| --- | --- | --- |
| 17:36:38 | brain | created from the form; harness started, pid 90013 (seq 1) |
| 17:36:47 | brain | called `spawn_task` with agent `worker` and a brief (seq 5); result "Started child task W6XW…" (seq 8) |
| 17:36:48 | worker | harness started, pid 90253, whose parent 88686 is the daemon (seq 1) |
| 17:36:49 | brain | replied that it handed the work off and ended its turn, $0.0874 (seq 9, 10) |
| 17:36:50 | worker | read step 8 and ran the old command (seq 5, 9) |
| 17:36:57 | worker | called a tool `bash`, refused as unknown (seq 15, 16) |
| 17:36:59 | worker | ran the candidate pattern; one line, the harness (seq 17, 21) |
| 17:37:04 | worker | edited README.md, ran the new command and committed `eb69b7b`, all in one Bash call (seq 25, 27) |
| 17:37:11 | worker | final reply in the asked shape; turn ended, $0.0956 (seq 28, 29) |
| 17:37:13 | worker | daemon pushed `orchestrator/W6XWZZSZG5QZFTD6BSV36GWHAK` at `eb69b7b37fad`, 1 commit ahead, 0 uncommitted (seq 31); the page shows the reply handed back to the parent |
| 17:37:13 | brain | harness started again, resumed by the hand-back, which the page shows as "Report from child W6XW…" (seq 13) |
| 17:37:20 | brain | final reply to the owner; turn ended, $0.1028 for the session (seq 17, 20) |
| 17:37:21 | brain | harness exited (seq 22) |

From the form to the brain's last exit took 43 s. The daemon started
`claude` three times, twice for the brain and once for the worker
(daemon log, "process started"). The task pages report costs as API
list-price equivalents; the owner's subscription is not billed per
token. Brain $0.1028 (turn 1 $0.0874, turn 2 $0.0154), worker $0.0956,
$0.1984 in all. The subscription's five-hour usage window, as the
dashboard's Budget section showed it, stayed at 22 %.

Screenshots, in `/tmp/dogfood/shots/`: `00-dashboard.png` before the
task, `03-brain-final.png` and `04-worker-final.png` (the task pages
after the run, with the child row, the hand-back and the pushed
branch), and `05-dashboard-final.png`. `01-agents.png` shows the
Agents page; the attempt at `02`, a task page during the run, wrote
no image (finding 13).

## The pushed branch

Fetched into a scratch clone with `git fetch
ssh://git@localhost:2222/orchestrator.git
'refs/heads/orchestrator/*:refs/remotes/dogfood/*'`. The remote had
`main` at `27de064d` and `orchestrator/W6XWZZSZG5QZFTD6BSV36GWHAK` at
`eb69b7b37fad09fd0170733cbb1f53dbc1502ae7`. The brain committed
nothing, so its branch was never pushed. The commit's author and
committer are `orchestrator <orchestrator@localhost>`, unsigned:

    README: match only the harness process in the run-as-user checklist

    The old 'grep claude' also matched the daemon via its -claude flag.

    Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>

```diff
--- a/README.md
+++ b/README.md
@@ -308,8 +308,11 @@ of steps 1 and 3 stay manual.
    with the user, the git and rm paths, and `ssh_agent=true`.
 7. Start a task with a repository and a prompt that commits a file.
    The task page shows the branch pushed.
-8. `ps -o user,pid,ppid,command -ax | grep claude` shows `claude` run
-   by `orch-agent`, its parent a `sudo` process.
+8. `ps -o user,pid,ppid,command -ax | grep -E '[0-9] +[^ ]*/claude -p '`
+   shows `claude` run by `orch-agent`, its parent a `sudo` process. The
+   pattern needs the harness's `/claude -p` command line, so it skips
+   the daemon, whose `-claude <path>` flag ends its line, and the
+   `grep` itself.
 9. In `sudo -u orch-agent -i`, `cat` the daemon's key and `ls` its
    state directory fail with "Permission denied". Ask the agent to run
    `ssh-add -l`: it lists the daemon's keys.
```

The commit was not merged into this repository; it existed only in the
container's repository, which was removed after the run.

## Findings

Severity is a guess: high blocks use, medium misleads the owner or an
agent, low is cosmetic or a gap for scripts, info is an observation.

1. **The parent is not told the child's branch (medium).** Neither the
   `spawn_task` result (brain seq 8) nor the hand-back names the
   child's branch or its push. The brain's report says the worker
   "committed it as `eb69b7b` on its own branch, and I haven't
   confirmed that commit is on `orchestrator/ZNXRYD54S7DAIT5KBAVHJJ7VKT`"
   (brain seq 17). That is the brain's own branch, which the daemon's
   instructions to the brain name; the brain never named
   `orchestrator/W6XW…`. The brain's page and its dashboard row show
   no branch (`03-brain-final.png`, `05-dashboard-final.png`); the work
   appears only on the child's page and row. An owner reading the
   brain's result must open the child to find what was delivered.
2. **The change hides the line step 8 relies on, and the reports
   overclaim (medium).** Step 8 says the harness's parent is a `sudo`
   process. The old command also listed the `sudo` process, so the
   reader could match its pid to the harness's ppid. The new pattern
   does not match that line. Run against the two `ps` lines in the
   [real-claude check](2026-10-09-harness-user-real-claude.md), section
   "Step 8: the process tree" (the `STAT` column dropped and the elided
   workspace path filled in), it prints only `orch-agent 607 604
   /usr/local/bin/claude -p …`, so the output no longer shows that 604
   is `sudo`. The worker also wrote that "the leading `[0-9] ` forces
   the match to start at the ppid column" (worker seq 28). The pattern
   is not anchored, and the line `sebnow 100 99 /bin/sh -c sleep 3
   /usr/local/bin/claude -p x` matches it. The brain repeated the claim
   as "it can't match text inside wrapper shell lines" (brain seq 17).
   The brain did flag that the command was not run under
   `-harness-user`, and asked the owner to run it there. Its prompt
   tells it to judge reports, but it did not test the worker's claims.
3. **The spawn result names a tool the child lacks (low).** The brain
   was told the child "works on its own and sends its result with
   send_message" (brain seq 8), though the `worker` agent allows no
   agent tools and reports by hand-back. The text is hard-coded in
   `spawnTask`, `internal/daemon/requests.go:87`. It did no harm here.
4. **A task's system prompt is on the harness's command line (low).**
   The worker's first `ps` (worker seq 9) captured its own `claude`
   command line, 2645 characters, with `--append-system-prompt` holding
   the whole composed system prompt and `--mcp-config` holding the
   gateway URL `http://127.0.0.1:60263/tasks/W6XW…/mcp`. The README's
   "Running the harness as another user" says the gateway URL shows in
   the process list; it does not say the system prompt does. The same
   output carried the command lines of the owner's other processes,
   including other Claude Code sessions, and is now stored in the
   server's database and shown on the worker's page. In single-user
   mode the agent can read those processes anyway.
5. **The form calls the repository field "https:// only" (low).** The
   GUI labels it "Repository (https:// only)"
   (`internal/component/task.go:432`), but the daemon accepts `ssh://`
   URLs and ssh addresses (`internal/daemon/workspace.go:73`; README,
   "Tasks"). This run's `ssh://` repository went through that form.
6. **The dashboard's Harness column starts empty (low).** Before any
   task it was blank, though the daemon had reported
   `harness=claude-code` and `harness_version=2.1.289` among its facts,
   the labels a daemon reports about its machine when it connects
   (README, "Placement"; `00-dashboard.png`). After the run it read
   "claude-code 2.1.289" (`05-dashboard-final.png`). The column shows
   the harness stored with the daemon's events
   (`internal/server/gui.go:200`), not the facts.
7. **Transcripts show raw Markdown (low).** Agent text is shown
   preformatted, with `**Changed:**` and backticks as typed
   (`03-brain-final.png`, `04-worker-final.png`). The worker's and
   brain's reports are long Markdown lists, so this is most of what
   the owner reads.
8. **Each allowed tool call is three entries (low).** Under
   `allow-all`, every Bash call appears as "Agent called Bash", "Agent
   asked to run Bash" and "Request … allowed by policy"
   (`04-worker-final.png`); the first two each repeat the tool's input
   in a collapsed block.
9. **Scripts cannot follow the tree (low).** `GET /v1/tasks/{id}` for
   the brain does not list its child tasks, the items of `GET
   /v1/tasks` carry no `parent_id`, and the brain's events hold neither
   the owner's prompt nor the hand-back it received; the worker's
   report survives there only as the brain's paraphrase. Only the task
   page shows "Report from child". The README's "Tasks" section
   describes the links between parent and children, and the messages,
   only for the task page.
10. **The agents work without the owner's conventions (info).** The
    harness runs with `--setting-sources project`, and the worker's
    init event lists only Claude Code's built-in plugins and skills
    (worker seq 4), none of the owner's. The repository has no
    `CLAUDE.md`. The commit uses the scope `README:`, where the
    README's "Version control" section asks for the file name,
    `README.md:`; neither agent read that section. **UNVERIFIED:**
    whether the owner's `~/.claude/CLAUDE.md` reaches a harness started
    with `--setting-sources project`; the init event does not say.
11. **The agents behaved as briefed, with small slips (info).** The
    brain spawned once and ended its turn without polling. Its brief
    to the worker named the section and the verification, and said
    what to show if no harness process was running when the worker
    checked the command. Its report asked the owner one question,
    whether to run the command under `-harness-user` (finding 2), with
    a recommendation. The worker called `bash` once and was refused
    (worker seq 16), then edited, verified and committed in a single
    Bash call without looking at the diff (seq 25). The brief asked for
    a `Co-Authored-By` trailer; **UNVERIFIED:** that the brain took it
    from Claude Code's own instructions.
12. **The server's cost is right for a resumed session (info).** The
    brain's second result reports `total_cost_usd` 0.1028 for the whole
    session: its `modelUsage` counts 1248 output tokens, the first
    turn's 472 plus the second's 776 (brain seq 10, 20). The server
    shows $0.1028; adding both results, $0.1902, would count the first
    turn twice. Most of the brain's first turn, $0.0874 for one tool
    call, went on 19702 tokens written to the one-hour prompt cache.
13. **No screenshot of the running stage (info, tooling).** Headless
    Chrome with `--virtual-time-budget` wrote no image of a task page
    within 60 s, and on the dashboard wrote one but did not exit;
    **UNVERIFIED:** that the task page's event stream and the
    dashboard's polling keep it running. With `--timeout` and a kill it
    worked, but by then the run had finished.

## Setup snags

None blocked the run, and no orchestrator code changed for it;
`test/dogfood/` is new with this note.

- The URL's path `/orchestrator.git` is absolute in the container, so
  the bare repository has to live at `/orchestrator.git`. A symlink
  there failed git's ownership check ("detected dubious ownership"),
  since root owns the link; the image moves the repository instead.
- `ssh-keyscan -p 2222 -t ed25519 localhost` printed a `#` comment line
  on standard output besides the key, so only the key line went into
  `known_hosts`.
