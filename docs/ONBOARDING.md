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
install, and the checksums are cosign-signed. When `cosign` is installed the installer
verifies that signature **strictly** — a missing or invalid signature refuses the install
rather than downgrading to sha256 only (set `TTORCH_INSTALL_ALLOW_UNSIGNED=1` to opt out).
See **Verifying a release** in the README to confirm a download independently.

## 3. First run

```sh
ttorch doctor          # check (and offer to install) tmux/git/gh/claude; reports WSL status
cd ~/code/my-project   # start in the project you want to work on
ttorch                 # open the manager here and attach
```

`ttorch` with no arguments opens the manager in your **current directory** (its default
project) and attaches you. Talk to it in plain language; it runs the team. The manager
session is **persistent** — closing the tab only detaches it, and running `ttorch` again
re-attaches to the same one. To work on other repos, just tell the manager their path;
one manager tracks them all. When you're done (or want to move it to a different folder):

```sh
ttorch stop            # stop the manager session (resumable)
```

`ttorch stop` is a *resumable pause*, not a teardown: your team state, worktrees, and
Claude conversations stay on disk. After a stop, a reboot, a crash, or a `ttorch update`,
running `ttorch` again rebuilds the manager **and every worker tab**, each resumed to the
exact conversation it had. Use `ttorch resume` to force that rebuild, or `ttorch reset` to
discard the saved session for a clean start (worktrees and branches are always kept).

## 4. The daily loop

You direct; the manager dispatches and supervises:

1. **Plan** — tell the manager what you want; it breaks the work into tasks.
2. **Dispatch** — `ttorch spawn <id> <repo>` starts a worker in its own isolated
   worktree (add `--scout` for investigation-only tasks that produce a report, not code).
3. **Supervise** — `ttorch status` lists workers; `ttorch peek <id>` reads a worker's
   output; `ttorch send <id> "<message>"` steers one. The manager arms an event-driven
   watcher that wakes it on real worker events, so an idle team costs nothing; while it is
   waiting on a decision from you it stays silent until you reply. On macOS, each spawned worker
   also opens a native terminal tab/window viewing its tmux window (see §11); inside tmux,
   `Ctrl-b w` still navigates between worker windows. With iTerm2 installed (recommended;
   `ttorch doctor` can install it), bare `ttorch` opens the manager in a new iTerm2 window
   so the manager and all worker tabs share one window.
4. **Review & validate** — `ttorch review-diff <id>` shows the changes;
   `ttorch validate <id>` runs the repo's build/test/lint checks (§6).
5. **Approve & integrate** — you run `ttorch approve <id>`, then the manager runs
   `ttorch merge-local <id>` (or opens a PR). **Nothing merges without your approval.**
6. **Finish** — `ttorch teardown <id>` returns the worktree to the pool for reuse.

Need an ad-hoc Claude session the manager can see? `cc` (or `ttorch cc`) opens one inside
the team session; `cc --isolated` gives it its own worktree.

## 5. Project setup & delivery modes

On first use ttorch sets a repo up automatically: both bare `ttorch` and `ttorch spawn`
write an `AGENTS.md` managed block, symlink `CLAUDE.md → AGENTS.md`, and derive a project
profile, so workers have project memory without a manual step. It uses the default delivery
mode, `pr`. This is **tracked-file-safe** — ttorch writes only untracked files, so if your
repo already commits `AGENTS.md`/`CLAUDE.md` it declines and nudges you to run `ttorch init`
instead (committing the block yourself). Opt out with `TTORCH_NO_AUTOINIT=1`.

Run `ttorch init` explicitly to pick a different delivery mode, or to set up a repo that
already tracks `AGENTS.md`:
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

- The manager never touches **tracked** files in your real checkouts; a worker's code
  changes all happen in disposable worktrees. The only write to a real checkout is
  zero-config auto-init on first use, which creates just **untracked** convention files
  (`AGENTS.md`, the `CLAUDE.md` symlink, the project profile) and declines if those are
  already tracked (see §5; `TTORCH_NO_AUTOINIT=1` to skip).
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
| `TTORCH_NO_AUTOINIT` | unset | set to any value to disable zero-config auto-init on first use (see §5) |
| `TTORCH_WORKER_TABS` | enabled | macOS-only: native-terminal behavior — open a native terminal tab/window viewing each new worker, and (with iTerm2) open the manager in a new iTerm2 window; set `0`/`off`/`false`/`no` to disable all of it (workers still run as tmux windows) |
| `TTORCH_TERMINAL` | `auto` | which terminal to use for worker views: `auto` (iTerm then Terminal.app), `iterm`, or `terminal` |
| `TTORCH_REPO` | `nution101/ttorch` | release source for install/update |

## 12. Picker-safety validation (one-time)

The manager puts decisions to you through Claude Code's `AskUserQuestion` picker. ttorch
removed every path that types into the manager session — the watcher wakes the manager
through the harness's own background-task-completion channel, never a keystroke — so an
open picker cannot be disturbed by a watcher firing. Confirm that once, on a real install,
before relying on pickers:

1. In a live manager session, open an `AskUserQuestion` picker (ask the manager to put a
   decision to you).
2. From another shell, insert an actionable event — e.g.
   `ttorch report blocked --task <id>` — and, separately, arm `ttorch watch` and let it
   fire.
3. Confirm the watcher's background completion **does not auto-select or dismiss the
   picker** (it cannot: completion is the harness's own notification, not stdin or
   keystrokes).
4. Test the mid-generation case too: let `ttorch watch` exit while the manager is actively
   generating, and confirm the completion is queued and surfaced at the next turn boundary
   with no disruption.
5. Record the result where the team can see it (e.g. the PR that lands the watcher).

**Prose-only fallback.** If a watcher firing ever disturbs an open picker, the manager
switches to plain-text questions (no picker) while awaiting your decision. See
`docs/design/sqlite-event-architecture.md` §4.6 for the full rationale.

## 13. Troubleshooting

- `ttorch doctor` — missing dependencies, package manager, WSL status.
- `ttorch status` — active workers and their state. `ttorch recovery` reconciles tracked
  tasks against live tmux windows after a crash or restart.

## 14. Uninstall

```sh
ttorch uninstall            # remove managed files (keeps anything you edited)
ttorch uninstall --purge    # also remove ~/.ttorch state and data
```
