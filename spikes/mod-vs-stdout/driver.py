"""Spike driver: runs `claude -p` in stream-json mode, writes turns to its
stdin, records every stdout line, and serves as the HTTP receiver the
spike-probe mod and the permission MCP server talk to. Everything lands in
one events.jsonl with timestamps from one clock.

Usage: python3 driver.py SCENARIO [--out DIR]   (see SCENARIOS)
"""

import argparse
import json
import os
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
MOD_DIR = HERE / "mod"
MCP_SERVER = HERE / "perm_mcp.py"

DENY_REASON = "Denied by the spike receiver acting as the daemon."


class Server(ThreadingHTTPServer):
    # The default backlog of 5 delayed concurrent mod reports by seconds.
    request_queue_size = 128


def now_ms():
    return time.time() * 1000.0


class Log:
    def __init__(self, path):
        self.lock = threading.Condition()
        self.records = []
        self.fh = open(path, "w", buffering=1)
        self.t0 = now_ms()

    def record(self, src, **fields):
        with self.lock:
            rec = {"n": len(self.records), "t": now_ms(), "src": src, **fields}
            rec["rel"] = round(rec["t"] - self.t0, 1)
            self.records.append(rec)
            self.fh.write(json.dumps(rec) + "\n")
            self.lock.notify_all()
            return rec

    def wait(self, pred, timeout, start=0, label=""):
        deadline = time.time() + timeout
        with self.lock:
            while True:
                for rec in self.records[start:]:
                    if pred(rec):
                        return rec
                left = deadline - time.time()
                if left <= 0:
                    self.record("driver", note=f"timeout waiting for {label}")
                    return None
                self.lock.wait(left)

    def count(self, pred):
        with self.lock:
            return sum(1 for r in self.records if pred(r))


def decide(req):
    """The daemon's permission policy, shared by the mod and the MCP server."""
    inp = req.get("input") or {}
    cmd = inp.get("command", "") if isinstance(inp, dict) else ""
    if "spike-allowed" in cmd or cmd.startswith(("sleep", "ping")):
        return {"decision": "allow", "reason": "Allowed by the spike receiver."}
    if "spike-denied" in cmd:
        return {"decision": "deny", "reason": DENY_REASON}
    return {}


def make_handler(log, commands):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def _body(self):
            n = int(self.headers.get("content-length") or 0)
            return self.rfile.read(n).decode() if n else ""

        def _reply(self, obj):
            data = json.dumps(obj).encode()
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_POST(self):
            raw = self._body()
            t = now_ms()
            try:
                body = json.loads(raw)
            except ValueError:
                body = {"raw": raw}
            if self.path == "/event":
                sent = body.get("sentAt")
                log.record("mod", latency_ms=round(t - sent, 1) if sent else None, **body)
                self._reply({})
            elif self.path == "/decide":
                answer = decide(body)
                log.record("receiver", action="decide", request=body, answer=answer)
                self._reply(answer)
            else:
                self.send_error(404)

        def do_GET(self):
            if self.path == "/command":
                with log.lock:
                    served = list(commands)
                    commands.clear()
                if served:
                    log.record("receiver", action="commands-served", commands=served)
                self._reply(served)
            else:
                self.send_error(404)

    return Handler


class Run:
    def __init__(self, scenario, out_dir):
        self.scenario = scenario
        self.out = out_dir
        self.out.mkdir(parents=True, exist_ok=True)
        self.log = Log(self.out / "events.jsonl")
        self.commands = []
        self.server = Server(("127.0.0.1", 0), make_handler(self.log, self.commands))
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        self.workdir = Path(tempfile.mkdtemp(prefix=f"modspike-{scenario}-"))
        self.proc = None

    def start(self, extra_args, with_mod, env=None):
        args = [
            "claude", "-p",
            "--input-format", "stream-json",
            "--output-format", "stream-json",
            "--verbose",
            "--model", "haiku",
            "--setting-sources", "project",
            "--strict-mcp-config",
            "--debug-file", str(self.out / "debug.log"),
        ]
        if with_mod:
            args += ["--plugin-dir", str(MOD_DIR)]
        args += extra_args
        env = dict(os.environ, SPIKE_RECEIVER_URL=self.url, **(env or {}))
        self.log.record("driver", note="spawn", argv=args, cwd=str(self.workdir), receiver=self.url)
        self.proc = subprocess.Popen(
            args, cwd=self.workdir, env=env, text=True, bufsize=1,
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        raw = open(self.out / "stdout.jsonl", "w", buffering=1)

        def read_stdout():
            for line in self.proc.stdout:
                raw.write(line)
                try:
                    msg = json.loads(line)
                except ValueError:
                    msg = {"unparsed": line.rstrip("\n")}
                self.log.record("stdout", msg=msg)

        def read_stderr():
            for line in self.proc.stderr:
                self.log.record("stderr", line=line.rstrip("\n"))

        threading.Thread(target=read_stdout, daemon=True).start()
        threading.Thread(target=read_stderr, daemon=True).start()

    def send_raw(self, obj):
        self.log.record("stdin", msg=obj)
        self.proc.stdin.write(json.dumps(obj) + "\n")
        self.proc.stdin.flush()

    def send(self, text):
        self.send_raw({"type": "user", "message": {"role": "user", "content": text}})

    def enqueue(self, cmd):
        with self.log.lock:
            self.commands.append(cmd)
        self.log.record("receiver", action="enqueue", command=cmd)

    def wait_result(self, k, timeout=120):
        """Waits for the k-th stdout `result` message (1-based)."""

        def is_result(r):
            return r["src"] == "stdout" and r["msg"].get("type") == "result"

        # Log.wait rescans from the start on every new record, so the
        # predicate must not count across calls.
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.log.count(is_result) >= k:
                return True
            time.sleep(0.1)
        self.log.record("driver", note=f"timeout waiting for result #{k}")
        return None

    def finish(self, timeout=30):
        time.sleep(2)
        self.log.record("driver", note="close stdin")
        self.proc.stdin.close()
        try:
            code = self.proc.wait(timeout)
        except subprocess.TimeoutExpired:
            self.proc.terminate()
            code = self.proc.wait(10)
            self.log.record("driver", note="terminated after timeout")
        time.sleep(0.5)
        files = sorted(p.name for p in self.workdir.iterdir())
        self.log.record("driver", note="exit", code=code, workdir_files=files)
        self.server.shutdown()


def mod_event(name, phase=None, test=None):
    def pred(r):
        if r["src"] != "mod" or r.get("event") != name:
            return False
        if phase and r.get("phase") != phase:
            return False
        return test(r) if test else True

    return pred


def bash_command(r):
    data = r.get("data") or {}
    return data.get("command", "") if isinstance(data, dict) else ""


PROMPT_PERMS = (
    "Use the Bash tool to run `touch spike-allowed.txt`, then use the Bash tool "
    "again to run `touch spike-denied.txt`, then reply with one line saying which succeeded."
)
# The engine refuses `sleep 30` and `sleep 30 && echo finished` in the
# foreground (control-1, control-2), so the abort scenarios use ping.
PROMPT_SLEEP = "Use the Bash tool to run `sleep 30 && echo finished`, then reply with exactly the word DONE."
PROMPT_LONG = "Use the Bash tool to run `ping -c 20 127.0.0.1` in the foreground, then reply with exactly the word DONE."


def scenario_observe(run):
    """Passive mod: what each side sees for a tool-using turn and a plain turn."""
    run.start(["--allowedTools", "Bash(date)", "--include-partial-messages"], with_mod=True)
    run.send("Use the Bash tool to run `date`, then reply with its output on one line.")
    run.wait_result(1)
    time.sleep(2)
    run.send("Reply with exactly the word TWO and nothing else.")
    run.wait_result(2)
    run.enqueue({"type": "snapshot"})
    run.log.wait(mod_event("spike.command", "done"), 10, label="snapshot")
    run.finish()


def scenario_control(run):
    """Mod answers permissions, aborts a running turn, and submits a turn."""
    run.start(["--permission-mode", "manual"], with_mod=True)
    run.send(PROMPT_PERMS)
    run.wait_result(1)

    time.sleep(2)
    start = len(run.log.records)
    run.send(PROMPT_SLEEP)
    hit = run.log.wait(
        mod_event("tool.call", "before", lambda r: bash_command(r).startswith("sleep")),
        90, start=start, label="tool.call sleep",
    )
    if hit:
        time.sleep(3)
        run.enqueue({"type": "abort"})
    run.wait_result(2, timeout=90)

    time.sleep(2)
    run.enqueue({
        "type": "submit",
        "text": "Reply with exactly the word THREE and nothing else.",
        "asUser": True,
    })
    run.log.wait(
        mod_event("turn.complete", "before", lambda r: "THREE" in str((r.get("data") or {}).get("answer"))),
        60, label="mod-submitted turn complete",
    )
    run.wait_result(3, timeout=30)

    time.sleep(2)
    run.send("Reply with exactly the word FOUR and nothing else.")
    run.wait_result(4, timeout=60)
    run.finish()


def scenario_abort(run):
    """Mod aborts a turn while a foreground tool runs; stdin continues after."""
    run.start(["--permission-mode", "manual"], with_mod=True)
    run.send(PROMPT_LONG)
    hit = run.log.wait(
        mod_event("tool.call", "before", lambda r: bash_command(r).startswith("ping")),
        90, label="tool.call ping",
    )
    if hit:
        time.sleep(3)
        run.enqueue({"type": "abort"})
    run.wait_result(1, timeout=90)

    time.sleep(2)
    run.send("Reply with exactly the word AFTER and nothing else.")
    run.wait_result(2, timeout=60)
    run.finish()


def scenario_mcp(run):
    """No mod: permissions via --permission-prompt-tool, interrupt via stdin."""
    config = {
        "mcpServers": {
            "perm": {
                "command": sys.executable,
                "args": [str(MCP_SERVER)],
                "env": {"SPIKE_RECEIVER_URL": run.url},
            }
        }
    }
    run.start([
        "--permission-mode", "manual",
        "--mcp-config", json.dumps(config),
        "--permission-prompt-tool", "mcp__perm__approve",
    ], with_mod=False)
    run.send(PROMPT_PERMS)
    run.wait_result(1)

    time.sleep(2)
    start = len(run.log.records)
    run.send(PROMPT_LONG)

    def long_tool_use(r):
        if r["src"] != "stdout" or r["msg"].get("type") != "assistant":
            return False
        for block in r["msg"].get("message", {}).get("content", []):
            if block.get("type") == "tool_use" and str(block.get("input", {}).get("command", "")).startswith("ping"):
                return True
        return False

    if run.log.wait(long_tool_use, 90, start=start, label="stdout tool_use ping"):
        time.sleep(3)
        run.send_raw({"type": "control_request", "request_id": "spike-int-1", "request": {"subtype": "interrupt"}})
    run.wait_result(2, timeout=90)

    time.sleep(2)
    run.send("Reply with exactly the word THREE and nothing else.")
    run.wait_result(3, timeout=60)
    run.finish()


PROMPT_STEPS = (
    "Use the Bash tool to run `ping -c 5 127.0.0.1` three times, one call after another, "
    "never in parallel and never in the background. After each call finishes, write the single "
    "word DONE-1, DONE-2 or DONE-3 before starting the next call. When all three are done, "
    "reply with exactly FINISHED."
)
PROMPT_PAUSE = (
    "Pause request from the operator: finish the step you are on, then stop. Do not start "
    "another step. Reply with one sentence saying where you stopped."
)
PROMPT_RESUME = "Resume the task from where you stopped and finish it."


def wait_idle(run, timeout=120):
    """Waits until every turn the mod saw start has a stdout result."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        started = run.log.count(mod_event("turn.start", "before"))
        results = run.log.count(lambda r: r["src"] == "stdout" and r["msg"].get("type") == "result")
        if started and results >= started:
            time.sleep(3)
            if run.log.count(mod_event("turn.start", "before")) == started:
                return results
        time.sleep(0.2)
    run.log.record("driver", note="timeout waiting for idle")
    return None


def pause_scenario(action):
    """Starts the three-step turn, applies `action` 1 s into the first ping,
    lets the session go idle, then resumes over stdin. `action` None is the
    uninterrupted baseline."""

    def scenario(run):
        run.start(["--allowedTools", "Bash(ping:*)"], with_mod=True, env={"SPIKE_QUIET": "1"})
        run.send(PROMPT_STEPS)
        hit = run.log.wait(
            mod_event("tool.call", "before", lambda r: bash_command(r).startswith("ping")),
            180, label="first tool.call ping",
        )
        if hit and action:
            time.sleep(1)
            action(run)
        n = wait_idle(run)
        if action and n:
            run.send(PROMPT_RESUME)
            run.wait_result(n + 1, timeout=120)
        run.finish()

    return scenario


def act_stdin(run):
    run.send(PROMPT_PAUSE)


def act_submit(run):
    run.enqueue({"type": "submit", "text": PROMPT_PAUSE, "asUser": True})


def act_append(run):
    run.enqueue({"type": "append", "text": PROMPT_PAUSE})


def act_abort_after_tool(run):
    run.enqueue({"type": "abort-after-tool"})


def act_interrupt(run):
    run.send_raw({"type": "control_request", "request_id": "spike-int-1", "request": {"subtype": "interrupt"}})


SCENARIOS = {
    "observe": scenario_observe,
    "control": scenario_control,
    "abort": scenario_abort,
    "mcp": scenario_mcp,
    "pause-baseline": pause_scenario(None),
    "pause-stdin": pause_scenario(act_stdin),
    "pause-submit": pause_scenario(act_submit),
    "pause-append": pause_scenario(act_append),
    "pause-abort-after-tool": pause_scenario(act_abort_after_tool),
    "pause-interrupt": pause_scenario(act_interrupt),
}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("scenario", choices=sorted(SCENARIOS))
    ap.add_argument("--out", type=Path)
    a = ap.parse_args()
    out = (a.out or HERE / "runs" / a.scenario).resolve()
    run = Run(a.scenario, out)
    try:
        SCENARIOS[a.scenario](run)
    finally:
        if run.proc and run.proc.poll() is None:
            run.proc.kill()
    print(f"events: {out / 'events.jsonl'}")


if __name__ == "__main__":
    main()
