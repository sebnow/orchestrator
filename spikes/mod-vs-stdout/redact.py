"""Redacts personal identifiers from run logs in place before they are kept.

Replaces e-mail addresses, every UUID that appears as an account or
organization id anywhere in the logs (including where the same UUID shows up
inside a path), the telemetry device id, and the auth handle on fetches.

Usage: python3 redact.py runs/*/events.jsonl runs/*/stdout.jsonl
"""

import re
import sys

EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+")
ID_FIELD = re.compile(r'(account_uuid|organization_uuid)\\*"\s*:\s*\\*"([0-9a-f-]{36})')
DEVICE_ID = re.compile(r'(device_id\\*"\s*:\s*\\*")[0-9a-f]+')
AUTH_HANDLE = re.compile(r"auth_[0-9a-f]{32}")


def main():
    paths = sys.argv[1:]
    texts = {p: open(p).read() for p in paths}
    ids = set()
    for t in texts.values():
        ids.update(m.group(2) for m in ID_FIELD.finditer(t))
    for p, t in texts.items():
        t = EMAIL.sub("<redacted-email>", t)
        for i in ids:
            t = t.replace(i, "<redacted-id>")
        t = DEVICE_ID.sub(r"\1<redacted-id>", t)
        t = AUTH_HANDLE.sub("auth_<redacted-id>", t)
        open(p, "w").write(t)
    print(f"redacted {len(paths)} files; {len(ids)} ids")


if __name__ == "__main__":
    main()
