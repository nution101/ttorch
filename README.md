# ttorch

Run a team of Claude Code agents. You act as the **manager**: plan the work, delegate to
isolated **worker** sessions, review the results, and approve delivery — instead of
writing and reviewing every line by hand.

> **Status: M0–M5.** Installs/updates safely, ships the global Claude Code surface,
> dispatches workers into isolated tmux worktrees, runs a zero-token supervisor, and
> gates delivery behind review + validation + your explicit approval.
>
> **New here? Start with [`docs/ONBOARDING.md`](docs/ONBOARDING.md).** Architecture and
> roadmap live in `TTORCH_PLAN.md`.

## Install

```sh
# macOS / Linux / WSL2
curl -fsSL https://raw.githubusercontent.com/nution101/ttorch/main/docs/install.sh | sh
```

```powershell
# Windows (basic; use WSL2 for the full experience)
irm https://raw.githubusercontent.com/nution101/ttorch/main/docs/install.ps1 | iex
```

Or build from source (requires Go):

```sh
git clone https://github.com/nution101/ttorch && cd ttorch
make install      # builds into ~/.ttorch/bin, links into ~/.local/bin, lays content
ttorch doctor      # check/installs tmux, git, gh, claude
```

## Verifying a release

Each release's `checksums.txt` is signed with [cosign](https://github.com/sigstore/cosign)
(keyless / Sigstore — no shared keys):

```sh
cosign verify-blob \
  --bundle checksums.txt.cosign.bundle \
  --certificate-identity-regexp '^https://github.com/nution101/ttorch/\.github/workflows/.+' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
shasum -a 256 -c checksums.txt --ignore-missing   # then verify your tarball
```

The installer already checks the sha256 against `checksums.txt`; the signature lets you
additionally confirm those checksums came from this repo's release workflow.

## Commands

| Command | Description |
| --- | --- |
| `ttorch` | Launch the manager session and attach |
| `ttorch cc [--isolated]` | Open a Claude session attached to the team |
| `ttorch spawn <id> <repo> [--scout]` | Start a worker on a task in an isolated worktree |
| `ttorch status` | List active workers |
| `ttorch peek <id> [lines]` | Read recent output from a worker |
| `ttorch send <id> <text>` | Type a message into a worker |
| `ttorch teardown <id> [--force]` | Finish a worker (returns its worktree to the pool) |
| `ttorch supervise` / `ttorch daemon …` | Run the background supervisor |
| `ttorch wake drain` | Print and clear pending supervision events |
| `ttorch install` / `update` / `uninstall` | Manage the installed content |
| `ttorch doctor [--yes]` | Detect and install missing dependencies |
| `ttorch skills [install]` | List/install recommended agent skills (e.g. axi) |
| `ttorch init [--mode m]` | Set up a repo's AGENTS.md + CLAUDE.md + delivery mode |
| `ttorch profile [dir]` | Derive the repo's stack/commands/conventions into AGENTS.md |
| `ttorch version` / `help` | Version / usage |

## How updates stay safe

`ttorch update` adds newly shipped skills and upgrades files you have not touched, but it
**never overwrites a file you edited**. Your version is kept; the new one is parked beside
it as `<name>.ttorch-new` and reported. A per-file sha256 manifest (`~/.ttorch/manifest.json`)
distinguishes "ttorch wrote this and it's unchanged" from "the developer changed it". Your
task state under `~/.ttorch/state` and `~/.ttorch/data` is never touched by updates.

## What gets installed

```
~/.ttorch/bin/ttorch                 # the binary (user-owned, for safe self-update)
~/.ttorch/manifest.json             # ledger of managed files
~/.claude/skills/ttorch-manager/    # the manager role
~/.claude/agents/ttorch-worker.md   # the worker brief contract
~/.claude/commands/ttorch.md        # the /ttorch slash command
~/.claude/AGENTS.md                # managed guidance block; CLAUDE.md symlinks to it
~/.agents/skills/ttorch-manager/    # vendor-neutral mirror
```

## Development

```sh
make build    # build ./bin/ttorch
make test     # go test ./...
make lint     # go vet + gofmt check + vocabulary lint
make dist     # cross-compile + checksums into ./dist
```

Contributions keep a professional, neutral tone — no themed personas or role-play vocabulary.

## License

MIT — see `LICENSE`.
