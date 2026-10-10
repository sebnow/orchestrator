#!/bin/bash
# The harness-user container check. It runs as root inside the image
# Dockerfile builds, with the server and daemon binaries mounted at
# /opt/orchestrator/bin, and works through the steps of the checklist in
# README.md, "Running the harness as another user", that need no Claude
# Code login, with the claude stub at /usr/local/bin/claude, and logs
# the stub in from the server (README.md, "Logging a daemon in"). Each check
# prints "ok" or "not ok"; lines starting with "#" are evidence. It exits
# 1 when a check failed. Logs go to /out when it is mounted.
#
# With --real-claude, in the image's target real, /usr/local/bin/claude
# is Claude Code itself, standard input carries the login, and after the
# shared steps 1 to 3 and 6 and the login status facts, the checks of
# real-claude.sh run instead of the stub's.
set -u

REAL=false
if [ "${1:-}" = --real-claude ]; then
	REAL=true
fi

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
SSHD_LOG=/var/log/sshd.log
SSH_CALLS=/var/log/ssh-calls.log
SUDO_LOG=/var/log/sudo.log
DAEMON_KEY=$STATE/ssh_ed25519
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

# ORCH runs the command that follows it as the daemon's user, with
# Debian's default PATH and nothing else of root's environment.
ORCH=(setpriv --reuid=orchestrator --regid=orchestrator --init-groups
	env -i PATH=$DAEMON_PATH HOME=$OHOME USER=orchestrator LOGNAME=orchestrator)

as_orch() {
	"${ORCH[@]}" "$@"
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
	if [ "$REAL" = true ]; then
		print_costs
		remove_credentials
	fi
	if [ -d /out ]; then
		cp "$DAEMON_LOG" "$SERVER_LOG" "$SSHD_LOG" "$SSH_CALLS" "$SUDO_LOG" /out/ 2>/dev/null
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

# start_daemon starts the daemon in the background and sets daemon_pid
# to its pid. It runs ORCH as a simple command rather than as_orch: a
# function run in the background gets a subshell, $! would name that
# subshell, and SIGKILL to it would leave the daemon running.
start_daemon() {
	"${ORCH[@]}" env SSH_AUTH_SOCK=$AGENT_SOCK $BIN/daemon \
		-server $API -cert $PKI/daemon.crt -key $PKI/daemon.key -state-dir $STATE \
		-harness-user orch-agent -workspace-dir $WS -claude $CLAUDE \
		>>"$DAEMON_LOG" 2>&1 &
	daemon_pid=$!
}

daemon_connected() {
	[ "$(grep -c 'msg=connecting' "$DAEMON_LOG")" -ge "$1" ]
}

# no_daemon succeeds when no daemon process runs. A daemon that outlived
# its SIGKILL would share its state directory with the restarted one, and
# both would journal the end of the turn.
no_daemon() {
	! pgrep -f "^$BIN/daemon " >/dev/null
}

# lock_wait_logged succeeds once a daemon has logged that it is waiting
# for another daemon to release the state directory's lock.
lock_wait_logged() {
	grep -q 'state directory held by another daemon; waiting' "$DAEMON_LOG"
}

# restart_reported TASK checks what the server holds of TASK, whose turn
# a daemon restart cut short, once the restarted daemon has sent its
# events: one harness_exited, the restart's, and the task paused for the
# owner to resume (docs/adr/2026-10-08-shutdown-recovery.md).
restart_reported() {
	local task=$1 exits
	check "restart: the restarted daemon sent the task's events and deleted its journal" \
		wait_until 30 test ! -e "$STATE/journal/$task.jsonl"
	exits=$(events "$task" | jq -c '[.[] | select(.kind == "harness_exited") | {seq, payload}]')
	echo "# harness_exited events: $exits"
	check "restart: exactly one harness_exited, the restart's cut-short one" \
		jq_true 'length == 1 and .[0].payload.exit_code == -1 and .[0].payload.error == "daemon restarted during the turn"' <<<"$exits"
	check "restart: no daemon logged the end of a process of the task" \
		test -z "$(grep "msg=\"process ended\" task=$task" "$DAEMON_LOG")"
	check "restart: the task is paused" jq_true '.state == "paused"' <<<"$(curl -sf "$API/v1/tasks/$task")"
}

# mirror_checks TASK BRANCH_PUSHED checks that the task's workspace was
# cloned from the daemon's mirror, and that its push went from the
# mirror to the remote as the daemon's user with the daemon's key alone.
mirror_checks() {
	local task=$1 pushed=$2 mirror origin out
	mirror=$STATE/mirrors/$(printf '%s' "$REPO" | sha256sum | cut -d' ' -f1).git
	origin=$(as_user orch-agent git -C "$WS/$task" config remote.origin.url)
	echo "# as orch-agent: git -C $WS/$task config remote.origin.url -> $origin"
	echo "# mirror: $(stat -c '%a %U %n' "$mirror")"
	check "mirror: the workspace's origin is the daemon's mirror of the repository" test "$origin" = "$mirror"
	check "mirror: the mirror is owned by orchestrator" test "$(stat -c %U "$mirror")" = orchestrator
	out=$(as_orch git --git-dir="$mirror" rev-parse --verify --quiet "refs/heads/orchestrator/$task")
	echo "# mirror: refs/heads/orchestrator/$task = $out"
	check "mirror: the mirror holds the pushed commit on the task branch" test -n "$out" -a "$out" = "$(jq -r .commit <<<"$pushed")"
	echo "# $SSH_CALLS:"
	evidence <"$SSH_CALLS"
	check "mirror: the push to the remote ran from the mirror as orchestrator, offering the daemon's key alone and checking the server's host keys" \
		grep -qE "^user=orchestrator parents=.*<git push --quiet origin refs/heads/orchestrator/$task:refs/heads/orchestrator/$task.* args=-i $DAEMON_KEY -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$STATE/known_hosts .*git-receive-pack" "$SSH_CALLS"
	check "mirror: no user but orchestrator ran ssh" test -z "$(grep -v '^user=orchestrator ' "$SSH_CALLS")"
	out=$(grep 'Accepted publickey for git' "$SSHD_LOG")
	echo "# $SSHD_LOG, accepted keys:"
	evidence <<<"$out"
	check "mirror: sshd accepted the daemon's key and no other" \
		test -n "$out" -a -z "$(grep -vF "ED25519 $daemon_key_fp" <<<"$out")"
	out=$(grep -F "COMMAND=/usr/bin/git upload-pack $WS/$task" "$SUDO_LOG")
	echo "# $SUDO_LOG, upload-pack:"
	evidence <<<"$out"
	check "mirror: the fetch into the mirror ran upload-pack in the workspace as orch-agent through sudo" \
		grep -q ' orchestrator : .*USER=orch-agent' <<<"$out"
}

if [ "$REAL" = true ]; then
	. /usr/local/bin/real-claude.sh
	install_credentials
fi

echo "# image: $(. /etc/os-release && echo "$PRETTY_NAME"), $(uname -m), kernel $(uname -r)"
echo "# $(sudo -V | head -1)"
echo "# $(git --version), $(ssh -V 2>&1)"
echo "# claude: $CLAUDE, $($CLAUDE --version)"

# --- Setup: the git remote over ssh, the daemon user's ssh agent, the
# server and its PKI. sshd logs each key it accepts, the image's
# /usr/local/bin/ssh (ssh-log) logs who runs ssh from which processes,
# and sudo logs each command it runs, so that the check can
# tell which user reached the remote, and with which key.
mkdir -p $MARKS && chmod 1777 $MARKS
install -m 0600 /dev/null "$DAEMON_LOG"
install -m 0600 /dev/null "$SERVER_LOG"
install -m 0622 /dev/null "$SSH_CALLS"
printf 'Defaults logfile=%s, loglinelen=0\n' "$SUDO_LOG" >/etc/sudoers.d/zz-log
chmod 0440 /etc/sudoers.d/zz-log

ssh-keygen -A >/dev/null
mkdir -p /run/sshd
/usr/sbin/sshd -E "$SSHD_LOG" || die "sshd did not start"

# The daemon user's own key, which its ssh agent holds and the relay
# offers the harness user. The remote does not accept it: the daemon
# pushes with the key it generates in its state directory, which the
# remote accepts once the daemon has started.
as_orch sh -c 'mkdir -p -m 0700 ~/.ssh && ssh-keygen -q -t ed25519 -N "" -C orchestrator@container -f ~/.ssh/id_ed25519' ||
	die "ssh-keygen for orchestrator"
install -d -o git -g git -m 0700 /home/git/.ssh
install -o git -g git -m 0600 /dev/null /home/git/.ssh/authorized_keys
as_user git sh -c "git init --quiet --bare -b main $BARE &&
	git init --quiet -b main /tmp/seed &&
	cd /tmp/seed &&
	echo seed >README &&
	git add README &&
	git -c user.name=seed -c user.email=seed@localhost commit --quiet -m seed &&
	git push --quiet $BARE main" || die "seed the bare repository"

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
	-permissions allow-all -github-meta-url '' >>"$SERVER_LOG" 2>&1 &
wait_until 20 curl -sf -o /dev/null $API/v1/tasks || die "the server did not answer"

# Checklist step 4, the part without Claude Code: the remote's host key,
# given to the server, which sends it to the daemon. The daemon user
# has no known_hosts of its own.
# OpenSSH 10 prints a comment line before the key, which the server
# takes and does not send.
keyscan=$(ssh-keyscan -t ed25519 localhost 2>/dev/null)
echo "# ssh-keyscan -t ed25519 localhost:"
evidence <<<"$keyscan"
host_key=$(grep -v '^#' <<<"$keyscan")
out=$(jq -n --arg forge "$keyscan" '{forge: $forge}' | curl -sf -X PUT --data-binary @- $API/v1/host-keys)
echo "# PUT /v1/host-keys: $out"
check "step 4: the server takes the remote's host key" \
	jq_true --arg line "$host_key" '.lines == [$line]' <<<"$out"
check "step 4: the daemon user has no known_hosts of its own" test ! -e $OHOME/.ssh/known_hosts

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

# --- Checklist step 4: the host keys the server sent the daemon.
check "host keys: the daemon wrote the server's host keys to its known_hosts" \
	wait_until 10 sh -c '[ "$(cat "$1")" = "$2" ]' - $STATE/known_hosts "$host_key"
echo "# host keys: $(stat -c '%a %U %n' $STATE/known_hosts)"
check "host keys: known_hosts has mode 0600, owned by orchestrator" test "$(stat -c '%a %U' $STATE/known_hosts)" = "600 orchestrator"

# --- Checklist step 6: the daemon's own key, generated at its first
# start and shown on its page, registered at the remote by the owner.
echo "# daemon key: $(stat -c '%a %U %n' $DAEMON_KEY $DAEMON_KEY.pub $STATE | tr '\n' ' ')"
check "daemon key: the private key has mode 0600, owned by orchestrator" test "$(stat -c '%a %U' $DAEMON_KEY)" = "600 orchestrator"
check "daemon key: the state directory has mode 0711" test "$(stat -c %a $STATE)" = 711
daemon_key_line=$(cat $DAEMON_KEY.pub)
daemon_key=$(awk '{print $1, $2}' <<<"$daemon_key_line")
daemon_key_fp=$(ssh-keygen -lf $DAEMON_KEY.pub | awk '{print $2}')
echo "# $DAEMON_KEY.pub: $daemon_key_line"
echo "# fingerprint: $daemon_key_fp"
check "daemon key: ssh-keygen reads the private key as the public key's pair" \
	test "$(as_orch ssh-keygen -y -f $DAEMON_KEY | awk '{print $1, $2}')" = "$daemon_key"
page_key=$(curl -sf $API/daemons/ct-1 | sed -n 's|.*<pre class="ssh-key"><code>\([^<]*\)</code></pre>.*|\1|p')
echo "# the daemon's page: $page_key"
check "daemon key: the daemon's page shows its public key" test "$page_key" = "$daemon_key orchestrator@ct-1"
printf '%s\n' "$page_key" >/home/git/.ssh/authorized_keys
out=$(sudo -u orch-agent -i cat $DAEMON_KEY 2>&1)
echo "# as orch-agent: cat $DAEMON_KEY"
evidence <<<"$out"
check "daemon key: orch-agent cannot read the daemon's ssh key" grep -q 'Permission denied' <<<"$out"

# daemon_jq FILTER succeeds when the daemon's view in the owner API
# satisfies the jq FILTER.
daemon_jq() {
	curl -sf "$API/v1/daemons/ct-1" | jq -e "$1" >/dev/null 2>&1
}

# daemon_sudo_ran ARGS succeeds when sudo's log shows the daemon's user
# running claude with ARGS as orch-agent.
daemon_sudo_ran() {
	grep -F "COMMAND=$CLAUDE $1" "$SUDO_LOG" | grep -q ' orchestrator : .*USER=orch-agent'
}

if [ "$REAL" = true ]; then
	# --- Login status (docs/adr/2026-10-10-harness-login.md): the
	# credentials file logs claude in; no login is made here.
	check "login: the daemon reports login=yes for the copied login" wait_until 20 daemon_jq '.facts.login == "yes"'
	echo "# login facts: $(curl -sf "$API/v1/daemons/ct-1" | jq -c '.facts | {login, login_method, account: (if .account then "present, \(.account | length) characters" else null end)}')"
	check "login: the daemon reports a login method" daemon_jq '.facts.login_method | type == "string" and length > 0'
	check "login: the daemon read the status as orch-agent through sudo" daemon_sudo_ran "auth status --json"
	real_checks
	finish
fi

# --- Login (docs/adr/2026-10-10-harness-login.md): the stub starts
# logged out, and is logged in from the server with a code.
STUB_LOGIN_URL=$(sed -n "s/^STUB_LOGIN_URL='\(.*\)'$/\1/p" $CLAUDE)
check "login: the daemon reports login=no for the logged-out stub" wait_until 20 daemon_jq '.facts.login == "no" and .facts.login_method == "none"'
check "login: the dashboard flags the daemon as needing a login" sh -c "curl -sf $API/ | grep -q 'login needed'"
W=$(create_task "report and commit, once logged in") || die "create task W"
echo "# task W: $W"
check "login: a task for the logged-out daemon waits, saying why" \
	wait_until 10 sh -c "curl -sf $API/v1/tasks/$W | jq -e '.queue.reason == \"daemon ct-1 is not logged in\"' >/dev/null"
status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/v1/daemons/ct-1/login")
check "login: the server takes the owner's login (202)" test "$status" = 202
check "login: the daemon reports the URL" wait_until 30 daemon_jq '.login.phase == "started"'
url=$(curl -sf "$API/v1/daemons/ct-1" | jq -r .login.url)
echo "# login URL: $url"
check "login: the URL is the stub's, without the hyperlink's escapes" test "$url" = "$STUB_LOGIN_URL"
check "login: the daemon ran claude auth login as orch-agent through sudo" daemon_sudo_ran "auth login"
status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/v1/daemons/ct-1/login/code" \
	-H 'Content-Type: application/json' -d '{"code":"stub-code#stub-state"}')
check "login: the server takes the code (202)" test "$status" = 202
check "login: login_finished ok" wait_until 30 daemon_jq '.login.phase == "finished" and .login.ok'
echo "# login: $(curl -sf "$API/v1/daemons/ct-1" | jq -c '.login | {phase, ok, error}')"
echo "# login facts: $(curl -sf "$API/v1/daemons/ct-1" | jq -c '.facts | {login, login_method, account}')"
check "login: the facts say logged in, by claude.ai, as stub@example.com/stub-org" \
	daemon_jq '.facts.login == "yes" and .facts.login_method == "claude.ai" and .facts.account == "stub@example.com/stub-org"'
check "login: the login is orch-agent's, in its home" test "$(stat -c %U /home/orch-agent/.claude/stub-login 2>&1)" = orch-agent
check "login: the daemon read the status as orch-agent through sudo" daemon_sudo_ran "auth status --json"
check "login: the waiting task runs once the daemon is logged in" wait_for "$W" 'any(.[]; .kind == "harness_exited")' 60

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
mirror_checks "$A" "$pushed"
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
out=$(sudo -u orch-agent -i cat $STATE/state.json 2>&1)
echo "# as orch-agent: cat $STATE/state.json"
evidence <<<"$out"
check "step 9: orch-agent cannot read the daemon's state, though it may pass through its directory" grep -q 'Permission denied' <<<"$out"
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
check "stop: the stopped task's workspace is kept for a follow-up" \
	sh -c "test -e '$WS/$D' && ! grep -qF 'COMMAND=/usr/bin/rm -rf -- $WS/$D' '$SUDO_LOG'"

curl -sf -o /dev/null -X POST "$API/v1/tasks/$D/dismiss"
check "dismiss: the daemon deleted the dismissed task's workspace through rm as orch-agent" \
	wait_until 10 sh -c "! test -e '$WS/$D' && grep -F 'COMMAND=/usr/bin/rm -rf -- $WS/$D' '$SUDO_LOG' | grep -q 'USER=orch-agent'"

# --- A restarted daemon terminates the harness its previous run left.
hold_task E
E=$task
kill -KILL "$daemon_pid"
wait "$daemon_pid" 2>/dev/null
check "restart: no daemon process outlives the SIGKILL" wait_until 5 no_daemon
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
restart_reported "$E"

# --- A second daemon on the restarted daemon's state directory waits for
# the lock rather than starting beside it; checking the full wait would
# take its default timeout (the 30 s shutdown timeout plus 30 s), so this
# only catches the Info log of the wait, then kills the second daemon.
start_daemon
lock_daemon_pid=$daemon_pid
check "lock: a second daemon on the same state directory waits for the first to release it" \
	wait_until 2 lock_wait_logged
kill -KILL "$lock_daemon_pid"
wait "$lock_daemon_pid" 2>/dev/null

finish
