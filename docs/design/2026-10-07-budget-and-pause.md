# Budget, priority, and graceful pause (2026-10-07)

Idea noted on 2026-10-07, not yet decided. Follows the brainstorm of
2026-10-06 (decision 8: subscription billing; open question: subscription
concurrency). To be refined into ADRs when the scheduling design is taken up.

## Idea

The subscription has a limited token budget per five-hour and weekly window.
Managing that budget is a core job of the orchestrator, not an afterthought.

- **Priority.** Tasks carry a priority. When budget is scarce, higher
  priority tasks run first and lower ones wait.
- **Filler tasks.** Some tasks exist only to use budget that would otherwise
  go unspent before the window resets. They run when nothing of higher
  priority is runnable and yield as soon as something is.
- **Graceful pause.** A task or thread can be paused without halting the
  harness mid-turn. The agent is told to finish what it is doing and stop at
  a clean point, so the tokens already spent on the current step are not
  wasted. Resumption continues from that point. An abrupt halt remains
  available for emergencies.

## Facts to establish

- How the harness reports remaining quota, if at all. `rate_limit_event`
  was observed on stdout but is undocumented (brainstorm, Research notes:
  Claude Code headless interface).
- Whether a pause can be delivered as a follow-up prompt over stdin (the
  mechanism confirmed in the brainstorm's stdin experiment) or needs a
  harness-level mechanism.
- What "a clean point" means for the agent: end of the current tool call,
  end of the current turn, or a point the agent chooses.
