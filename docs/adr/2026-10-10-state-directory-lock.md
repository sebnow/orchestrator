---
status: accepted
date: 2026-10-10
source: >-
  owner decision, 2026-10-10: the lock waits with a timeout rather than
  refusing at once
---

# State directory lock

## Context

The harness-user container check ([real-claude
findings](../design/2026-10-09-harness-user-real-claude.md) era) once ran
two daemons on the same state directory: a SIGKILL aimed at the daemon
hit a subshell instead, the daemon it had started ran on, and the
restarted daemon started beside it. Each kept its own sequence counter,
so the two daemons' journals collided and the server kept whichever
event arrived first (commit `test/harness-user: start the daemon so
that SIGKILL reaches it`, which found and fixed the check's own bug,
not the daemon's).

A daemon's state directory ([task lifetime](2026-10-08-task-lifetime.md),
[shutdown recovery](2026-10-08-shutdown-recovery.md)) holds the one
record of what each of its tasks needs to be resumed and the per-task
journals that back the protocol's replay
([client protocol](2026-10-07-client-protocol.md)). Two daemons writing
it at once is the failure above, not a case the protocol's sequence
numbers were designed to survive. On a restart, the previous daemon may
still be flushing: shutdown recovery gives a restarted daemon up to the
shutdown timeout, 30 seconds by default, to wait for a harness process
it finds still running before killing it, and a supervisor such as `jj`
restarting the daemon itself can start the new one before the old one
has let go of the directory.

## Decision

The daemon holds an exclusive `flock` on `<state-dir>/lock` for as long
as it runs. A second daemon started on the same state directory waits
for the lock, retrying every 250 ms and logging once that it is
waiting, for a configurable timeout, `Config.LockTimeout`, before
refusing to start with "state directory … is still in use by another
daemon after …". The default is the shutdown timeout plus 30 s, the
same wait shutdown recovery gives an old harness process to exit, so
that a daemon's own restart does not race its predecessor's shutdown.

## Consequences

- A supervisor that restarts the daemon, in the manner of `jj`'s own
  working-copy restarts, needs no flag: the new process waits out the
  old one's shutdown and then starts.
- The lock is released by the kernel when the process holding it exits,
  including on a crash, so no stale lock file can block a later start.
- `flock` is not relied on across a network filesystem; the state
  directory is assumed local to the daemon's machine.
- The README's `-state-dir` paragraph already documents the lock and
  the wait.

Revisit if a daemon needs to share a state directory with another by
design, or if the default timeout proves too short for a shutdown under
real load.
