#!/bin/bash
# The harness-user container check. It runs as root inside the image
# Dockerfile builds, with the server and daemon binaries mounted at
# /opt/orchestrator/bin, and works through the steps of the checklist in
# README.md, "Running the harness as another user", that need no Claude
# Code login, with the claude stub at /usr/local/bin/claude. Each check
# prints "ok" or "not ok"; lines starting with "#" are evidence. It exits
# 1 when a check failed. Logs go to /out when it is mounted.
set -u

BIN=/opt/orchestrator/bin
DAEMON_PATH=/usr/local/bin:/usr/bin:/bin
OHOME=/home/orchestrator
STATE=$OHOME/state
PKI=$OHOME/pki
AGENT_SOCK=$OHOME/.ssh/agent.sock
WS=/srv/orchestrator/workspaces
SERVER_DIR=/var/lib/orch-server
API=http://127.0.0.1:8080
REPO=git@localhost:repo.git
BARE=/home/git/repo.git
MARKS=/tmp/stub-claude
DAEMON_LOG=/var/log/orch-daemon.log
SERVER_LOG=/var/log/orch-server.log
CLAUDE=/usr/local/bin/claude

passed=0
failed=0

ok() {
	passed=$((passed + 1))
	echo "ok - $*"
}

not_ok() {
	failed=$((failed + 1))
	echo "not ok - $*"
}

# check DESCRIPTION COMMAND... passes when COMMAND succeeds.
check() {
	local description=$1
	shift
	if "$@"; then ok "$description"; else not_ok "$description"; fi
}

# quiet COMMAND... runs COMMAND without its output.
quiet() {
	"$@" >/dev/null 2>&1
}

# jq_true ARGS... succeeds when jq's last output is true.
jq_true() {
	jq -e "$@" >/dev/null
}

# evidence prints its standard input as "#" lines.
evidence() {
	sed 's/^/#   /'
}

# as_orch runs a command as the daemon's user, with Debian's default PATH
# and nothing else of root's environment.
as_orch() {
	setpriv --reuid=orchestrator --regid=orchestrator --init-groups \
		env -i PATH=$DAEMON_PATH HOME=$OHOME USER=orchestrator LOGNAME=orchestrator "$@"
}

as_user() {
	local user=$1
	shift
	setpriv --reuid="$user" --regid="$user" --init-groups \
		env -i PATH=$DAEMON_PATH HOME=/home/$user USER=$user LOGNAME=$user "$@"
}

die() {
	echo "Bail out! $*"
	finish
}

finish() {
	if [ -d /out ]; then
		cp "$DAEMON_LOG" "$SERVER_LOG" /out/ 2>/dev/null
	fi
	echo "# daemon log:"
	evidence <"$DAEMON_LOG"
	echo "# passed $passed, failed $failed"
	if [ "$failed" -gt 0 ]; then exit 1; fi
	exit 0
}

events() {
	curl -sf "$API/v1/tasks/$1/events"
}

# wait_for TASK FILTER SECONDS waits until the task's events satisfy the
# jq FILTER.
wait_for() {
	local deadline=$((SECONDS + $3))
	while [ $SECONDS -lt $deadline ]; do
		if events "$1" | jq -e "$2" >/dev/null 2>&1; then return 0; fi
		sleep 0.5
	done
	return 1
}

# wait_until SECONDS COMMAND... waits until COMMAND succeeds.
wait_until() {
	local deadline=$((SECONDS + $1))
	shift
	while [ $SECONDS -lt $deadline ]; do
		if "$@"; then return 0; fi
		sleep 0.5
	done
	return 1
}

# payload TASK KIND prints the payload of the task's last event of KIND.
payload() {
	events "$1" | jq -c --arg k "$2" '[.[] | select(.kind == $k)] | last | .payload'
}

# report TASK prints the stub's report from the task's first assistant
# line.
report() {
	events "$1" | jq -c '[.[] | select(.kind == "harness_output" and .payload.type == "assistant")] | first | .payload.message.content[0].text | fromjson'
}

# create_task PROMPT creates a task on the daemon with the repository
# and prints its id; it bails out when the server refuses.
create_task() {
	local prompt=$1 response
	response=$(curl -sS -X POST "$API/v1/tasks" -H 'Content-Type: application/json' \
		-d "$(jq -cn --arg p "$prompt" --arg r "$REPO" \
			'{daemon_id:"ct-1",prompt:$p,workspace:{repo:$r,ref:"main"},pause_limits:{acknowledge:"1m",cleanup:"5m"}}')")
	if ! jq -er .task_id <<<"$response" 2>/dev/null; then
		echo "Bail out! the server refused the task: $response" >&2
		return 1
	fi
}

running() {
	kill -0 "$1" 2>/dev/null
}

not_running() {
	! kill -0 "$1" 2>/dev/null
}

start_daemon() {
	as_orch env SSH_AUTH_SOCK=$AGENT_SOCK $BIN/daemon \
		-server $API -cert $PKI/daemon.crt -key $PKI/daemon.key -state-dir $STATE \
		-harness-user orch-agent -workspace-dir $WS -claude $CLAUDE \
		>>"$DAEMON_LOG" 2>&1 &
	daemon_pid=$!
}

daemon_connected() {
	[ "$(grep -c 'msg=connecting' "$DAEMON_LOG")" -ge "$1" ]
}

echo "# image: $(. /etc/os-release && echo "$PRETTY_NAME"), $(uname -m), kernel $(uname -r)"
echo "# $(sudo -V | head -1)"
echo "# $(git --version), $(ssh -V 2>&1)"
echo "# claude: $CLAUDE, $($CLAUDE --version)"

# --- Setup: the git remote over ssh, the daemon user's ssh agent, the
# server and its PKI.
mkdir -p $MARKS && chmod 1777 $MARKS
install -m 0600 /dev/null "$DAEMON_LOG"
install -m 0600 /dev/null "$SERVER_LOG"

ssh-keygen -A >/dev/null
mkdir -p /run/sshd
/usr/sbin/sshd || die "sshd did not start"

as_orch sh -c 'mkdir -p -m 0700 ~/.ssh && ssh-keygen -q -t ed25519 -N "" -C orchestrator@container -f ~/.ssh/id_ed25519' ||
	die "ssh-keygen for orchestrator"
install -d -o git -g git -m 0700 /home/git/.ssh
install -o git -g git -m 0600 $OHOME/.ssh/id_ed25519.pub /home/git/.ssh/authorized_keys
as_user git sh -c "git init --quiet --bare -b main $BARE &&
	git init --quiet -b main /tmp/seed &&
	cd /tmp/seed &&
	echo seed >README &&
	git add README &&
	git -c user.name=seed -c user.email=seed@localhost commit --quiet -m seed &&
	git push --quiet $BARE main" || die "seed the bare repository"

# Checklist step 4, the part without Claude Code: known_hosts for the
# harness user.
as_user orch-agent sh -c 'mkdir -p -m 0700 ~/.ssh && ssh-keyscan -t ed25519 localhost >~/.ssh/known_hosts 2>/dev/null'
check "step 4: the harness user's known_hosts lists localhost" \
	grep -q '^localhost ssh-ed25519 ' /home/orch-agent/.ssh/known_hosts

as_orch ssh-agent -a $AGENT_SOCK >/dev/null || die "ssh-agent for orchestrator"
as_orch env SSH_AUTH_SOCK=$AGENT_SOCK ssh-add -q $OHOME/.ssh/id_ed25519 || die "ssh-add for orchestrator"
fingerprint=$(ssh-keygen -lf $OHOME/.ssh/id_ed25519.pub | awk '{print $2}')
echo "# daemon user's key: $fingerprint"

mkdir -p $SERVER_DIR
$BIN/server init-ca -pki-dir $SERVER_DIR/pki >/dev/null || die "init-ca"
$BIN/server issue-daemon-cert -pki-dir $SERVER_DIR/pki -id ct-1 >/dev/null || die "issue-daemon-cert"
install -d -o orchestrator -g orchestrator -m 0755 $PKI
install -o orchestrator -g orchestrator -m 0644 $SERVER_DIR/pki/daemons/ct-1/daemon.crt $PKI/daemon.crt
install -o orchestrator -g orchestrator -m 0600 $SERVER_DIR/pki/daemons/ct-1/daemon.key $PKI/daemon.key
$BIN/server -insecure-loopback -listen 127.0.0.1:8080 -db $SERVER_DIR/server.db \
	-permissions allow-all -slots-per-daemon 4 >>"$SERVER_LOG" 2>&1 &
wait_until 20 curl -sf -o /dev/null $API/v1/tasks || die "the server did not answer"

# --- Checklist steps 1 to 3.
echo "# id orch-agent"
id orch-agent 2>&1 | evidence
check "step 1: the harness user orch-agent exists" quiet id orch-agent
echo "# visudo -cf /etc/sudoers.d/orchestrator"
visudo -cf /etc/sudoers.d/orchestrator 2>&1 | evidence
check "step 2: visudo accepts the sudoers rule" quiet visudo -cf /etc/sudoers.d/orchestrator
out=$(as_orch sudo -n -u orch-agent -D / -- $CLAUDE --version 2>&1)
echo "# as orchestrator: sudo -n -u orch-agent -D / -- $CLAUDE --version"
evidence <<<"$out"
check "step 2: orchestrator runs claude --version as orch-agent through sudo" grep -q 'Claude Code' <<<"$out"
out=$(as_orch sudo -n -u orch-agent -- /usr/bin/cat /etc/hostname 2>&1)
echo "# as orchestrator: sudo -n -u orch-agent -- /usr/bin/cat /etc/hostname"
evidence <<<"$out"
check "step 2: sudo refuses a command the rule does not name" grep -q 'a password is required\|not allowed' <<<"$out"
out=$(as_orch sudo -n -u root -- /usr/bin/git --version 2>&1)
echo "# as orchestrator: sudo -n -u root -- /usr/bin/git --version"
evidence <<<"$out"
check "step 2: sudo refuses to run git as root" grep -q 'a password is required\|not allowed' <<<"$out"
out=$(as_orch sudo -n -u orch-agent -D / FOO=bar -- $CLAUDE --version 2>&1)
echo "# as orchestrator: sudo -n -u orch-agent -D / FOO=bar -- $CLAUDE --version"
evidence <<<"$out"
check "step 2: sudo refuses a variable env_keep does not list" grep -q 'not allowed to set the following environment variables' <<<"$out"
out=$(as_orch sudo -n -u orch-agent -D / GIT_TERMINAL_PROMPT=0 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 'GIT_SSH_COMMAND=ssh -o BatchMode=yes' -- /usr/bin/git --version 2>&1)
check "step 2: sudo passes the daemon's four git variables to git" grep -q '^git version' <<<"$out"
check "step 3: the workspace directory is owned by orch-agent" \
	test "$(stat -c %U /srv/orchestrator/workspaces)" = orch-agent

# --- Checklist step 6.
start_daemon
wait_until 30 daemon_connected 1 || die "the daemon did not connect"
line=$(grep 'running tasks as the harness user' "$DAEMON_LOG")
echo "# $line"
check "step 6: the daemon logs the harness user, git, rm and ssh_agent=true" \
	grep -q 'user=orch-agent git=/usr/bin/git rm=/usr/bin/rm workspace_dir=/srv/orchestrator/workspaces ssh_agent=true' <<<"$line"
fwd_dir=$(ls -d /tmp/orchestrator-agent-* 2>/dev/null | head -1)
fwd_sock=$(ls "$fwd_dir"/agent-*.sock 2>/dev/null | head -1)
echo "# forwarder: $(stat -c '%A %U %n' "$fwd_dir" "$fwd_sock" | tr '\n' ' ')"
check "ssh agent relay: directory mode 0711, owned by orchestrator" test "$(stat -c '%a %U' "$fwd_dir")" = "711 orchestrator"
check "ssh agent relay: socket mode 0666" test "$(stat -c '%a' "$fwd_sock")" = 666
out=$(sudo -u orch-agent -i ls "$fwd_dir" 2>&1)
echo "# as orch-agent: ls $fwd_dir"
evidence <<<"$out"
check "ssh agent relay: orch-agent cannot list the relay's directory" grep -q 'Permission denied' <<<"$out"
out=$(sudo -u orch-agent -i env SSH_AUTH_SOCK=$AGENT_SOCK ssh-add -l 2>&1)
echo "# as orch-agent: SSH_AUTH_SOCK=$AGENT_SOCK ssh-add -l"
evidence <<<"$out"
check "ssh agent relay: orch-agent cannot open the daemon user's own agent socket" grep -q 'Permission denied' <<<"$out"

# --- Checklist step 7: a task with a repository whose turn commits a file.
A=$(create_task "report and commit") || die "create task A"
echo "# task A: $A"
check "step 7: task A's harness started" wait_for "$A" 'any(.[]; .kind == "harness_started")' 30
check "step 7: task A's harness exited" wait_for "$A" 'any(.[]; .kind == "harness_exited")' 60
started=$(payload "$A" harness_started)
rep=$(report "$A")
pushed=$(payload "$A" branch_pushed)
exited=$(payload "$A" harness_exited)
echo "# harness_started: $started"
echo "# stub report: $rep"
echo "# branch_pushed: $pushed"
echo "# harness_exited: $exited"
check "step 7: harness_started names the workspace under -workspace-dir" \
	test "$(jq -r .workdir <<<"$started")" = "$WS/$A"
check "the stub ran as orch-agent, uid $(id -u orch-agent)" \
	test "$(jq -r '"\(.user) \(.uid)"' <<<"$rep")" = "orch-agent $(id -u orch-agent)"
check "the stub ran with HOME=/home/orch-agent" test "$(jq -r .home <<<"$rep")" = /home/orch-agent
check "the stub ran in the workspace" test "$(jq -r .cwd <<<"$rep")" = "$WS/$A"
check "the stub's SSH_AUTH_SOCK is the relay's socket under /tmp/orchestrator-agent-*" \
	grep -Eq '^/tmp/orchestrator-agent-[^/]+/agent-[^/]+\.sock$' <<<"$(jq -r .ssh_auth_sock <<<"$rep")"
check "step 9: ssh-add -l as the stub lists the daemon user's key" \
	grep -qF "$fingerprint" <<<"$(jq -r .ssh_add_l <<<"$rep")"
check "the stub committed a file as orch-agent" test -n "$(jq -r .commit <<<"$rep")"
check "step 7: branch_pushed for orchestrator/$A, ahead >= 1, no error" \
	jq_true --arg b "orchestrator/$A" '.branch == $b and .ahead >= 1 and .error == ""' <<<"$pushed"
remote=$(as_user git git --git-dir=$BARE rev-parse --verify --quiet "refs/heads/orchestrator/$A")
echo "# bare repository: refs/heads/orchestrator/$A = $remote"
check "step 7: the bare repository holds orchestrator/$A at the pushed commit" \
	test -n "$remote" -a "$remote" = "$(jq -r .commit <<<"$pushed")"
as_user git git --git-dir=$BARE log -1 --format='# commit %h by %an <%ae>: %s' "refs/heads/orchestrator/$A"
check "step 7: harness_exited with exit code 0" jq_true '.exit_code == 0' <<<"$exited"
check "step 7: the task page shows the branch" \
	sh -c "curl -sf $API/tasks/$A | grep -qF 'orchestrator/$A'"

echo "# workspace: $(stat -c '%A %U %n' "$WS/$A")"
others=$(find "$WS/$A" ! -user orch-agent)
check "the workspace's files are all owned by orch-agent" test -z "$others"
check "the workspace has mode 0700 (umask=0077 from sudoers)" test "$(stat -c %a "$WS/$A")" = 700
out=$(as_orch ls "$WS/$A" 2>&1)
echo "# as orchestrator: ls $WS/$A"
evidence <<<"$out"
check "the daemon's user cannot list the workspace itself" grep -q 'Permission denied' <<<"$out"
out=$(as_orch sudo -n -u orch-agent -D "$WS/$A" -- /usr/bin/git rev-parse --show-toplevel 2>&1)
echo "# as orchestrator: sudo -n -u orch-agent -D $WS/$A -- /usr/bin/git rev-parse --show-toplevel"
evidence <<<"$out"
check "-D with CWD=* runs git in the workspace" test "$out" = "$WS/$A"

# --- Checklist step 9: the daemon's files, as the harness user.
echo "# daemon files: $(stat -c '%a %U %n' $PKI/daemon.key $STATE $STATE/* | tr '\n' ' ')"
out=$(sudo -u orch-agent -i cat $PKI/daemon.key 2>&1)
echo "# as orch-agent: cat $PKI/daemon.key"
evidence <<<"$out"
check "step 9: orch-agent cannot read the daemon's key" grep -q 'Permission denied' <<<"$out"
out=$(sudo -u orch-agent -i ls $STATE 2>&1)
echo "# as orch-agent: ls $STATE"
evidence <<<"$out"
check "step 9: orch-agent cannot list the daemon's state directory" grep -q 'Permission denied' <<<"$out"
out=$(sudo -u orch-agent -i cat $OHOME/.ssh/id_ed25519 2>&1)
echo "# as orch-agent: cat $OHOME/.ssh/id_ed25519"
evidence <<<"$out"
check "orch-agent cannot read the daemon user's ssh key" grep -q 'Permission denied' <<<"$out"

# hold_task NAME starts a task whose stub holds its turn, and sets
# task, sudo_pid and stub_pid.
hold_task() {
	sudo_pid="" stub_pid=""
	task=$(create_task "report, then hold:$MARKS/release-$1") || die "create task $1"
	echo "# task $1: $task"
	if ! wait_for "$task" 'any(.[]; .kind == "harness_output" and .payload.type == "assistant")' 30; then
		not_ok "task $1's stub reported"
		return 1
	fi
	sudo_pid=$(payload "$task" harness_started | jq -r .pid)
	stub_pid=$(report "$task" | jq -r .pid)
	echo "# task $1: sudo pid $sudo_pid, stub pid $stub_pid"
}

# --- Checklist steps 8 and 10.
hold_task B
B=$task
ps_out=$(ps -o user,pid,ppid,command -ax | grep '[c]laude')
echo "# ps -o user,pid,ppid,command -ax | grep claude"
evidence <<<"$ps_out"
check "step 8: the stub runs as orch-agent, its parent the sudo pid harness_started names" \
	test "$(ps -o user:32=,ppid= -p "$stub_pid" | awk '{print $1, $2}')" = "orch-agent $sudo_pid"
check "step 8: the parent is sudo -n -u orch-agent -D <workspace> -- $CLAUDE" \
	grep -q "^sudo -n -u orch-agent -D $WS/$B -- $CLAUDE " <<<"$(ps -o command= -p "$sudo_pid")"
# A journal stays while its harness runs; the daemon deletes it once the
# server holds its events and no process writes it.
journal=$STATE/journal/$B.jsonl
echo "# journal: $(stat -c '%a %U %n' "$journal")"
out=$(sudo -u orch-agent -i cat "$journal" 2>&1)
echo "# as orch-agent: cat $journal"
evidence <<<"$out"
check "step 9: orch-agent cannot read task B's journal" grep -q 'Permission denied' <<<"$out"
sudo -u orch-agent kill -TERM "$stub_pid"
echo "# as root: sudo -u orch-agent kill -TERM $stub_pid"
check "step 10: task B's harness exited" wait_for "$B" 'any(.[]; .kind == "harness_exited")' 30
echo "# harness_exited: $(payload "$B" harness_exited)"
check "step 10: the stub got SIGTERM" test -e "$MARKS/$stub_pid.term"
check "step 10: the stub and its sudo are gone" wait_until 5 sh -c "! kill -0 $stub_pid 2>/dev/null && ! kill -0 $sudo_pid 2>/dev/null"

# --- Checklist step 11: SIGTERM to sudo from the daemon's user.
hold_task C
C=$task
out=$(as_orch kill -TERM "$sudo_pid" 2>&1)
echo "# as orchestrator: kill -TERM $sudo_pid${out:+ -> $out}"
check "step 11: task C's harness exited" wait_for "$C" 'any(.[]; .kind == "harness_exited")' 30
echo "# harness_exited: $(payload "$C" harness_exited)"
check "step 11: sudo relayed SIGTERM to the stub" test -e "$MARKS/$stub_pid.term"
check "step 11: the stub and its sudo are gone" wait_until 5 sh -c "! kill -0 $stub_pid 2>/dev/null && ! kill -0 $sudo_pid 2>/dev/null"

# --- The daemon's Kill: a stop whose harness outlasts the 30 s shutdown
# timeout, since the held stub reads neither the interrupt nor the end
# of its input.
hold_task D
D=$task
t0=$SECONDS
curl -sf -o /dev/null -X POST "$API/v1/tasks/$D/commands" -H 'Content-Type: application/json' -d '{"kind":"stop"}'
check "stop: task D's harness exited" wait_for "$D" 'any(.[]; .kind == "harness_exited")' 90
elapsed=$((SECONDS - t0))
echo "# stop to harness_exited: ${elapsed}s; harness_exited: $(payload "$D" harness_exited)"
check "stop: the stub ended on SIGTERM, not on the end of its input" \
	test -e "$MARKS/$stub_pid.term" -a ! -e "$MARKS/$stub_pid.eof"
check "stop: the daemon killed it after the shutdown timeout (>= 29 s)" test "$elapsed" -ge 29
check "stop: the stub and its sudo are gone" wait_until 5 sh -c "! kill -0 $stub_pid 2>/dev/null && ! kill -0 $sudo_pid 2>/dev/null"
check "stop: the daemon deleted the stopped task's workspace through rm as orch-agent" \
	wait_until 30 test ! -e "$WS/$D"

# --- A restarted daemon terminates the harness its previous run left.
hold_task E
E=$task
kill -KILL "$daemon_pid"
wait "$daemon_pid" 2>/dev/null
check "restart: the stub outlives the daemon killed with SIGKILL" running "$stub_pid"
start_daemon
check "restart: the restarted daemon starts" wait_until 30 daemon_connected 2
check "restart: the stub got SIGTERM within the 30 s the daemon waits for it" wait_until 60 test -e "$MARKS/$stub_pid.term"
check "restart: the stub and its sudo are gone" wait_until 5 sh -c "! kill -0 $stub_pid 2>/dev/null && ! kill -0 $sudo_pid 2>/dev/null"
line=$(grep "harness left by the previous daemon did not exit; killed it.*task=$E" "$DAEMON_LOG")
echo "# $line"
check "restart: the daemon logs that it killed the harness its previous run left" test -n "$line"
check "restart: task E's harness exit is reported" wait_for "$E" 'any(.[]; .kind == "harness_exited")' 30
echo "# harness_exited: $(payload "$E" harness_exited)"

finish
