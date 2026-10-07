# Client connectivity

- Status: Accepted
- Decided: 2026-10-06
- Source: [initial brainstorm](../design/2026-10-06-brainstorm.md),
  decisions 2 and 3

## Context

The system has three parts. The server runs on a VPS, accepts tasks,
stores sessions and transcripts, and serves a web interface and API for
the owner, the single person who runs the server and the client machines.
A client daemon runs on each machine that executes agents. The harness is
the Claude Code CLI process that the daemon spawns.

Agents run on cloud VPSes and on the owner's laptop. The owner needs one
place to steer them. Reaching each machine directly brings NAT and
per-machine exposure problems.

## Decision

- The client daemon dials out to the server.
- All clients speak the same protocol to the server, and the owner steers
  every agent through the server's web interface.
- The owner provisions each machine, installs the daemon, and points it at
  the server. Server-driven provisioning is deferred.

## Consequences

- Machines behind NAT can take part, and only the server needs to be
  exposed.
- Any machine that can reach the server can run agents, including the
  owner's laptop.
- Authentication between client and server is not decided.
  Certificate-based authentication is preferred, because a server that
  provisions machines could issue each certificate when it builds the
  machine. Client-server connections must work without a VPN or private
  network.
