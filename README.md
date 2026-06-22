# ttorch

Run a team of Claude Code agents. You act as the **manager**: plan the work, delegate to
isolated **worker** sessions, review the results, and approve delivery — instead of
writing and reviewing every line by hand.

[![CI](https://github.com/nution101/ttorch/actions/workflows/ci.yml/badge.svg)](https://github.com/nution101/ttorch/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/nution101/ttorch?sort=semver)](https://github.com/nution101/ttorch/releases/latest)

> ttorch installs/updates safely, ships a global Claude Code surface, dispatches workers
> into isolated tmux worktrees, runs a zero-token supervisor, gates delivery behind
> review + validation + your explicit approval, and learns each repo's conventions over
> time.
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
| `ttorch` | Start/attach the manager (persistent; a new one starts in the current folder) |
| `ttorch stop` | Stop the manager session and the supervisor |
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

## Session effort

The **manager** is a lean orchestrator: it launches at `--effort high` and carries a charter
that makes it *plan and delegate* (via `ttorch spawn`) rather than write code itself. The
deep work happens in **workers**, which run in **ultracode** by default (`xhigh` reasoning +
dynamic workflow orchestration). `ttorch cc` also defaults to ultracode.

| Env | Applies to | Default | Effect |
| --- | --- | --- | --- |
| `TTORCH_MANAGER_EFFORT` | the manager | `high` | `low`…`max`, or `ultracode`/`off` |
| `TTORCH_EFFORT` | workers + `ttorch cc` | `ultracode` | `ultracode`, a fixed `--effort` level (`max`…`low`), or `off` |

`ultracode` is not an `--effort` level — it is `xhigh` plus workflow orchestration (set via
`--settings`); the discrete levels go through `--effort`. The manager is deliberately *not*
ultracode by default, because that pushes a session to do deep work (and spawn its own
internal sub-agents) instead of delegating.

```sh
TTORCH_EFFORT=max ttorch              # workers at highest raw reasoning, no nested orchestration
TTORCH_MANAGER_EFFORT=ultracode ttorch # opt the manager back into ultracode
```

## Worker visibility

Every worker runs as a window in a shared tmux session (default name `ttorch`). The
zero-token supervisor, `ttorch status`, `ttorch peek`, `ttorch send`, and `ttorch teardown`
all drive those windows, and you can navigate between them inside tmux with `Ctrl-b w`.

On macOS, ttorch additionally opens a **native terminal tab or window** that *attaches a
view* onto each new worker's tmux window, so you can watch a worker without leaving tmux.
The native tab only views the worker — the worker process keeps running inside its tmux
window, and closing the tab tears down only that view (the worker and its window stay
alive). iTerm gets a new tab; Terminal.app gets a new window.

**iTerm2 is recommended** for the cleanest experience: it gives one window with a tab per
worker. When iTerm2 is installed, running bare `ttorch` opens the **manager itself in a new
iTerm2 window**, so the manager tab and the per-worker view tabs all live together in one
window. ttorch brings that iTerm2 window to the front, so your invoking terminal returns to
a prompt and you drive the team from the new window. With Terminal.app (the always-present
fallback) each worker still opens its own separate window instead, and the manager attaches
in place. `ttorch doctor` can install iTerm2 for you on macOS (via Homebrew).

| Env | Effect |
| --- | --- |
| `TTORCH_WORKER_TABS` | Native-terminal behavior (worker views **and** the manager-in-iTerm2 launch) is on by default; set `0`/`off`/`false`/`no` to disable (workers still run as tmux windows). |
| `TTORCH_TERMINAL` | `auto` (default) detects iTerm then falls back to Terminal.app; force with `iterm` or `terminal`. |

This is a macOS-only convenience and best-effort: on other platforms, or if it can't open
a tab, workers run in tmux exactly as before.

## Updating

```sh
ttorch update                 # self-update the binary, then re-apply managed content
ttorch update --content-only  # re-apply content only (no binary change)
```

Updates add newly shipped skills and upgrade files you have not touched, but **never
overwrite a file you edited** — your version is kept and the new one is parked beside it
as `<name>.ttorch-new` and reported. A per-file sha256 manifest (`~/.ttorch/manifest.json`)
distinguishes "ttorch wrote this and it's unchanged" from "you changed it". Your task
state under `~/.ttorch/state` and `~/.ttorch/data` is never touched.

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
make lint     # go vet + gofmt check
make dist     # cross-compile all targets + checksums into ./dist
```

Contributions keep a professional, neutral tone — no themed personas or role-play vocabulary.

## Releases & CI

- **CI** (`.github/workflows/ci.yml`) runs `go vet`, the `gofmt` check, and `go test` on
  macOS and Linux for every push and pull request.
- **Releases are automated** by [release-please](https://github.com/googleapis/release-please):
  as `feat:` / `fix:` commits land on `main` it maintains a release pull request; merging
  that PR tags the version, then the workflow cross-compiles the binaries, generates
  `checksums.txt`, **signs it with cosign (keyless)**, and attaches everything to the
  GitHub release.
- **Manual release** (fallback): `git tag vX.Y.Z && git push origin vX.Y.Z` runs the same
  build → sign → publish via `.github/workflows/release.yml`.
- Artifacts are named `ttorch-<version>-<os>-<arch>.tar.gz`; `install.sh`/`install.ps1`
  and `ttorch update` resolve the latest release automatically.

## License

MIT — see `LICENSE`.
