"""Prints a one-line-per-record timeline of a run's events.jsonl.

Usage: python3 timeline.py runs/<scenario>/events.jsonl [--all]
Without --all, partial-message stream events, per-tool and per-command
description events, and the $ calls built-in mods make are counted rather
than listed.
"""

import json
import sys
from collections import Counter

NOISY_MOD = {"tool.describe", "prompt.section", "prompt.compose", "prompt.context", "prompt.attachment",
             "agent.offer", "command.describe", "skill.prompt"}


def brief(r):
    src = r["src"]
    if src == "stdout":
        m = r["msg"]
        kind = m.get("type", "?")
        sub = m.get("subtype") or (m.get("event", {}) or {}).get("type") or ""
        extra = ""
        if kind == "assistant":
            parts = []
            for b in m.get("message", {}).get("content", []):
                if b.get("type") == "text":
                    parts.append("text=" + repr(b["text"][:60]))
                elif b.get("type") == "tool_use":
                    parts.append(f"tool_use {b['name']} {json.dumps(b.get('input'))[:80]}")
                else:
                    parts.append(b.get("type"))
            extra = "; ".join(parts)
        elif kind == "user":
            for b in m.get("message", {}).get("content", []) if isinstance(m.get("message", {}).get("content"), list) else []:
                if b.get("type") == "tool_result":
                    extra = f"tool_result is_error={b.get('is_error')} {str(b.get('content'))[:80]!r}"
        elif kind == "result":
            extra = f"result={m.get('result')!r} cost={m.get('total_cost_usd')} usage.out={m.get('usage', {}).get('output_tokens')}"
        elif kind == "rate_limit_event":
            extra = json.dumps({k: v for k, v in m.items() if k not in ("uuid",)})[:200]
        elif kind == "system":
            extra = json.dumps({k: m[k] for k in m if k not in ("type", "subtype", "uuid", "session_id", "tools", "slash_commands", "skills", "agents")})[:160]
        return f"stdout {kind}/{sub} {extra}"
    if src == "mod":
        d = r.get("data")
        s = json.dumps(d)[:160] if d is not None else ""
        return f"mod    {r.get('event')}:{r.get('phase')} lat={r.get('latency_ms')} {s}"
    if src == "stdin":
        return f"stdin  {json.dumps(r['msg'])[:160]}"
    if src == "receiver":
        return f"recv   {r.get('action')} {json.dumps(r.get('answer') or r.get('command') or r.get('commands'))[:160]}"
    if src == "stderr":
        return f"stderr {r['line'][:200]}"
    return f"driver {json.dumps({k: v for k, v in r.items() if k not in ('n', 't', 'rel', 'src')})[:200]}"


def main():
    path = sys.argv[1]
    show_all = "--all" in sys.argv
    skipped = Counter()
    for line in open(path):
        r = json.loads(line)
        if not show_all:
            if r["src"] == "stdout" and r["msg"].get("type") == "stream_event":
                skipped["stdout stream_event/" + r["msg"].get("event", {}).get("type", "?")] += 1
                continue
            origin = (r.get("origin") or {}).get("plugin")
            if r["src"] == "mod" and origin not in (None, "engine"):
                skipped[f"mod {r.get('event')}:{r.get('phase')} from {origin}"] += 1
                continue
            if r["src"] == "mod" and r.get("event") in NOISY_MOD:
                skipped[f"mod {r.get('event')}:{r.get('phase')}"] += 1
                continue
            if r["src"] == "mod" and r.get("event") == "turn.step" and r.get("phase") == "chunk":
                skipped[f"mod turn.step:chunk/{(r.get('data') or {}).get('kind')}"] += 1
                continue
        print(f"{r['rel']:>9.1f} {brief(r)}")
    if skipped:
        print("\nCounted, not listed:")
        for k, v in sorted(skipped.items()):
            print(f"  {v:4d}  {k}")


if __name__ == "__main__":
    main()
