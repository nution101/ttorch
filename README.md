# orcha

Run a team of Claude Code agents. You act as the **manager**: plan the work, delegate to
isolated **worker** sessions, review the results, and approve delivery — instead of
writing and reviewing every line by hand.

> **Status: M0 (foundation/distribution).** This milestone ships the install/update
> machinery, the global Claude Code surface, and dependency setup. The orchestration
> runtime (tmux sessions, the supervisor daemon, worker dispatch) lands in later
> milestones. See `ORCHA_PLAN.md` for the full architecture and roadmap.

## Install

```sh
# macOS / Linux / WSL2
curl -fsSL https://nution101.github.io/orcha/install.sh | sh
```

```powershell
# Windows (basic; use WSL2 for the full experience)
irm https://nution101.github.io/orcha/install.ps1 | iex
```

Or build from source (requires Go):

```sh
git clone https://github.com/nution101/orcha && cd orcha
make install      # builds into ~/.orcha/bin, links into ~/.local/bin, lays content
orcha doctor      # check/installs tmux, git, gh, claude
```

## Commands (M0)

| Command | Description |
| --- | --- |
| `orcha install` | Install/update managed skills, agents, and global guidance |
| `orcha update [--content-only]` | Self-update the binary, then re-apply content |
| `orcha uninstall [--purge]` | Remove managed files (keeps files you edited) |
| `orcha doctor [--yes]` | Detect and install missing dependencies |
| `orcha version` | Print version |

## How updates stay safe

`orcha update` adds newly shipped skills and upgrades files you have not touched, but it
**never overwrites a file you edited**. Your version is kept; the new one is parked beside
it as `<name>.orcha-new` and reported. A per-file sha256 manifest (`~/.orcha/manifest.json`)
distinguishes "orcha wrote this and it's unchanged" from "the developer changed it". Your
task state under `~/.orcha/state` and `~/.orcha/data` is never touched by updates.

## What gets installed

```
~/.orcha/bin/orcha                 # the binary (user-owned, for safe self-update)
~/.orcha/manifest.json             # ledger of managed files
~/.claude/skills/orcha-manager/    # the manager role
~/.claude/agents/orcha-worker.md   # the worker brief contract
~/.claude/commands/orcha.md        # the /orcha slash command
~/.claude/AGENTS.md                # managed guidance block; CLAUDE.md symlinks to it
~/.agents/skills/orcha-manager/    # vendor-neutral mirror
```

## Development

```sh
make build    # build ./bin/orcha
make test     # go test ./...
make lint     # go vet + gofmt check + vocabulary lint
make dist     # cross-compile + checksums into ./dist
```

Contributions keep a professional tone (no role-play vocabulary); `scripts/lint-vocab.sh`
enforces this in CI.

## License

MIT — see `LICENSE`. Later milestones vendor code from upstream MIT projects; see
`THIRD_PARTY.md`.
