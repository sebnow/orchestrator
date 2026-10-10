# Harnesses per daemon (2026-10-10)

The owner's direction, not yet decided: the daemon is the host, one per
machine, and a harness is an entity the daemon hosts. At the time of
writing a daemon runs one harness, Claude Code, and much of the
protocol and the server treats "the daemon" and "its harness" as one
thing. This note lists where that coupling sits and what a second
harness adapter would change, so that a later decision record can
settle whether to adopt the direction.

**UNVERIFIED** marks a claim inferred from, rather than read in, the
referenced code.

## The direction

A daemon hosts harnesses. Each harness has:

- a name and a version;
- the models it offers;
- a login and the account it is logged in to;
- a mapping from the neutral tool classes to its own tools;
- capabilities, such as whether it can report its login or list its
  models.

Machine facts (memory, CPUs, slots, ssh key, host keys) stay on the
daemon; the per-harness properties listed above move to the harness.

## Current coupling

- One set of harness facts per daemon. `harness` and `harness_version`
  (`internal/protocol/facts.go:26-27`), `login` and `login_method`
  (`:57-58`) and `account` (`:65`) are single keys in the daemon's
  facts map; `detectFacts` fills the first two from the one harness's
  `Info()` (`internal/daemon/facts.go:28`), and `readLogin` fills the
  login facts from the one harness's `LoginStatus`
  (`internal/daemon/login.go:37-44`).
- One `models` label. `FactModels` (`internal/protocol/facts.go:42`)
  is a single `;`-separated list per daemon, merged with the owner's
  label of the same key (`internal/server/models.go:49`).
- One harness in the daemon's configuration. `daemon.Config` has a
  single `Harness harness.Harness` (`internal/daemon/serve.go:47`), and
  the daemon command has one `-claude` path
  (`cmd/daemon/main.go:62`), from which it builds the one adapter
  (`cmd/daemon/main.go:162`).
- `StartTask` names a model but not a harness. It carries `Model`
  (`internal/protocol/command.go:86`); the daemon runs it on its one
  harness.
- Login commands are addressed to the daemon. `login` and `login_code`
  (`internal/protocol/command.go:41,43`) are daemon commands that name
  neither a task nor a harness; the daemon runs the login of its one
  harness (`internal/daemon/login.go`, `startLogin`). The server keeps
  one login per daemon (`internal/server/login.go`, `Server.logins`).
- Placement reads one harness per daemon. `schedule.serves` matches an
  agent's model entry against the daemon's `harness` fact and models
  (`internal/server/scheduler.go:488`, `internal/server/models.go:65`),
  and the scheduler skips a daemon whose single `login` fact is `no`
  (`internal/server/scheduler.go:367`).

## Already shaped for several harnesses

- The budget key is (harness, account), or the daemon when no account
  is reported (`internal/server/budget.go:19-37`). `budgetKeyOf` reads
  the single `harness` and `account` facts, so it would separate two
  harnesses' budgets once those facts are per harness.
- Agent model entries can be qualified as `harness:model`
  (`internal/server/agents.go:22`, `internal/server/models.go:59-74`),
  so an agent can already say which harness a model is meant for.
- Events carry the harness's name and version per event
  (`protocol.Harness`, `internal/protocol/protocol.go:137`, from
  `Harness.Info()`), not per daemon.
- Each adapter owns its tool-class mapping
  (`internal/harness/claude/tools.go:19`), so a second adapter brings
  its own.

## What changes with a second adapter

These are proposals inferred from the code above; none has been
implemented or tested.

- Facts per harness, under a naming scheme such as
  `harness.<name>.version`, `harness.<name>.login`,
  `harness.<name>.account` and `harness.<name>.models`, or a structured
  fact the server parses. Label keys are 1 to 63 ASCII letters, digits,
  `.`, `_` and `-` (`internal/server/labels.go:22-34`), so a scheme that
  puts harness names in keys limits names to those characters and to
  the length the rest of the key leaves.
- `StartTask.Harness`, naming which of the daemon's harnesses runs the
  task; empty could mean the daemon's only harness, to keep older
  servers and daemons working.
- Placement over (daemon, harness, model) rather than (daemon, model):
  `serves` would match an entry against each harness's models, and the
  login check would be per harness.
- Login and login status per harness: the `login` command gains a
  harness, the server keeps one login per (daemon, harness), and the
  daemon page's login section (`internal/component/daemon.go:88`,
  `DaemonLoginSection`) appears once per harness.
- The daemon's configuration lists its harnesses with their paths,
  replacing `-claude` with, for example, a repeated `-harness
  name=path` flag or a configuration file.
- Budgets would need the per-harness harness and account facts; the
  (harness, account) key itself stays the same.

## Not covered

This note does not cover whether two harnesses on one daemon share
slots, whether they share the OS user that runs harnesses
(`-harness-user`) and its home directory for their logins, or how the
provisioning user data installs a second harness.
