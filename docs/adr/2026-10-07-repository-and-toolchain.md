# Repository and toolchain

- Status: Accepted
- Decided: 2026-10-06
- Source: [initial brainstorm](../design/2026-10-06-brainstorm.md),
  decision 12

## Context

The server and the client daemon run on different machines, including
cloud VPSes and the owner's macOS laptop. Every machine needs to build
them the same way. The Claude Agent SDK is available only for TypeScript
and Python.

## Decision

- The server and the client daemon are written in Go.
- All components live in one monorepo.
- Jujutsu is used for version control.
- A Nix flake dev shell pins Go and the development tools, so builds run
  in the dev shell use the same toolchain on every machine.
- Decisions are recorded as ADRs in `docs/adr/`.

## Consequences

- The daemon cannot use the Claude Agent SDK, so it drives the Claude Code
  CLI process directly
  ([harness integration](2026-10-07-harness-integration.md)).
