# Transcripts

- Status: Accepted
- Decided: 2026-10-06
- Source: [initial brainstorm](../design/2026-10-06-brainstorm.md),
  decision 13

## Context

The server stores agent sessions and their transcripts centrally.

## Decision

Transcripts are kept centrally as a record and are later fed back into
agent sessions as input. Memory shared across agents, the context and
knowledge gained from each session, is outside this decision and
deferred.
