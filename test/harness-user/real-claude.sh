# The checks of check.sh --real-claude, sourced by check.sh after its
# helpers. They run Claude Code itself, logged in with the owner's login,
# which run.sh --real-claude passes on standard input: steps 4 and 7
# with the real binary, the PATH the agent's tools get under sudo's
# secure_path, the permission gateway, and how claude and its tools end
# on SIGTERM, on stop and on a daemon restart.
#
# The login lands in $CREDS only, on the tmpfs run.sh mounts at
# ~orch-agent/.claude, and finish deletes it. Nothing here prints it.

CREDS=/home/orch-agent/.claude/.credentials.json
AGENT_HOME=/home/orch-agent
# The sudoers line that lets the daemon's PATH through to claude.
PATH_RULE=/etc/sudoers.d/orchestrator-path
costs=""

# install_credentials writes standard input, the login JSON, to $CREDS
# with mode 0600, owned by orch-agent, and then closes standard input.
install_credentials() {
	chown orch-agent:orch-agent $AGENT_HOME/.claude
	chmod 0700 $AGENT_HOME/.claude
	(umask 077 && cat >"$CREDS")
	exec </dev/null
	chown orch-agent:orch-agent "$CREDS"
	if ! jq -e '.claudeAiOauth.accessToken | type == "string" and length > 0' "$CREDS" >/dev/null 2>&1; then
		rm -f "$CREDS"
		echo "Bail out! no Claude Code login on standard input"
		exit 1
	fi
	echo "# login: $(stat -c '%a %U' "$CREDS") $CREDS on $(stat -f -c %T $AGENT_HOME/.claude), keys $(jq -c '.claudeAiOauth | keys' "$CREDS")"
}

remove_credentials() {
	rm -f "$CREDS"
	if [ -e "$CREDS" ]; then
		not_ok "the login is deleted from the container"
	else
		ok "the login is deleted from the container"
	fi
}

now() {
	date +%s.%N
}

# since T prints the seconds since T, a now() value.
since() {
	awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.1f", b - a }'
}

# alive PID succeeds while PID runs and is not a zombie.
alive() {
	local s
	s=$(ps -o stat= -p "$1" 2>/dev/null) && [ -n "$s" ] && [ "${s#Z}" = "$s" ]
}

gone() {
	! alive "$1"
}

# env_path PID prints the PATH in the environment of PID, a process of
# orch-agent. It reads it as orch-agent: the container has no
# CAP_SYS_PTRACE, which root needs to read another user's environ.
env_path() {
	as_user orch-agent cat "/proc/$1/environ" 2>/dev/null | tr '\0' '\n' | sed -n 's/^PATH=//p'
}

# cost LABEL JSON records the total_cost_usd of a result.
cost() {
	local c
	c=$(jq -r '.total_cost_usd // empty' <<<"$2" 2>/dev/null)
	if [ -n "$c" ]; then costs="$costs$1 $c"$'\n'; fi
}

# task_cost LABEL TASK records the cost of the task's results.
task_cost() {
	local c
	c=$(events "$2" | jq '[.[] | select(.kind == "harness_output" and .payload.type == "result") | .payload.total_cost_usd // 0] | add // empty')
	if [ -n "$c" ]; then costs="$costs$1 $c"$'\n'; else costs="$costs$1 no result"$'\n'; fi
}

print_costs() {
	echo "# total_cost_usd reported by claude:"
	printf '%s' "$costs" | evidence
	echo "# sum: $(printf '%s' "$costs" | awk '$NF ~ /^[0-9.e-]+$/ { s += $NF } END { printf "%.4f", s }')"
}

# init TASK prints fields of the task's system/init line.
init() {
	events "$1" | jq -c '[.[] | select(.kind == "harness_output" and .payload.type == "system" and .payload.subtype == "init")] | first | .payload | {claude_code_version, model, permissionMode, apiKeySource, cwd, tools: (.tools | length), mcp_servers}'
}

# tool_uses TASK prints the task's tool calls, one per line.
tool_uses() {
	events "$1" | jq -r '.[] | select(.kind == "harness_output" and .payload.type == "assistant") | .payload.message.content[]? | select(.type == "tool_use") | "\(.name) \(.input | tostring)"'
}

# tool_results TASK prints the text of the task's tool results.
tool_results() {
	events "$1" | jq -r '.[] | select(.kind == "harness_output" and .payload.type == "user") | .payload.message.content | if type == "array" then .[] else empty end | select(.type == "tool_result") | "is_error=\(.is_error // false)", (if (.content | type) == "string" then .content else ([.content[]? | .text? // empty] | join("\n")) end)'
}

# result TASK prints the task's last result line, cut down.
result() {
	events "$1" | jq -c '[.[] | select(.kind == "harness_output" and .payload.type == "result")] | last | .payload | if . == null then null else {subtype, is_error, num_turns, duration_ms, total_cost_usd, result: (.result // "" | .[0:200])} end'
}

# show_tree prints the harness user's processes and the task's sudo.
show_tree() {
	echo "# ps -o user,pid,ppid,stat,args, sudo $sudo_pid and orch-agent's processes, cut to 160 columns:"
	{ ps -o user:12,pid,ppid,stat,args -p "$sudo_pid"; ps -o user:12=,pid=,ppid=,stat=,args= -u orch-agent; } | cut -c1-160 | evidence
}

# HOLD is the command a held turn runs through the Bash tool. Claude
# Code's Bash tool refuses a long leading `sleep` ("Long leading `sleep`
# commands are blocked", in 2.1.289), so the turn waits on tail, which
# never ends on its own.
HOLD='tail -f /dev/null'

holding() {
	pgrep -u orch-agent -fx "$HOLD" >/dev/null
}

# hold NAME starts a task whose turn runs $HOLD through the Bash tool and
# waits for it. It sets task, sudo_pid, claude_pid and tool_pid, the
# pid of $HOLD.
hold() {
	local name=$1
	task="" sudo_pid="" claude_pid="" tool_pid=""
	task=$(create_task "Run exactly this command with the Bash tool, in the foreground, as your first and only tool call: $HOLD") || die "create task $name"
	echo "# task $name: $task"
	if ! wait_until 180 holding; then
		not_ok "task $name's Bash tool runs $HOLD"
		echo "# tool calls:"; tool_uses "$task" | cut -c1-300 | evidence
		echo "# tool results:"; tool_results "$task" | cut -c1-600 | evidence
		echo "# result: $(result "$task")"
		echo "# harness_exited: $(payload "$task" harness_exited | cut -c1-600)"
		task_cost "task $name" "$task"
		return 1
	fi
	ok "task $name's Bash tool runs $HOLD"
	tool_pid=$(pgrep -n -u orch-agent -fx "$HOLD")
	sudo_pid=$(payload "$task" harness_started | jq -r .pid)
	claude_pid=$(pgrep -P "$sudo_pid" | head -1)
	if [ -z "$claude_pid" ] || [ -z "$tool_pid" ]; then
		not_ok "task $name: found claude under sudo $sudo_pid and $HOLD"
		return 1
	fi
	echo "# task $name: sudo pid $sudo_pid, claude pid $claude_pid, $HOLD pid $tool_pid"
	show_tree
	echo "# PATH: claude $(env_path "$claude_pid"); $HOLD $(env_path "$tool_pid")"
}

# ended NAME T0 checks that claude, its sudo and $HOLD are gone after
# a signal sent at T0, and reports how long each took.
ended() {
	local name=$1 t0=$2 t_claude t_sleep
	if wait_until 90 gone "$claude_pid"; then
		t_claude=$(since "$t0")
		ok "$name: claude (pid $claude_pid) exited, ${t_claude}s after the signal"
	else
		not_ok "$name: claude (pid $claude_pid) exited within 90 s"
	fi
	if wait_until 15 gone "$tool_pid"; then
		t_sleep=$(since "$t0")
		ok "$name: the Bash tool's $HOLD (pid $tool_pid) ended, ${t_sleep}s after the signal"
	else
		not_ok "$name: the Bash tool's $HOLD (pid $tool_pid) ended with claude"
		show_tree
		kill -KILL "$tool_pid" 2>/dev/null
	fi
	check "$name: sudo (pid $sudo_pid) is gone" wait_until 15 gone "$sudo_pid"
}

real_checks() {
	echo "# Debian's sudoers defaults:"
	grep -E '^Defaults' /etc/sudoers | evidence
	secure_path=$(sed -n 's/^Defaults[[:space:]]*secure_path="\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' /etc/sudoers)
	echo "# the daemon's PATH: $DAEMON_PATH"

	# --- Step 4: the login, found under sudo -i.
	echo "# as root: sudo -u orch-agent -i claude -p 'Reply with the single word ok.' --model haiku --output-format json"
	local out
	out=$(sudo -u orch-agent -i claude -p 'Reply with the single word ok.' --model haiku --output-format json 2>&1)
	if jq -e . <<<"$out" >/dev/null 2>&1; then
		jq -c '{type, subtype, is_error, result, total_cost_usd, models: (.modelUsage // {} | keys)}' <<<"$out" | evidence
	else
		cut -c1-400 <<<"$out" | evidence
	fi
	cost "step 4" "$out"
	check "step 4: claude -p as orch-agent under sudo -i replies with the login it finds" \
		jq_true '.is_error == false and (.result | test("ok"; "i"))' <<<"$out"
	echo "# ~orch-agent after the first run: $(ls -A $AGENT_HOME | tr '\n' ' ')"
	echo "# ~orch-agent/.claude.json: $(jq -c '{hasCompletedOnboarding, oauthAccount: has("oauthAccount"), keys: length}' $AGENT_HOME/.claude.json 2>&1)"

	# --- Step 7 with claude: a task that writes a file, commits it and
	# shows its PATH.
	A=$(create_task 'Run exactly this one command with the Bash tool, then reply "done": printf "hello from the harness user\n" > hello.txt && git add hello.txt && git commit -q -m "Add hello.txt" && git log -1 --format=%H; echo "PATH=$PATH"; id -un; which git node rg; true') ||
		die "create task A"
	echo "# task A: $A"
	check "step 7: task A's harness started" wait_for "$A" 'any(.[]; .kind == "harness_started")' 30
	check "step 7: task A's harness exited" wait_for "$A" 'any(.[]; .kind == "harness_exited")' 240
	local started pushed exited remote results
	started=$(payload "$A" harness_started)
	pushed=$(payload "$A" branch_pushed)
	exited=$(payload "$A" harness_exited)
	echo "# harness_started: $started"
	echo "# init: $(init "$A")"
	echo "# tool calls:"
	tool_uses "$A" | cut -c1-400 | evidence
	echo "# permission_requested:"
	events "$A" | jq -c '.[] | select(.kind == "permission_requested") | .payload | {request_id, tool, input: (.input | tostring | .[0:200])}' | evidence
	results=$(tool_results "$A")
	echo "# tool results:"
	evidence <<<"$results"
	echo "# result: $(result "$A")"
	echo "# branch_pushed: $pushed"
	echo "# harness_exited: $exited"
	task_cost "task A" "$A"
	check "step 7: init reports claude 2.x in the workspace" \
		jq_true --arg w "$WS/$A" '.cwd == $w and (.claude_code_version | startswith("2."))' <<<"$(init "$A")"
	check "gateway: the Bash call went to the permission tool (permission_requested for Bash)" \
		jq_true 'any(.[]; .kind == "permission_requested" and .payload.tool == "Bash")' <<<"$(events "$A")"
	check "gateway: allow-all's answer let the Bash call run (a tool result without is_error)" \
		grep -q '^is_error=false$' <<<"$results"
	check "the Bash tool ran as orch-agent" grep -qx 'orch-agent' <<<"$results"
	check "the Bash tool reports its PATH" grep -q '^PATH=' <<<"$results"
	check "step 7: hello.txt in the workspace is owned by orch-agent" \
		test "$(stat -c %U "$WS/$A/hello.txt" 2>&1)" = orch-agent
	check "step 7: branch_pushed for orchestrator/$A, ahead >= 1, no error" \
		jq_true --arg b "orchestrator/$A" '.branch == $b and .ahead >= 1 and .error == ""' <<<"$pushed"
	remote=$(as_user git git --git-dir=$BARE rev-parse --verify --quiet "refs/heads/orchestrator/$A")
	echo "# bare repository: refs/heads/orchestrator/$A = $remote"
	check "step 7: the bare repository holds orchestrator/$A at the pushed commit" \
		test -n "$remote" -a "$remote" = "$(jq -r .commit <<<"$pushed")"
	as_user git git --git-dir=$BARE log -1 --format='# commit %h by %an <%ae>: %s' "refs/heads/orchestrator/$A"
	check "step 7: harness_exited with exit code 0" jq_true '.exit_code == 0' <<<"$exited"
	mirror_checks "$A" "$pushed"

	# --- Step 11 with claude: SIGTERM to sudo from the daemon's user,
	# under the README's rule, where secure_path sets claude's PATH.
	if hold C; then
		C=$task
		check "step 8: claude runs as orch-agent, its parent the sudo pid harness_started names" \
			test "$(ps -o user:32=,ppid= -p "$claude_pid" | awk '{print $1, $2}')" = "orch-agent $sudo_pid"
		check "secure_path: claude's PATH is sudo's secure_path, not the daemon's" \
			test "$(env_path "$claude_pid")" = "$secure_path"
		local t0
		t0=$(now)
		as_orch kill -TERM "$sudo_pid"
		echo "# as orchestrator: kill -TERM $sudo_pid"
		ended "step 11" "$t0"
		check "step 11: task C's harness exited" wait_for "$C" 'any(.[]; .kind == "harness_exited")' 30
		echo "# harness_exited: $(payload "$C" harness_exited | cut -c1-600)"
		task_cost "task C" "$C"
	fi

	# --- The sudoers line that exempts claude from secure_path.
	echo 'Defaults!/usr/local/bin/claude !secure_path' >$PATH_RULE
	chmod 0440 $PATH_RULE
	echo "# $PATH_RULE: $(cat $PATH_RULE)"
	check "secure_path: visudo accepts the exemption" quiet visudo -c

	# --- The daemon's stop, with the exemption.
	if hold D; then
		D=$task
		check "secure_path exempted: claude's PATH is the daemon's" test "$(env_path "$claude_pid")" = "$DAEMON_PATH"
		check "secure_path exempted: the Bash tool's $HOLD has the daemon's PATH" test "$(env_path "$tool_pid")" = "$DAEMON_PATH"
		local t0
		t0=$(now)
		curl -sf -o /dev/null -X POST "$API/v1/tasks/$D/commands" -H 'Content-Type: application/json' -d '{"kind":"stop"}'
		echo "# POST /v1/tasks/$D/commands {\"kind\":\"stop\"}"
		ended "stop" "$t0"
		check "stop: task D's harness exited" wait_for "$D" 'any(.[]; .kind == "harness_exited")' 60
		echo "# stop to harness_exited event seen: $(since "$t0")s; harness_exited: $(payload "$D" harness_exited | cut -c1-600)"
		grep "task=$D" "$DAEMON_LOG" | grep -E 'process ended|kill|did not exit' | evidence
		check "stop: the daemon deleted the stopped task's workspace" wait_until 30 test ! -e "$WS/$D"
		task_cost "task D" "$D"
	fi

	# --- A restarted daemon and the claude its previous run left.
	if hold E; then
		E=$task
		local t0
		kill -KILL "$daemon_pid"
		wait "$daemon_pid" 2>/dev/null
		t0=$(now)
		echo "# as root: kill -KILL <daemon>"
		check "restart: no daemon process outlives the SIGKILL" wait_until 5 no_daemon
		sleep 2
		if alive "$claude_pid"; then
			echo "# 2 s after the daemon's SIGKILL claude (pid $claude_pid) still runs"
		else
			echo "# 2 s after the daemon's SIGKILL claude (pid $claude_pid) is gone"
		fi
		start_daemon
		check "restart: the restarted daemon starts" wait_until 30 daemon_connected 2
		ended "restart" "$t0"
		grep "task=$E" "$DAEMON_LOG" | grep -E 'previous daemon' | evidence
		check "restart: task E's harness exit is reported" wait_for "$E" 'any(.[]; .kind == "harness_exited")' 60
		echo "# harness_exited: $(payload "$E" harness_exited | cut -c1-600)"
		restart_reported "$E"
		task_cost "task E" "$E"
	fi
}
