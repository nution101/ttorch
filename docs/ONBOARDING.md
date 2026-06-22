# Onboarding

orcha lets you run a team of Claude Code agents: you act as the **manager** — plan the
work, delegate to isolated **worker** sessions, review, and approve delivery — instead
of writing and reviewing every line yourself.

## 1. Requirements

- **macOS, Linux, or Windows with WSL2.** (orcha is Linux/macOS-native; on Windows it
  runs inside WSL2 — see §10.)
- `tmux`, `git`, `gh`, and `claude` (Claude Code). `orcha doctor` installs the missing
  ones for you.

## 2. Install

**macOS / Linux / WSL2:**
```sh
curl -fsSL https://nution101.github.io/orcha/install.sh | sh
```

**Windows (bootstraps into WSL2):**
```powershell
irm https://nution101.github.io/orcha/install.ps1 | iex
```

**From source (needs Go):**
```sh
git clone https://github.com/nution101/orcha && cd orcha
make install          # builds into ~/.orcha/bin, links into ~/.local/bin, lays content
```

The binary lives in the user-owned `~/.orcha/bin/orcha` with a PATH symlink at
`~/.local/bin/orcha`. Make sure `~/.local/bin` is on your `PATH`.

## 3. First run

```sh
orcha doctor          # check (and offer to install) tmux/git/gh/claude; reports WSL status
orcha                 # launch the manager session and attach
```

`orcha` with no arguments ensures your dependencies, the tmux session, and the
background supervisor, then opens the manager and attaches you to it. Talk to the
manager in plain language; it runs the team.

## 4. The daily loop

You direct; the manager dispatches and supervises:

1. **Plan** — tell the manager what you want; it breaks the work into tasks.
2. **Dispatch** — `orcha spawn <id> <repo>` starts a worker in its own isolated
   worktree (add `--scout` for investigation-only tasks that produce a report, not code).
3. **Supervise** — `orcha status` lists workers; `orcha peek <id>` reads a worker's
   output; `orcha send <id> "<message>"` steers one. The background supervisor wakes the
   manager on real events, so an idle team costs nothing.
4. **Review & validate** — `orcha review-diff <id>` shows the changes;
   `orcha validate <id>` runs the repo's build/test/lint checks (§6).
5. **Approve & integrate** — you run `orcha approve <id>`, then the manager runs
   `orcha merge-local <id>` (or opens a PR). **Nothing merges without your approval.**
6. **Finish** — `orcha teardown <id>` returns the worktree to the pool for reuse.

Need an ad-hoc Claude session the manager can see? `cc` (or `orcha cc`) opens one inside
the team session; `cc --isolated` gives it its own worktree.

## 5. Delivery modes

Set a repo's delivery mode once:
```sh
orcha init --mode pr        # propose work as a pull request (default)
orcha init --mode local     # fast-forward the local default branch after approval
orcha init --mode validated # run the validation gate before opening a PR
```
This writes an `AGENTS.md` block and symlinks `CLAUDE.md → AGENTS.md` in the repo.
`orcha init` also derives a **project profile** (stack, exact build/test/lint commands,
layout, and a few exemplar files) into `AGENTS.md` so workers match the repo's style;
refresh it anytime with `orcha profile`. Commit `AGENTS.md` so workers pick it up.

## 6. Validation gate

`orcha validate <id>` runs a repo's own checks against a worker's worktree:

- **Go:** `go build`, `go vet`, `gofmt`, `go test`.
- **Node:** the `build` / `lint` / `test` scripts present in `package.json`.
- **Override:** add an executable-or-not `.orcha/validate.sh` to define checks yourself.

Each check runs under a timeout (`ORCHA_VALIDATE_TIMEOUT`, default 10m). A non-zero exit
means at least one check failed, so the manager can gate on it.

> **Trust:** validation runs the repository's own commands on your machine with your
> credentials. Only run it against repositories and worker output you trust.

## 7. Skills & memory (ramp up the crew)

Workers get more effective when they start informed:

- **Skills.** A worker is a normal Claude Code session, so it inherits every Agent Skill
  in `~/.claude/skills` — orcha adds its own on top. `orcha skills` lists recommended
  skills and `orcha skills install` adds them (e.g. the `axi` guidelines for
  agent-ergonomic CLIs; needs `npx`/Node). A team can also ship its own skills through
  orcha's managed content so `orcha update` distributes them to everyone.
- **Memory.** A repo's committed `AGENTS.md` (with `CLAUDE.md` symlinked to it by
  `orcha init`) is durable project memory — conventions, gotchas, where things live.
  Workers read it automatically. At delivery the manager records lessons with
  `orcha learn` into `.orcha/learnings.jsonl`; **recurring** lessons auto-promote into a
  capped `AGENTS.md` "Learnings" block, so the repo gets smarter over time without that
  block ever bloating. Commit `AGENTS.md` and `.orcha/learnings.jsonl` so the whole team
  benefits. (`orcha learnings` lists them.)

## 8. Approvals & safety

- The manager is **read-only** over your real checkouts; all changes happen in
  disposable worktrees.
- `merge-local` requires an approval **bound to the reviewed commit**: if the worker's
  HEAD changed after you approved, the merge is refused and you must re-review.
- Approvals are time-boxed (`--ttl`, default 10m), single-use, and recorded in
  `~/.orcha/audit.log`.

## 9. Updating

```sh
orcha update                 # self-update the binary, then re-apply managed content
orcha update --content-only  # just re-apply content (e.g. from a source checkout)
```
Updates **add** new capabilities and upgrade files you haven't touched, but **never
overwrite a file you edited** — your version is kept and the new one is parked beside it
as `<file>.orcha-new` and reported. Your task state under `~/.orcha` is never touched.

## 10. Windows / WSL2

Run orcha **inside WSL2**, not on the Windows host. The PowerShell installer bootstraps
into your default WSL distribution. WSL1 is not supported (its process/cwd semantics are
unreliable); `orcha doctor` warns if it detects WSL1. Install your dependencies inside
the WSL distribution (`orcha doctor` will).

## 11. Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `ORCHA_HOME` | `~/.orcha` | install home, state, worktrees, audit log |
| `ORCHA_CLAUDE_DIR` | `~/.claude` | where skills/agents/commands install |
| `ORCHA_AGENTS_DIR` | `~/.agents` | vendor-neutral skill mirror |
| `ORCHA_BIN_DIR` | `~/.local/bin` | directory for the PATH symlink |
| `ORCHA_TMUX_SESSION` | `orcha` | tmux session name |
| `ORCHA_MAX_WORKTREES` | `16` | worktree pool size per repository |
| `ORCHA_VALIDATE_TIMEOUT` | `10m` | per-check timeout for `orcha validate` |
| `ORCHA_REPO` | `nution101/orcha` | release source for install/update |

## 12. Troubleshooting

- `orcha doctor` — missing dependencies, package manager, WSL status.
- `orcha daemon status` — is the supervisor running? `orcha supervise` (re)starts it;
  `orcha daemon stop` stops it.
- `orcha status` — active workers and their state. `orcha recovery` reconciles tracked
  tasks against live tmux windows after a crash or restart.
- `orcha wake drain` — pending supervision events (the manager drains these each turn).

## 13. Uninstall

```sh
orcha uninstall            # remove managed files (keeps anything you edited)
orcha uninstall --purge    # also remove ~/.orcha state and data
```
