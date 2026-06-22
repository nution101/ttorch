# Onboarding

ttorch lets you run a team of Claude Code agents: you act as the **manager** — plan the
work, delegate to isolated **worker** sessions, review, and approve delivery — instead
of writing and reviewing every line yourself.

## 1. Requirements

- **macOS, Linux, or Windows with WSL2.** (ttorch is Linux/macOS-native; on Windows it
  runs inside WSL2 — see §10.)
- `tmux`, `git`, `gh`, and `claude` (Claude Code). `ttorch doctor` installs the missing
  ones for you.

## 2. Install

**macOS / Linux / WSL2:**
```sh
curl -fsSL https://raw.githubusercontent.com/nution101/ttorch/main/docs/install.sh | sh
```

**Windows (bootstraps into WSL2):**
```powershell
irm https://raw.githubusercontent.com/nution101/ttorch/main/docs/install.ps1 | iex
```

**From source (needs Go):**
```sh
git clone https://github.com/nution101/ttorch && cd ttorch
make install          # builds into ~/.ttorch/bin, links into ~/.local/bin, lays content
```

The binary lives in the user-owned `~/.ttorch/bin/ttorch` with a PATH symlink at
`~/.local/bin/ttorch`. Make sure `~/.local/bin` is on your `PATH`.

Release downloads are sha256-checked against the release's `checksums.txt` during
install; the checksums are also cosign-signed — see **Verifying a release** in the
README to confirm a download independently.

## 3. First run

```sh
ttorch doctor          # check (and offer to install) tmux/git/gh/claude; reports WSL status
ttorch                 # launch the manager session and attach
```

`ttorch` with no arguments ensures your dependencies, the tmux session, and the
background supervisor, then opens the manager and attaches you to it. Talk to the
manager in plain language; it runs the team.

## 4. The daily loop

You direct; the manager dispatches and supervises:

1. **Plan** — tell the manager what you want; it breaks the work into tasks.
2. **Dispatch** — `ttorch spawn <id> <repo>` starts a worker in its own isolated
   worktree (add `--scout` for investigation-only tasks that produce a report, not code).
3. **Supervise** — `ttorch status` lists workers; `ttorch peek <id>` reads a worker's
   output; `ttorch send <id> "<message>"` steers one. The background supervisor wakes the
   manager on real events, so an idle team costs nothing.
4. **Review & validate** — `ttorch review-diff <id>` shows the changes;
   `ttorch validate <id>` runs the repo's build/test/lint checks (§6).
5. **Approve & integrate** — you run `ttorch approve <id>`, then the manager runs
   `ttorch merge-local <id>` (or opens a PR). **Nothing merges without your approval.**
6. **Finish** — `ttorch teardown <id>` returns the worktree to the pool for reuse.

Need an ad-hoc Claude session the manager can see? `cc` (or `ttorch cc`) opens one inside
the team session; `cc --isolated` gives it its own worktree.

## 5. Delivery modes

Set a repo's delivery mode once:
```sh
ttorch init --mode pr        # propose work as a pull request (default)
ttorch init --mode local     # fast-forward the local default branch after approval
ttorch init --mode validated # run the validation gate before opening a PR
```
This writes an `AGENTS.md` block and symlinks `CLAUDE.md → AGENTS.md` in the repo.
`ttorch init` also derives a **project profile** (stack, exact build/test/lint commands,
layout, and a few exemplar files) into `AGENTS.md` so workers match the repo's style;
refresh it anytime with `ttorch profile`. Commit `AGENTS.md` so workers pick it up.

## 6. Validation gate

`ttorch validate <id>` runs a repo's own checks against a worker's worktree:

- **Go:** `go build`, `go vet`, `gofmt`, `go test`.
- **Node:** the `build` / `lint` / `test` scripts present in `package.json`.
- **Override:** add an executable-or-not `.ttorch/validate.sh` to define checks yourself.

Each check runs under a timeout (`TTORCH_VALIDATE_TIMEOUT`, default 10m). A non-zero exit
means at least one check failed, so the manager can gate on it.

> **Trust:** validation runs the repository's own commands on your machine with your
> credentials. Only run it against repositories and worker output you trust.

## 7. Skills & memory (ramp up the crew)

Workers get more effective when they start informed:

- **Skills.** A worker is a normal Claude Code session, so it inherits every Agent Skill
  in `~/.claude/skills` — ttorch adds its own on top. `ttorch skills` lists recommended
  skills and `ttorch skills install` adds them (e.g. the `axi` guidelines for
  agent-ergonomic CLIs; needs `npx`/Node). A team can also ship its own skills through
  ttorch's managed content so `ttorch update` distributes them to everyone.
- **Memory.** A repo's committed `AGENTS.md` (with `CLAUDE.md` symlinked to it by
  `ttorch init`) is durable project memory — conventions, gotchas, where things live.
  Workers read it automatically. At delivery the manager records lessons with
  `ttorch learn` into `.ttorch/learnings.jsonl`; **recurring** lessons auto-promote into a
  capped `AGENTS.md` "Learnings" block, so the repo gets smarter over time without that
  block ever bloating. Commit `AGENTS.md` and `.ttorch/learnings.jsonl` so the whole team
  benefits. (`ttorch learnings` lists them.)

## 8. Approvals & safety

- The manager is **read-only** over your real checkouts; all changes happen in
  disposable worktrees.
- `merge-local` requires an approval **bound to the reviewed commit**: if the worker's
  HEAD changed after you approved, the merge is refused and you must re-review.
- Approvals are time-boxed (`--ttl`, default 10m), single-use, and recorded in
  `~/.ttorch/audit.log`.

## 9. Updating

```sh
ttorch update                 # self-update the binary, then re-apply managed content
ttorch update --content-only  # just re-apply content (e.g. from a source checkout)
```
Updates **add** new capabilities and upgrade files you haven't touched, but **never
overwrite a file you edited** — your version is kept and the new one is parked beside it
as `<file>.ttorch-new` and reported. Your task state under `~/.ttorch` is never touched.

## 10. Windows / WSL2

Run ttorch **inside WSL2**, not on the Windows host. The PowerShell installer bootstraps
into your default WSL distribution. WSL1 is not supported (its process/cwd semantics are
unreliable); `ttorch doctor` warns if it detects WSL1. Install your dependencies inside
the WSL distribution (`ttorch doctor` will).

## 11. Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `TTORCH_HOME` | `~/.ttorch` | install home, state, worktrees, audit log |
| `TTORCH_CLAUDE_DIR` | `~/.claude` | where skills/agents/commands install |
| `TTORCH_AGENTS_DIR` | `~/.agents` | vendor-neutral skill mirror |
| `TTORCH_BIN_DIR` | `~/.local/bin` | directory for the PATH symlink |
| `TTORCH_TMUX_SESSION` | `ttorch` | tmux session name |
| `TTORCH_MAX_WORKTREES` | `16` | worktree pool size per repository |
| `TTORCH_VALIDATE_TIMEOUT` | `10m` | per-check timeout for `ttorch validate` |
| `TTORCH_REPO` | `nution101/ttorch` | release source for install/update |

## 12. Troubleshooting

- `ttorch doctor` — missing dependencies, package manager, WSL status.
- `ttorch daemon status` — is the supervisor running? `ttorch supervise` (re)starts it;
  `ttorch daemon stop` stops it.
- `ttorch status` — active workers and their state. `ttorch recovery` reconciles tracked
  tasks against live tmux windows after a crash or restart.
- `ttorch wake drain` — pending supervision events (the manager drains these each turn).

## 13. Uninstall

```sh
ttorch uninstall            # remove managed files (keeps anything you edited)
ttorch uninstall --purge    # also remove ~/.ttorch state and data
```
