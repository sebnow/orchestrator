---
status: accepted
date: 2026-10-08
source: owner decision, working session on 2026-10-08
---

# Permission policy

## Context

The harness can ask before it runs a tool, through the daemon's
`--permission-prompt-tool` ([human steering](2026-10-07-human-steering.md)).
The daemon sends each request to the server as a sequenced event, and
the request waits until an answer command names it.
[Client protocol](2026-10-07-client-protocol.md) says the server answers
by policy or hands the request to the owner, that policy is evaluated
on the server, that the policy engine is not decided, and that by
default a request waits indefinitely for an answer.

Before this record, no policy existed, so every request waited for the
owner to answer it in the GUI or the owner API. A task that asks while
the owner is away holds its harness process and its scheduling slot
until the owner returns, which defeats running agents unattended.

## Decision

The server consults a permission policy for each permission request as
it stores the request. The policy is one decision per request: allow,
deny with a message for the agent, or ask the owner. An answer the
policy gives travels as the same answer command the owner's would, so
the daemon and the protocol do not change. A request the policy leaves
to the owner waits as before.

Two policies exist, chosen by a server setting:

- allow everything, the default;
- ask the owner for everything, the behaviour before this record.

The transcript says who answered each request, the owner or the policy.

The policy engine remains undecided. This decision fixes only the
boundary a policy engine will plug into: one decision per request, made
on the server, from the task and the request's tool and input.

This record changes the default that the client protocol record names,
from waiting for the owner to allowing every request. It does not
supersede that record, whose transport, events and commands stand.

### Rejected alternatives

- Answering on the daemon. Client protocol places policy on the server,
  and a daemon-side answer would leave the server's record without the
  decision.
- Choosing a policy engine now. The owner wants unattended runs before
  the rules are worked out.

## Consequences

- With the default, an agent runs any tool the harness asks about,
  under the OS user that runs the daemon, with nobody approving it. The
  workspace and the daemon's machine are exposed to whatever the agent
  does.
- A request is decided once, when it is first stored. Changing the
  setting does not decide requests already waiting; the owner answers
  those.
- A policy's decision is part of the stored command log, so the record
  shows which answers came from the policy rather than the owner.

Revisit if a policy engine is chosen, or if allowing everything lets an
agent do damage the owner did not accept.
