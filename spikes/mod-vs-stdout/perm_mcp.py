"""Minimal stdio MCP server exposing `approve`, for --permission-prompt-tool.

It forwards each permission request to the driver's /decide endpoint so the
same policy answers both routes, and logs the request there as well.
"""

import json
import os
import sys
import urllib.request

RECEIVER = os.environ["SPIKE_RECEIVER_URL"]

TOOL = {
    "name": "approve",
    "description": "Decides whether Claude Code may run a tool call.",
    "inputSchema": {
        "type": "object",
        "properties": {
            "tool_name": {"type": "string"},
            "input": {"type": "object"},
            "tool_use_id": {"type": "string"},
        },
        "required": ["tool_name", "input"],
    },
}


def ask_receiver(args):
    req = urllib.request.Request(
        RECEIVER + "/decide",
        data=json.dumps({"route": "mcp", "tool": args.get("tool_name"), "input": args.get("input"),
                         "tool_use_id": args.get("tool_use_id"), "raw": args}).encode(),
        headers={"content-type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read() or b"{}")


def handle(msg):
    method = msg.get("method")
    if method == "initialize":
        return {
            "protocolVersion": msg["params"].get("protocolVersion", "2025-06-18"),
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "perm", "version": "0.1.0"},
        }
    if method == "tools/list":
        return {"tools": [TOOL]}
    if method == "tools/call":
        args = msg["params"].get("arguments") or {}
        answer = ask_receiver(args)
        if answer.get("decision") == "allow":
            verdict = {"behavior": "allow", "updatedInput": args.get("input") or {}}
        else:
            verdict = {"behavior": "deny", "message": answer.get("reason", "No decision from the receiver.")}
        return {"content": [{"type": "text", "text": json.dumps(verdict)}]}
    if method == "ping":
        return {}
    return None


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        msg = json.loads(line)
        if "id" not in msg:
            continue
        result = handle(msg)
        if result is None:
            reply = {"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32601, "message": "method not found"}}
        else:
            reply = {"jsonrpc": "2.0", "id": msg["id"], "result": result}
        sys.stdout.write(json.dumps(reply) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
