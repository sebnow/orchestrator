# Server storage

- Status: Accepted
- Decided: 2026-10-06
- Source: [initial brainstorm](../design/2026-10-06-brainstorm.md),
  decision 11

## Context

The server stores sessions and transcripts. It serves one user and a
handful of agents, and runs as one binary on a VPS.

## Decision

The server stores its data in SQLite.

Revisit if SQLite becomes the bottleneck; Postgres is the replacement.
