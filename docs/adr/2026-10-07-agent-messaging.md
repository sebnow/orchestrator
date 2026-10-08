---
status: accepted
date: 2026-10-06
source: [initial brainstorm](../design/2026-10-06-brainstorm.md), decision 7
---

# Agent messaging

## Context

Agents can ask the server to spawn further agents. Placement is the
server's decision and invisible to the requesting agent. Agents run on
cloud VPSes and on the owner's laptop.

Surveyed multi-agent coordination patterns fall into two families:

- Blocking supervisor (Anthropic research system, OpenAI agents-as-tools,
  LangGraph supervisor). The parent waits and the result returns to it.
  These run in a single process, and none documents cross-machine
  placement.
- Asynchronous message passing (AutoGen actor model, Claude Code agent
  teams, claude-flow). Children are addressed by name or ID, results
  arrive as messages or via shared state, and the parent is not blocked.
  AutoGen and claude-flow support multiple machines, each through its own
  infrastructure: a gRPC runtime and message queues in AutoGen, SQLite
  plus JSON messaging in claude-flow. Claude Code teams exchange messages
  through JSON mailbox files under `~/.claude/teams/` and are documented
  for a single machine only.

Sources: https://www.anthropic.com/engineering/multi-agent-research-system,
https://code.claude.com/docs/en/agent-teams,
https://developers.openai.com/api/docs/guides/agents/orchestration,
https://microsoft.github.io/autogen/stable/user-guide/core-user-guide/index.html,
https://docs.langchain.com/oss/python/langgraph/use-graph-api,
https://github.com/ruvnet/claude-flow

## Decision

Agents communicate asynchronously through per-agent inboxes. Parents are
not blocked on children. Placement does not matter to the parent.

## Consequences

- The server routes messages between agents.
- How the inbox is exposed to the agent (an MCP tool, a Claude Code mod,
  or both) is not decided.
