# AGENTS.md

The orchestrator: a Go monorepo for a server and a daemon that run Claude
Code agents unattended, versioned with jujutsu.

Read before changing anything:

- `docs/conventions.md` — working conventions.
- `docs/roadmap.md` — what is decided and what is next.
- `docs/open-items.md` — known gaps.
- `docs/adr/` — decisions; immutable once accepted, supersede, never edit.
- `docs/design/` — dated findings; never revised.
- `README.md` — how things build and run.

Verify before reporting done:

    nix develop -c sh -c 'go build ./... && go test -count=1 ./... && gofmt -l .'
