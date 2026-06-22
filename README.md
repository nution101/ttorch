# orcha

Run a team of Claude Code agents. You act as the **manager**: plan the work, delegate to
isolated **worker** sessions, review the results, and approve delivery — instead of
writing and reviewing every line by hand.

> **Status: M0–M5.** Installs/updates safely, ships the global Claude Code surface,
> dispatches workers into isolated tmux worktrees, runs a zero-token supervisor, and
> gates delivery behind review + validation + your explicit approval.
>
> **New here? Start with [`docs/ONBOARDING.md`](docs/ONBOARDING.md).** Architecture and
> roadmap live in `ORCHA_PLAN.md`.

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

## Commands

| Command | Description |
| --- | --- |
| `orcha` | Launch the manager session and attach |
| `orcha cc [--isolated]` | Open a Claude session attached to the team |
| `orcha spawn <id> <repo> [--scout]` | Start a worker on a task in an isolated worktree |
| `orcha status` | List active workers |
| `orcha peek <id> [lines]` | Read recent output from a worker |
| `orcha send <id> <text>` | Type a message into a worker |
| `orcha teardown <id> [--force]` | Finish a worker (returns its worktree to the pool) |
| `orcha supervise` / `orcha daemon …` | Run the background supervisor |
| `orcha wake drain` | Print and clear pending supervision events |
| `orcha install` / `update` / `uninstall` | Manage the installed content |
| `orcha doctor [--yes]` | Detect and install missing dependencies |
| `orcha skills [install]` | List/install recommended agent skills (e.g. axi) |
| `orcha init [--mode m]` | Set up a repo's AGENTS.md + CLAUDE.md + delivery mode |
| `orcha version` / `help` | Version / usage |

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

Contributions keep a professional, neutral tone — no themed personas or role-play vocabulary.

## License

MIT — see `LICENSE`.
