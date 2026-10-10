---
status: accepted
date: 2026-10-10
source: owner decision, working session on 2026-10-10
---

# Server loss

## Context

The server keeps its whole record (every daemon, task, event and
command) in one SQLite database on its VM
([server storage](2026-10-07-server-storage.md)). The owner wants to
replace the VM by destroying it and starting a new server with the same
configuration, in a few steps and without losing that record. The VM
can also be lost without warning.

[SQLite backups](2026-10-10-sqlite-backups.md) has the server copy its
database with `VACUUM INTO` on a schedule and on demand, keep the
copies in a local directory and upload them to an S3-compatible bucket,
and leaves restore as a manual step with the server stopped. Backups
run on the schedule and on the owner's request only, and each upload is
the uncompressed copy. The owner keeps the bucket on Cloudflare R2
within its free tier, which bounds the bucket's storage.

Some of the record exists nowhere else. A daemon records each task's
events in a journal on its machine, ending with the event that says the
task's harness process exited. Once the server has acknowledged every
event up to that harness exit, the daemon deletes the journal
([task lifetime](2026-10-08-task-lifetime.md)), and from then on the
server's database holds the only copy of those events.

The server numbers all daemons' commands from a single sequence. A
daemon records the id of the last command it applied and, when it
reconnects, asks for the commands after that id
([client protocol](2026-10-07-client-protocol.md)). The sequence is stored
in the database, so a database restored from a backup issues ids from
the backup's last id onward. Those ids can be ones a daemon applied
before the restore, and a daemon asking for the commands after its id
would not receive the new commands.

[Daemon loss](2026-10-08-daemon-loss.md) declares a daemon lost when its
command stream is closed and the server has not seen it for the daemon
timeout, and counts any request of the daemon as seeing it.

This record amends three records, each at stated points, and
supersedes none:

- [SQLite backups](2026-10-10-sqlite-backups.md): when the server backs
  up; that uploads are gzipped; that a server restores on its own when
  it starts without a database; and that every restore, manual or at
  start, gunzips a gzipped backup and starts a new epoch. Its local
  copies, schedule, retention and the manual restore's checks stay as
  they are.
- [Client protocol](2026-10-07-client-protocol.md): a command's event id
  carries its epoch, the server resends after a daemon's last command by
  the rules below rather than by id alone, and the daemon ignores a
  command only when both its epoch and its id show it applied.
- [Daemon loss](2026-10-08-daemon-loss.md): what counts as seeing a
  daemon whose command stream the server refuses.

## Decision

### More backups

Besides its schedule and the owner's requests, the server backs up:

- after it stores a harness exit, because the daemon may then delete
  the task's journal;
- on graceful shutdown, after the command and transcript streams end
  and before the process exits.

One backup runs at a time. Triggers that arrive during a backup lead to
one more backup after it.

### Gzipped uploads

The server gzips each copy it uploads, so that the bucket's storage,
bounded by its free tier, holds more copies; the local copies stay
plain. Restore gunzips a gzipped backup and takes a plain one as it is,
so the plain uploads made before this record still restore and still
count towards the kept copies.

### Restore at start

A server configured with a bucket that finds no database file restores
the newest upload, gzipped or plain, before it opens the database, with
the manual restore's checks: SQLite's integrity check and a schema
version the server can open. It puts the restored database in place
whole or not at all. An existing database file is used as it is.

If the bucket is reachable and holds no upload, the server starts with
an empty database and logs that it did. If the bucket cannot be read,
or the download or a check fails, the server refuses to start, so that
a failed request to the bucket does not make it start empty.

### Command lineage

The database carries an epoch: a random id created with the database,
and replaced by a new one at every restore, manual or at start. A
database created before epochs gets its first epoch the first time a
server that knows epochs opens it. Each epoch after the first records
its parent, and every epoch when it was created, so the database holds
its own ancestry and its backups carry it.

A restored database issues commands only under its new epoch, even if
the process ends partway through the restore. The restore leaves a
durable mark before the restored database appears, and the server
starts the new epoch when it opens a marked database, before it issues
any command, and then clears the mark. A crash between starting the
epoch and clearing the mark makes the next start add one more epoch.
No command was issued under the abandoned one, and the lineage stays a
chain.

Each command records the epoch it was issued under. Ids still come from
the single sequence; a restored database carries it on from the
backup's last id, so ids can repeat across epochs. The command
stream's event ids and the daemon's Last-Event-ID carry both, as
`EPOCH:ID`. The daemon stores the epoch and id of the last command it
applied. It skips a command only when the command has that epoch and an
id at or below the stored id; it applies a command of any other epoch,
and stores that command's epoch and id.

When a daemon opens its command stream, the server decides by the epoch
the daemon reports:

- The server's current epoch: the daemon's id must be at or below the
  last id the server has issued in that epoch. An id past it means the
  daemon's state does not match this database, so the server refuses
  the stream with the reason `daemon_ahead`.
- An earlier epoch of the server's lineage: the server was restored. It
  sends that epoch's commands past the daemon's id, then the commands of
  each later epoch, oldest epoch first.
- An epoch outside the lineage: the daemon's state comes from elsewhere,
  or the daemon applied commands of an epoch that the restored backup
  predates, as when a second restore uses a backup taken before the
  first. The server refuses the stream with the reason
  `unknown_epoch`, and the owner resets the epoch and command id the
  daemon stores by hand, as the
  [README](../../README.md#command-ids-and-epochs) describes.

The owner's daemon list shows each refusal and since when the daemon's
streams have been refused.

A daemon that applied its last command before commands had epochs
reports a bare id. The server reads it as an id in the first epoch,
because every command issued before epochs existed belongs to that
epoch. The owner upgrades the server and the daemons together, so a
bare id is expected only from a daemon's record saved before the
upgrade.

### A refused daemon is unseen

A daemon whose command stream the server refuses counts as unseen from
the first refusal, whatever else it sends, so after the daemon timeout
it is lost and its tasks move as a lost daemon's do. It stays lost
while its streams are refused, and is seen again once the server
accepts one.

### Rejected alternatives

- Litestream. It is a second process to deploy and supervise beside the
  server, and it keeps a continuous replica rather than the discrete,
  dated copies that the local directory, the retention and the manual
  restore are built on.
- Cloudflare Durable Objects. Moving the server's state there is a
  rewrite of the server, Go is not a first-class language there, and
  the long-lived SSE streams would be billed for their duration.
- Raising the command sequence past the id a daemon reports. Any daemon
  could then move the sequence for every daemon, and a daemon reporting
  an id near the largest the sequence allows would exhaust it.
- UUIDv7 command ids. Every message that carries a command id would
  change type, and the ids' order would depend on the issuing machine's
  clock unless a monotonic generator were seeded from the database.

## Consequences

- A VM lost without a graceful shutdown loses what the server stored
  since its last successful backup: up to one interval's worth while
  backups succeed, less where a harness exit triggered a backup sooner,
  and more after backups have failed.
- A harness exit triggers a backup, but the daemon may delete the
  journal before that backup completes. Losing the VM in that window
  loses the task's last events.
- A restored database is behind the daemons. On reconnecting, a daemon
  learns how far the restored server holds each task and resends every
  later event still in its journal. Tasks created after the backup are
  unknown to the restored server, which refuses their events; the
  daemon stops sending them and they stay in its journal.
- After a restore, a daemon receives the new epoch's commands even when
  their ids repeat ones it applied before the restore.
- A daemon refused with `unknown_epoch` or `daemon_ahead` takes no
  commands until its stored epoch and id are corrected, and after the
  daemon timeout its tasks move to other daemons, starting their work
  again there.
- Harness exits make backups more frequent, and since the server keeps
  a fixed number of copies, they span less time. Each upload holds the
  whole database, gzipped.
- Recreating the VM means starting the server with its bucket
  configured first. Anything that creates the database file before
  that, such as the `issue-owner-token` command, which opens the
  database, or a start without the bucket, creates an empty database,
  and the server then does not restore.

Revisit if the event stream moves to a Nostr relay
([Nostr direction](../design/2026-10-09-nostr-direction.md)), if a
backup takes a large part of the interval to upload, if command ids are
to be numbered per task's lineage, as each task's events are numbered by
task, or if a restored server is to recover the tasks it does not know
from the daemons' journals and the task records in their state files.
