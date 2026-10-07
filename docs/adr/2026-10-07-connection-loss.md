# Connection loss

- Status: Accepted
- Decided: 2026-10-06
- Source: [initial brainstorm](../design/2026-10-06-brainstorm.md), decision 4

## Context

The client daemon dials the server
([client connectivity](2026-10-07-client-connectivity.md)) and streams
each agent session to it live. Agents run unattended, and the server keeps
their transcripts. The connection between a daemon and the server can
drop.

## Decision

An agent survives a lost connection. The daemon keeps the harness running,
buffers transcript events while disconnected, and replays them on
reconnect.
