---
status: accepted
date: 2026-10-10
source: >-
  owner decision, 2026-10-10 (Q20b): host keys distributed by the
  server, trust on first contact rejected; no manual steps on a client
---

# Forge host keys from the server

## Context

A fresh VPS has no `known_hosts`, and the daemon pushes with `ssh -o
BatchMode=yes`, which refuses a host whose key it cannot verify rather
than asking.

[Work delivery](2026-10-08-work-delivery.md) says, in its Decision,
under Credentials, that "git runs with the user's and the system's git
configuration ignored and its prompts turned off, so that the owner's
URL rewrites and credential helpers do not apply; ssh authentication
comes from the daemon user's own ssh setup, which git uses regardless
of its configuration." [Daemon push identity](2026-10-10-daemon-push-identity.md)
later gave that ssh setup a shape of its own: the daemon's generated
key, used from its mirror, rather than whatever was assumed present.
Neither record gives a fresh daemon a way to verify a forge's host key,
and trust on first contact was considered and rejected: a daemon's first
connection to a forge would then trust whatever key the forge, or an
attacker on the path to it, presents.

## Decision

The server keeps the forges' host keys as a setting, built from two
sources:

- the owner's: `known_hosts` lines pasted on the Settings page, checked
  for shape (a valid key of the type it names) and refused, naming the
  line, if a line is not;
- GitHub's: fetched from `https://api.github.com/meta` at start and
  every 24 hours, retried an hour after a failed fetch, and kept across
  a restart without network access until a fetch succeeds.
  `-github-meta-url` names the URL; empty turns the fetch off.

The server sends their union to every daemon as the daemon-level command
`host_keys`, at connect and whenever the union changes. The daemon
writes the lines to `<state-dir>/known_hosts`, mode 0600, replacing the
file, and runs ssh with `StrictHostKeyChecking=yes` and
`UserKnownHostsFile` naming that file. In single-user mode the daemon
also keeps the daemon user's own `~/.ssh/known_hosts`, which that user's
ssh consults as usual. The system `/etc/ssh/ssh_known_hosts` stays
consulted in either mode.

This amends [work delivery](2026-10-08-work-delivery.md)'s Credentials
clause, quoted above: ssh authentication is still "the daemon user's own
ssh setup," now [daemon push identity](2026-10-10-daemon-push-identity.md)'s
generated key, but host verification is no longer whatever that setup
happens to already trust; it comes from the server instead, over the
mTLS channel every daemon already has.

## Consequences

- A daemon that connects before the server has any keys set is sent
  none, and its ssh fails host verification until the owner sets keys
  and the daemon reconnects or is sent a `host_keys` command. This is
  the upgrade race: a start queued before the daemon's first
  `host_keys` command may run its push with no `known_hosts` yet.
- GitHub's keys are trusted through the system's TLS roots that verify
  `api.github.com`, not pinned against a known fingerprint.
- A key rotated at a forge reaches daemons within a day, or within an
  hour of the next fetch after a failure, for GitHub; immediately, once
  the owner pastes it, for every other forge.
- One command kind, `host_keys`, is added to the protocol for this.

Revisit if trust on first contact is wanted after all, or if a forge's
host keys should be fetched the way GitHub's are.
