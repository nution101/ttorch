# orcha — contributor guide

orcha is a single Go binary that installs and orchestrates a team of Claude Code agents.

## Build & verify

```sh
make build    # ./bin/orcha
make test     # go test ./...
make lint     # go vet + gofmt check + vocabulary lint
make dist     # cross-compiled artifacts + checksums
```

Always run `make lint && make test` before committing.

## Layout

- `cmd/orcha` — entrypoint
- `internal/manifest` — clobber-safe content reconciliation (the core safety guarantee)
- `internal/installer` — maps embedded content into `~/.claude`/`~/.agents`; AGENTS/CLAUDE symlink
- `internal/selfupdate` — atomic binary replace + checksum verify + macOS quarantine handling
- `internal/doctor` — dependency detection + install
- `internal/paths`, `internal/buildinfo`, `internal/cli`
- `content/` — embedded payload (skills, agents, commands, global guidance)

## Conventions

- **Professional tone only.** Use neutral engineering language and the manager/worker/lead
  roles — no themed personas or role-play vocabulary.
- **Do not reference any specific company or employer** in code, content, or docs.
- Never clobber developer-edited files; surface conflicts as `.orcha-new` (see
  `internal/manifest`).
- The real binary lives in a user-owned dir (`~/.orcha/bin`) so macOS self-update works.

## Roadmap

See `ORCHA_PLAN.md` (one level up in the workspace, or the design doc) for milestones M0–M6.

<!-- BEGIN orcha-profile -->
Maintained by orcha (`orcha profile`). The repo's stack, commands, and
conventions — match these when changing code here.

- Stack: Go
- Build: `go build ./...`
- Test: `go test ./...`
- Lint/format: `gofmt -l .`, `go vet ./...`
- Tests: standard `testing` package; tests in *_test.go beside sources
- Layout: cmd/, content/, docs/, internal/
- Match the style of: internal/paths/paths.go, internal/tmux/tmux.go, internal/validate/validate.go
<!-- END orcha-profile -->
