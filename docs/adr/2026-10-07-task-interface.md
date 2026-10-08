---
status: accepted
date: 2026-10-06
source: [initial brainstorm](../design/2026-10-06-brainstorm.md), decision 5
---

# Task interface

## Context

Coordination logic lives in the server. The server places agents on
machines and routes messages between them. A client daemon on each
machine prepares the agent's workspace and spawns the harness, the Claude
Code CLI.

## Decision

A task is a prompt, plus an optional workspace specification, plus
optional model settings.

- When a task omits model settings, the server chooses them.
- The server owns the system prompt.
- A task may name a workspace: a repository and a ref. Most tasks are
  expected to use a repository. Without one, the daemon prepares an empty
  directory.
- Tasks are submitted through a web API.
