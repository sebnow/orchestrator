---
status: accepted
date: 2026-10-10
source: >-
  owner decision, 2026-10-10: in-process, no external tool
---

# SQLite backups to object storage

## Context

The server's store is SQLite (`modernc.org/sqlite`,
`internal/server/store.go`, [server storage](2026-10-07-server-storage.md)),
and the server will run on a VPS rather than on a machine the owner
backs up by other means. The database holds the record of every task:
its events, the command log, and now the daemons' facts; a daemon's
login state lives on the daemon's own machine, not in it, but the
Hetzner API token a provisioning server needs
([VPS provisioning](2026-10-10-vps-provisioning.md)) is kept outside the
database too. The module carries no AWS SDK or other object-storage
client today, and the owner wants none added.

## Decision

The server writes a consistent copy of its database with `VACUUM INTO`,
on a schedule and on demand from the GUI, and uploads it to an
S3-compatible bucket. The upload is `PUT`, signed with a stdlib
implementation of AWS Signature Version 4; `GET` and `LIST`, signed the
same way, support restore and let the server enforce a configured
number of kept copies by deleting the oldest. The dashboard shows the
time and result of the last backup.

Restore is a documented manual step: download the chosen copy, stop the
server, and replace its database file with it.

## Consequences

- A backup carries the command log, with login codes blanked, and every
  task's transcript; anyone who can read the bucket can read them.
- The bucket's endpoint and credentials are server configuration, kept
  outside the database alongside the Hetzner token.
- The SigV4 implementation is small enough to keep in the server and is
  tested against the chosen provider's bucket, not only against
  recorded requests.

Revisit if a second object-storage provider with an incompatible
signing scheme is needed, or if restore needs to run without stopping
the server.
