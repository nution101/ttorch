# Onboarding

ttorch lets you run a team of Claude Code agents. You act as the **lead** — you talk to one
**manager**, in plain language, and it plans the work, briefs each task, reviews the
results, and approves delivery. A deterministic **scheduler** keeps the fleet moving and
**workers** do the coding in isolated git worktrees. Instead of writing and reviewing every
line yourself, you direct a team.

This guide gets you from zero to a running, autonomous team. For how the system works
internally, see [`ARCHITECTURE.md`](ARCHITECTURE.md).

## 1. Requirements

- **macOS, Linux, or Windows with WSL2.** (ttorch is Linux/macOS-native; on Windows it runs
  inside WSL2 — see §11.)
- `tmux`, `git`, `gh`, and `claude` (Claude Code). `ttorch doctor` installs the missing ones
  for you.

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

Release downloads are sha256-checked against the release's `checksums.txt` during install,
and the checksums are cosign-signed. When `cosign` is installed the installer verifies that
signature **strictly** — a missing or invalid signature refuses the install rather than
downgrading to sha256 only (set `TTORCH_INSTALL_ALLOW_UNSIGNED=1` to opt out). See
**Verifying a release** in the README to confirm a download independently.

## 3. The four roles

ttorch coordinates four roles through a single SQLite store (the source of truth):

- **You (the lead).** A human. You talk *only* to the manager, in one tab. You approve
  delivery (except in trusted mode, §7).
- **The manager.** A Claude Code session running the `ttorch-manager` skill. It plans tasks,
  briefs them, gates finished work, answers blocked workers, and surfaces decisions to you.
  It delegates all coding and review — it never writes code itself.
- **The scheduler daemon.** A plain Go loop (no LLM), auto-started with the manager. It
  dispatches ready disjoint backlog, lands already-gated work, and recovers crashed workers.
- **Workers.** Claude Code sessions, one per task, each in its own isolated git worktree.

The key idea: the daemon does only what it can *prove* safe (a declared file footprint, a
disjoint file set, a passing verdict); the manager and you do everything that needs
judgment. So **gating** (recording a passing verdict) is always separate from **landing**
(merging gated work).

## 4. First run

```sh
ttorch doctor          # check (and offer to install) tmux/git/gh/claude; reports WSL status
cd ~/code/my-project   # start in the project you want to work on
ttorch                 # open the manager here and attach
```

`ttorch` with no arguments opens the manager in your **current directory** (its default
project) and attaches you; the scheduler auto-starts behind it. Talk to the manager in plain
language; it runs the team. The manager session is **persistent** — closing the tab only
detaches it, and running `ttorch` again re-attaches to the same one. To work on other repos,
just tell the manager their path; one manager tracks them all. When you're done (or want to
move it to a different folder):

```sh
ttorch stop            # stop the manager session (resumable)
```

`ttorch stop` is a *resumable pause*, not a teardown: your team state, worktrees, and Claude
conversations stay on disk. After a stop, a reboot, a crash, or a `ttorch update`, running
`ttorch` again rebuilds the manager **and every worker tab**, each resumed to the exact
conversation it had. Use `ttorch resume` to force that rebuild, or `ttorch reset` to discard
the saved session for a clean start (worktrees and branches are always kept).

## 5. The daily loop

The default experience is **autonomous**: you plan and approve, the scheduler dispatches,
lands, and recovers, and the manager bridges the two with judgment.

1. **Plan.** Tell the manager what you want; it breaks the work into tasks, giving each a
   precise **file footprint** and a stored **brief** (§6).
2. **Dispatch (automatic).** The scheduler launches a worker for every backlog task whose
   files are disjoint from all in-flight work, within worktree capacity — hands-off. You can
   still dispatch by hand with `ttorch spawn <id> <repo>` (add `--scout` for
   investigation-only tasks that produce a report, not code).
3. **Supervise.** `ttorch tasks` shows the whole board (including pending backlog);
   `ttorch status` lists live workers; `ttorch peek <id>` reads a worker's output;
   `ttorch send <id> "<message>"` steers one. The manager arms an event-driven watcher that
   wakes it only on real worker events, so an idle team costs nothing; while it is waiting on
   a decision from you it stays silent until you reply. Crashed workers are recovered
   automatically within a bounded retry budget. On macOS, each spawned worker also opens a
   native terminal tab/window viewing its tmux window (§12); inside tmux, `Ctrl-b w`
   navigates between worker windows. With iTerm2 installed (recommended; `ttorch doctor` can
   install it), bare `ttorch` opens the manager in a new iTerm2 window so the manager and all
   worker tabs share one window.
4. **Gate.** When a worker reports done, the manager runs the repo's checks
   (`ttorch validate <id>`, §8) and the adversarial-review gate (§7), recording a durable
   **verdict**. `ttorch review-diff <id>` shows the changes. Gating produces a verdict; it
   never merges.
5. **Approve & land.** In most modes you run `ttorch approve <id>` and the gated work lands
   (the scheduler lands already-gated work for you, or the manager runs `ttorch land`). In
   **trusted** mode a passing verdict + a fresh green validate lands it with no separate
   approval (§7). **Outside trusted mode, nothing merges without your approval.**
6. **Finish.** Delivered tasks return their worktree to the pool for reuse
   (`ttorch teardown <id>` if you finish one by hand).

Need an ad-hoc Claude session the manager can see? `cc` (or `ttorch cc`) opens one inside the
team session; `cc --isolated` gives it its own worktree.

## 6. Planning work: backlog, footprints & briefs

ttorch organizes work as a hierarchy — **projects → epics → phases → tasks** — that you can
inspect with `ttorch tasks --tree`. The task is the unit of work. Most planning is the
manager's job, but two things make a task **dispatchable hands-off** by the scheduler:

- A **file footprint** (`--touches "path/a.go,path/b/"`) — the files (or prefixes) the task
  will change. The scheduler dispatches a task only when its footprint is disjoint from every
  live worker, so footprints are how parallel safety is proven without reading code. Declare
  them at **file granularity** — a whole-package footprint needlessly serializes disjoint
  tasks. Preview conflicts with `ttorch check-overlap "<paths>"`.
- A **stored brief** (`--brief-file <path>` or `--brief "…"`) — the full instructions the
  worker launches on. Without a brief, a dispatched worker only gets a generic stub.

```sh
ttorch task add my-task --project <id> \
  --title "Add retry to the uploader" \
  --touches "internal/upload/retry.go,internal/upload/retry_test.go" \
  --brief-file ./briefs/uploader-retry.md
```

A task created this way sits in the backlog; the scheduler picks it up automatically once
its files are free. A task **without** both a footprint and a brief is left for the manager
to dispatch by hand.

## 7. Delivery modes & the trust gate

Set a repo's delivery mode with `ttorch init` (default `pr`). The mode lives in the repo's
`AGENTS.md` ttorch-managed block (with `CLAUDE.md` symlinked to it):

```sh
ttorch init --mode pr        # propose work as a pull request (default)
ttorch init --mode local     # fast-forward the local default branch after approval
ttorch init --mode validated # approval-gated local fast-forward (same as local in v0.10.0)
ttorch init --mode trusted   # auto-merge through the review gate (see below)
```

Before anything merges, a worker's diff passes a **trust gate**: independent AI reviewers
plus the repo's own build/test/lint, enforced in Go at the merge point. Review has three
blocking dimensions — **correctness, scope, security** — each run by an independent reviewer
subagent and scaled to the diff size. The manager runs `ttorch trust prep` → reviewer
subagents → `ttorch trust record`, which writes a durable, **commit-pinned** verdict. (A
separate, advisory `ttorch security-review` runs in *every* mode and never blocks.)

What each mode does at the merge:

| Mode | Integration | Authorization |
| --- | --- | --- |
| `pr` | Open/merge a PR, then fast-forward the local default | GitHub review / branch protection |
| `local` | Approval-gated local fast-forward | Your `ttorch approve` |
| `validated` | Approval-gated local fast-forward (identical to `local` in v0.10.0) | Your `ttorch approve` |
| `trusted` | Approval-gated local fast-forward, full review + validate gate | A passing verdict + fresh green validate — **no separate approval** |

(`local` and `validated` behave the same today; the verdict + validate gate at the merge
engages only in `trusted` mode or when you pass `--require-verdict`.)

**Trusted mode is the only path that merges without a human reading the diff.** It is an
explicit, repo-scoped decision and is guard-railed: it requires a `.ttorch/validate.sh` on
the default branch (the gate validates the committed sha with the *default-branch* script, so
a worker can't weaken its own gate), and a trusted auto-merge can never change the gate
itself (`.ttorch/validate.sh` or the delivery-mode block) — that always needs a human.

`ttorch init` also derives a **project profile** (stack, exact build/test/lint commands,
layout, and a few exemplar files) into `AGENTS.md` so workers match the repo's style; refresh
it anytime with `ttorch profile`. Commit `AGENTS.md` so workers pick it up.

> On first use ttorch also sets a repo up automatically (the `AGENTS.md` block, the
> `CLAUDE.md` symlink, and the profile), using the default `pr` mode. This is
> **tracked-file-safe** — it writes only untracked files, so if your repo already commits
> `AGENTS.md`/`CLAUDE.md` it declines and nudges you to run `ttorch init`. Opt out with
> `TTORCH_NO_AUTOINIT=1`.

## 8. Validation gate

`ttorch validate <id>` runs a repo's own checks against a worker's worktree:

- **Custom:** an executable-or-not `.ttorch/validate.sh` (one step) overrides detection.
- **Go:** `go build`, `go vet`, `gofmt`, `go test`.
- **Node:** the `build` / `lint` / `test` scripts present in `package.json`.

Each check runs under a timeout (`TTORCH_VALIDATE_TIMEOUT`, default 10m). A non-zero exit
means at least one check failed, so the manager (and the trust gate) can gate on it. At a
gated merge the gate re-validates the **immutable committed sha** against the
**default-branch** definition, so a worker cannot weaken its own gate, and "no checks
detected" is a hard block.

> **Trust:** validation runs the repository's own commands on your machine with your
> credentials. Only run it against repositories and worker output you trust.

## 9. Skills & memory (ramp up the crew)

Workers get more effective when they start informed:

- **Skills.** A worker is a normal Claude Code session, so it inherits every Agent Skill in
  `~/.claude/skills` — ttorch adds its own on top. `ttorch skills` lists recommended skills
  and `ttorch skills install` adds them (e.g. the `axi` guidelines for agent-ergonomic CLIs;
  needs `npx`/Node). A team can also ship its own skills through ttorch's managed content so
  `ttorch update` distributes them to everyone.
- **Memory.** A repo's committed `AGENTS.md` (with `CLAUDE.md` symlinked to it by
  `ttorch init`) is durable project memory — conventions, gotchas, where things live. Workers
  read it automatically. At delivery the manager records lessons with `ttorch learn` into
  `.ttorch/learnings.jsonl`; **recurring** lessons auto-promote into a capped `AGENTS.md`
  "Learnings" block, so the repo gets smarter over time without that block ever bloating.
  Commit `AGENTS.md` and `.ttorch/learnings.jsonl` so the whole team benefits.
  (`ttorch learnings` lists them.)

## 10. Approvals & safety

- The manager never touches **tracked** files in your real checkouts; a worker's code
  changes all happen in disposable worktrees. The only write to a real checkout is
  zero-config auto-init on first use, which creates just **untracked** convention files
  (`AGENTS.md`, the `CLAUDE.md` symlink, the project profile) and declines if those are
  already tracked (see §7; `TTORCH_NO_AUTOINIT=1` to skip).
- `merge-local` requires an approval **bound to the reviewed commit**: if the worker's HEAD
  changed after you approved, the merge is refused and you must re-review.
- Approvals are time-boxed (`--ttl`, default 10m), single-use, and recorded in
  `~/.ttorch/audit.log`. The trust verdict (from review) and the approval token (your sign-off)
  are kept as separate authorizations, so an audit can always tell them apart.

## 11. Updating

```sh
ttorch update                 # self-update the binary, then re-apply managed content
ttorch update --content-only  # just re-apply content (e.g. from a source checkout)
```
Updates **add** new capabilities and upgrade files you haven't touched, but **never overwrite
a file you edited** — your version is kept and the new one is parked beside it as
`<file>.ttorch-new` and reported. Your task state under `~/.ttorch` is never touched.

## 12. Windows / WSL2

Run ttorch **inside WSL2**, not on the Windows host. The PowerShell installer bootstraps into
your default WSL distribution. WSL1 is not supported (its process/cwd semantics are
unreliable); `ttorch doctor` warns if it detects WSL1. Install your dependencies inside the
WSL distribution (`ttorch doctor` will).

## 13. Worker visibility (macOS)

On macOS, ttorch opens a native terminal tab/window that *views* each worker's tmux window,
so you can watch a worker without leaving tmux — the worker keeps running in tmux, and
closing the view tears down only the view. iTerm2 (recommended) gives one window with a tab
per worker and launches the manager itself in a new iTerm2 window; Terminal.app (the
fallback) opens a separate window per worker. Toggle with `TTORCH_WORKER_TABS` and
`TTORCH_TERMINAL` (§14). On other platforms, workers run in tmux exactly as before
(`Ctrl-b w` to navigate). See the README's "Worker visibility" for full detail.

## 14. Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `TTORCH_HOME` | `~/.ttorch` | install home, state, worktrees, audit/scheduler logs |
| `TTORCH_CLAUDE_DIR` | `~/.claude` | where skills/agents/commands install |
| `TTORCH_AGENTS_DIR` | `~/.agents` | vendor-neutral skill mirror |
| `TTORCH_BIN_DIR` | `~/.local/bin` | directory for the PATH symlink |
| `TTORCH_TMUX_SESSION` | `ttorch` | tmux session name |
| `TTORCH_SCHEDULER_AUTOSTART` | enabled | the scheduler auto-starts with the manager (dispatch + land + supervise); set `0`/`off`/`false`/`no` to disable and drive dispatch/land/recovery by hand |
| `TTORCH_MAX_WORKTREES` | `16` | worktree pool size per repository (the dispatch capacity) |
| `TTORCH_EFFORT` | `ultracode` | worker + `ttorch cc` reasoning effort (`ultracode` = xhigh + workflow orchestration; or a fixed `--effort` level; or `off`) |
| `TTORCH_MANAGER_EFFORT` | `high` | manager reasoning effort (deliberately not ultracode, so it delegates) |
| `TTORCH_VALIDATE_TIMEOUT` | `10m` | per-check timeout for `ttorch validate` |
| `TTORCH_NO_AUTOINIT` | unset | set to any value to disable zero-config auto-init on first use (§7) |
| `TTORCH_WORKER_TABS` | enabled | macOS-only: native-terminal worker views + the manager-in-iTerm2 launch; set `0`/`off`/`false`/`no` to disable (workers still run as tmux windows) |
| `TTORCH_TERMINAL` | `auto` | which terminal to use for worker views: `auto` (iTerm then Terminal.app), `iterm`, or `terminal` |
| `TTORCH_REPO` | `nution101/ttorch` | release source for install/update |

## 15. Troubleshooting

- `ttorch doctor` — missing dependencies, package manager, WSL status.
- `ttorch status` — active workers and their state. `ttorch recovery` reconciles tracked
  tasks against live tmux windows after a crash or restart.
- `ttorch tasks` — the full board, including pending backlog and any task the scheduler
  poison-pilled to `failed` (with an actionable event explaining why).
- The auto-started scheduler logs to `~/.ttorch/scheduler.log` (never the manager tab).

> **Note on the watcher and pickers.** The manager is woken by the harness's own
> background-task-completion channel, not by a keystroke into its session, so a watcher
> firing can never disturb an open `AskUserQuestion` picker or a mid-generation turn. The
> rationale and the one-time verification procedure are in
> [`ARCHITECTURE.md`](ARCHITECTURE.md) §6.

## 16. Uninstall

```sh
ttorch uninstall            # remove managed files (keeps anything you edited)
ttorch uninstall --purge    # also remove ~/.ttorch state and data
```
