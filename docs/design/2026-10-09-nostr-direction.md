# A Nostr direction, deferred (2026-10-09)

This note records a direction the owner may take the orchestrator in:
Nostr events as its wire format, after the model of Buzz (see
Research). Nothing here is decided. It is not an architecture decision
record, and the code does not follow it. It keeps the owner's statements, the
coordinator's reading of them, and the scout's research, as they stood
on 2026-10-09, so that a later decision can start from them.

Markers: **UNVERIFIED** flags a claim not checked against a source.
The research section reports the scout's findings of 2026-10-08 with
the sources it gave; this note did not check them again.

## The owner's intent

- Base the orchestrator on Buzz's model rather than integrate with
  Buzz.
- Nostr events as the wire format is deferred, not out of scope.
- Each client, that is each daemon, has its own Nostr private key as
  its identity. The daemon may expose that key to the agent, because
  the key is the client's own.
- The server allow-lists daemons' public keys.
- Messages must not reach the public network, so the orchestrator runs
  its own relay, which in effect reimplements Buzz.

## The owner's answers

- **Deployment.** A single process, for simplicity of deployment, with
  the possibility of providing a standalone relay later.
- **Identity.** Either the server's own key, or the user's identity
  relayed, where the user's key might also be the GUI login. This is
  undecided.

## The coordinator's reading

The coordinator drew the following from the owner's statements.

- The relay should be a package behind one interface, used in-process
  or remotely, so that the single process and a later standalone relay
  share it.
- Daemons allow-list two authorities: the owner's key, which signs
  intent, and the server's key, which signs what the server derives
  from it, such as admissions and policy answers.
- The relay replaces the POST batches of events and the SSE command
  stream, with their per-task seq and acknowledgements, and the
  command log ([client protocol](../adr/2026-10-07-client-protocol.md)).
- It keeps the harness adapter, the task model, the scheduler, the GUI
  and branch delivery.
- Costs: neither websockets nor BIP-340 Schnorr signatures are in the
  Go standard library. A Nostr event's `created_at` is in whole seconds
  and set by its publisher, so it does not order a task's events, and
  the per-task seq survives as a tag on each event.
- The gain is commands the owner signs, which daemons can verify
  without trusting the server.

## Research

The scout reported the following on 2026-10-08.

- **Buzz.** Buzz (Block Inc, <https://github.com/block/buzz>, July
  2026) stores NIP-34 events in PostgreSQL on a relay per workspace:
  30617 repository announcement, 30618 state with branch refs, 1617
  patch, 1618 pull request, 1621 issue, and 1630 to 1633 status. It
  keeps git objects in S3-compatible storage. Clients push over git's
  smart HTTP, authenticated by `git-credential-nostr`, which signs a
  NIP-98 event with an nsec from `$NOSTR_PRIVATE_KEY` or
  `~/.nostr/key` (<https://engineering.block.xyz/blog/run-your-own-buzz-relay>,
  <https://nips.4rs.nl/nips/34>).
- **ngit and gitworkshop.dev.** These use a `git-remote-nostr` helper
  with `nostr://` remotes; `pr/` branches become kind-1618 events, and
  credentials are kept in a keyring (<https://lib.rs/crates/ngit>).
  **UNVERIFIED:** whether the helper works non-interactively.
- **Delegation.** NIP-26 delegation scopes a sub-key by event kind and
  expiry, not by repository (<https://nips.4rs.nl/nips/26>).
- **Remote signing.** NIP-46 remote signing is the equivalent of
  ssh-agent (<https://nips.4rs.nl/nips/46>). **UNVERIFIED:** whether
  `git-credential-nostr` supports NIP-46.

## Addendum: tool inheritance

This section is unrelated to the Nostr direction. It is recorded here
because an accepted record is not amended (README, "Documents").
[Agents and placement](../adr/2026-10-09-agents-and-placement.md) says
that a child that names no agent "is allowed both agent tools, and
inherits from its parent as the Context describes" (the record's
Context section). From 2026-10-09 the server narrows that: a child
spawned without an agent is allowed exactly the gateway tools its
parent is allowed, and a child spawned as an agent is allowed those of
the agent's tools that its parent is also allowed, so no child may
call `spawn_task` or `send_message` when its parent may not. The
server follows this narrower rule from change `wnqqkzvv`; the record's
text predates it.
