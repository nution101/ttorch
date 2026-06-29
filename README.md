# ttorch

Run a team of Claude Code agents on your codebases, in parallel, with a trust gate before
anything merges. You act as the **lead**: you talk to one **manager**, in plain language,
and it plans the work, briefs each task, reviews the results, and approves delivery — while
a deterministic **scheduler** keeps the fleet moving and **workers** do the coding in
isolated git worktrees. Instead of writing and reviewing every line by hand, you direct a
team.

[![CI](https://github.com/nution101/ttorch/actions/workflows/ci.yml/badge.svg)](https://github.com/nution101/ttorch/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/nution101/ttorch?sort=semver)](https://github.com/nution101/ttorch/releases/latest)

ttorch is a single Go binary. It installs/updates safely, ships a global Claude Code
surface, dispatches workers into isolated tmux + git-worktree sessions over a SQLite store,
supervises them with a zero-token event-driven watcher, gates delivery behind review +
validation + your approval (or a trusted auto-merge gate), and learns each repo's
conventions over time.

> **New here?** Start with [`docs/ONBOARDING.md`](docs/ONBOARDING.md). For how the system
> works internally, see [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## How it works

Four roles, one source of truth (a SQLite store), and a clean split between deterministic
machinery and LLM judgment:

- **You (the lead).** A human. You talk *only* to the manager, in one tab, in plain
  language. You approve delivery (except in trusted mode).
- **The manager.** A Claude Code session running the `ttorch-manager` skill. It does the
  work that needs judgment — plan tasks, write each a precise brief, run the adversarial
  review gate, answer blocked workers, surface decisions to you. It delegates all coding and
  review to workers; it never writes code itself.
- **The scheduler daemon.** A plain Go loop with **no LLM in it**, auto-started with the
  manager by default. It deterministically **dispatches** ready disjoint backlog, **lands**
  work that already passed the gate, and **supervises** (recovers) crashed workers.
- **Workers.** Claude Code sessions, one per task, each in its own isolated git worktree.

The dividing line: the daemon only does work whose safety it can *prove* from the store (a
declared file footprint, a disjoint file set, a passing verdict, a confirmed-dead window).
Everything else — *what* the tasks are, *whether* a diff is good, *whether* to approve — is
the manager's and your judgment. That's why **gating** (recording a passing verdict) is
always separate from **landing** (merging gated work): the daemon never gates, and it never
lands anything ungated. Full detail in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

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

Prerequisites are `tmux`, `git`, `gh`, and `claude` (Claude Code); `ttorch doctor` installs
the missing ones (and offers iTerm2 on macOS). The binary lives in the user-owned
`~/.ttorch/bin/ttorch` with a PATH symlink at `~/.local/bin/ttorch` (make sure
`~/.local/bin` is on your `PATH`).

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

The `install.sh` installer runs this same `cosign verify-blob` automatically. When `cosign`
is installed it verifies **strictly**: a missing or invalid signature is fatal and the
install is refused — it never silently downgrades to a sha256-only check, which an attacker
could otherwise force by stripping the signature from a tampered download. Every release
from v0.1.0 on is signed, so this never blocks a real release. Set
`TTORCH_INSTALL_ALLOW_UNSIGNED=1` to opt out for the rare case that must proceed without a
verifiable signature (an air-gapped mirror, or a genuinely unsigned release): a *missing*
signature then degrades to a loud warning plus the sha256 check, while a signature that is
present but fails verification stays fatal. When `cosign` is **not** installed, provenance
cannot be verified at all, so the installer warns and verifies only the sha256 — which
confirms the download is internally consistent but does **not** prove it came from this
repo; install `cosign` for that provenance guarantee. The commands above let you verify a
release by hand.

## The daily loop

The default experience is **autonomous**. You plan and approve; the scheduler dispatches,
lands, and recovers; the manager bridges the two with judgment.

1. **Start.** `cd` into a repo and run `ttorch`. The manager opens in that folder and the
   scheduler auto-starts behind it. Talk to the manager in plain language.
2. **Plan.** Tell the manager a goal. It breaks the work into tasks, giving each a precise
   **file footprint** (`--touches`) and a stored **brief** so the scheduler can dispatch it
   hands-off (`ttorch task add … --touches … --brief-file …`).
3. **Dispatch (automatic).** The scheduler launches a worker for every backlog task whose
   files are disjoint from all in-flight work, within worktree capacity — no manual `spawn`
   needed. You can still dispatch by hand (`ttorch spawn`) when you want to.
4. **Supervise (automatic + you).** `ttorch tasks` shows the whole board; `ttorch status`
   shows live workers; `ttorch peek <id>` reads a worker's output. The manager wakes only on
   real worker events (a zero-token watcher), so an idle team costs nothing. Crashed workers
   are recovered automatically within a bounded retry budget.
5. **Gate (the manager, with you).** When a worker reports done, the manager runs the
   repo's checks (`ttorch validate`) and the adversarial-review gate (`ttorch trust …`),
   recording a durable, commit-pinned **verdict**. Gating produces a verdict; it never
   merges.
6. **Land.** In most modes you run `ttorch approve <id>` and the gated work lands. In
   **trusted** mode a passing verdict + a fresh green validate lands it with no separate
   approval (see below). The scheduler lands already-gated work for you.
7. **Finish.** Delivered tasks return their worktree to the pool for reuse.

Need an ad-hoc Claude session the manager can see? `ttorch cc` opens one inside the team
session (`cc --isolated` gives it its own worktree).

## Commands

Grouped the way `ttorch help` groups them. `ttorch help` is the authoritative, always-current
reference; this table covers the surface a lead and manager use day to day.

**Team**

| Command | Description |
| --- | --- |
| `ttorch` | Start/attach the manager (persistent). With a saved session, rebuilds the manager + every worker tab; otherwise starts fresh in the current folder |
| `ttorch resume` | Force a rebuild of the manager + all worker tabs from saved state, then attach |
| `ttorch reset [--yes]` | Discard the saved session for a clean start (worktrees/branches are kept) |
| `ttorch stop` | Stop the manager session (resumable: run `ttorch` to come back) |
| `ttorch cc [--isolated]` | Open a Claude session attached to the team |
| `ttorch spawn <id> <repo>` | Start a worker on a task in an isolated worktree. Flags: `--scout`, `--touches "a,b"`, `--brief`/`--brief-file`, `--effort <level>`, `--init`, `--force-overlap`, `--cmd` |
| `ttorch status` | List live workers (tmux state + each task's DB status/stage/owner + free dispatch capacity) |
| `ttorch check-overlap "<paths>"` | Show which live workers a proposed footprint conflicts with |
| `ttorch peek <id> [lines]` | Read recent output from a worker |
| `ttorch send <id> <text>` | Type a message into a worker (delivered verbatim; also `-` for stdin, `--message-file`) |
| `ttorch teardown <id> [--force]` | Finish a worker (returns its worktree to the pool; refuses to discard unlanded work without `--force`) |

**Backlog & planning** (read/write the DB; `tasks` includes pending backlog)

| Command | Description |
| --- | --- |
| `ttorch tasks` | List tasks. Flags: `--project`, `--epic`, `--status s[,s…]`, `--tree` (projects→epics→phases→tasks), `--timeline <id>` |
| `ttorch task add <id> --project <id>` | Create a pending backlog task (does not spawn). Flags: `--epic`, `--phase`, `--title`, `--touches`, `--brief`/`--brief-file` |
| `ttorch project add <repo>` / `project ls` | Register / list repos (caches delivery mode for display) |
| `ttorch epic add` / `epic ls` / `epic set-status` | Manage epics under a project |
| `ttorch phase add` / `phase ls` / `phase set-status` | Manage phases under an epic |

**Worker reporting** (run by a worker about its own task)

| Command | Description |
| --- | --- |
| `ttorch report <done\|blocked\|needs-input\|active> [-m]` | Set the task's status (`done`/`blocked`/`needs-input` wake the manager) |
| `ttorch stage "<text>"` | Set a free-text progress stage (does not wake the manager) |
| `ttorch note <text>` | Record freeform activity (does not wake the manager) |
| `ttorch follow-on <new-id> --title "…"` | File a child task into the backlog (does not spawn) |

**Supervision**

| Command | Description |
| --- | --- |
| `ttorch watch` | Block until an actionable DB event, print the batch + watermark, exit. Flags: `--since`, `--timeout`, `--coalesce`, `--reset`. The manager arms this each non-blocking turn |
| `ttorch await-lead [--clear]` | Mark the manager as awaiting the lead (the watcher stays silent until cleared) |
| `ttorch watchdog` | External manager-liveness net (run from launchd/cron). Flags: `--stall`, `--interval`, `--quiet` |
| `ttorch scheduler` | The deterministic dispatch+land+supervise daemon. Flags: `--dispatch` (on), `--land`, `--supervise`, `--interval`, `--once`, `--singleton` |

**Delivery**

| Command | Description |
| --- | --- |
| `ttorch validate <id>` | Run the repo's build/test/lint checks on a worker |
| `ttorch ci-parity [dir] [--list]` | Reproduce the repo's actual CI run-steps locally |
| `ttorch review-diff <id> [--stat]` | Show a worker's changes vs the default branch |
| `ttorch trust prep\|record\|show <id>` | Prepare / record / show the adversarial-review verdict (the trust gate) |
| `ttorch security-review prep\|record\|show <id>` | Standalone advisory security audit (every mode; never blocks) |
| `ttorch qa-review prep\|record\|show <id>` | Optional advisory test-adequacy audit (never blocks) |
| `ttorch approve <id> [--ttl 10m]` | Grant a time-boxed, single-use approval (the lead's action) |
| `ttorch merge-local <id> [--require-verdict]` | Fast-forward the local default branch (needs approval) |
| `ttorch land <id>… \| --all [--require-verdict]` | One safe atomic delivery: fetch, rebase, re-validate, integrate per delivery mode, fast-forward. `--all` lands the whole done set concurrently |
| `ttorch promote <id>` | Turn a scout task into a ship task |
| `ttorch pr-check <id> <url>` | Arm a PR-merge check (surfaced by `ttorch watch`) |
| `ttorch fleet-sync [dir]` / `recovery` | Refresh local default + prune gone branches / reconcile tracked tasks against live windows |
| `ttorch learn "<lesson>"` / `learnings` | Record / list durable repo lessons |

**Setup**

| Command | Description |
| --- | --- |
| `ttorch install` / `update [--content-only]` / `uninstall [--purge]` | Manage the installed binary + content |
| `ttorch doctor [--yes]` | Detect and install missing dependencies |
| `ttorch skills [install]` | List/install recommended agent skills (e.g. `axi`) |
| `ttorch init [--mode pr\|local\|validated\|trusted]` | Set up a repo's AGENTS.md + CLAUDE.md + delivery mode + profile |
| `ttorch profile [dir]` | Derive the repo's stack/commands/conventions into AGENTS.md |
| `ttorch version` / `help` | Version / full usage |

## The scheduler daemon

`ttorch scheduler` is a deterministic Go loop (no LLM) that drains the task board on a
ticker (default every 5s). Each tick runs up to three independent, error-isolated passes
in fixed order — **supervise → dispatch → land**:

- **Dispatch** — claim and launch every `pending` task it can *prove* safe to run in
  parallel: a **declared footprint**, disjoint from every live and just-claimed worker,
  within free worktree capacity. It claims atomically before spawning, so two ticks (or two
  daemons) can never double-dispatch. Overlapping, no-capacity, or footprint-less tasks are
  **skipped** (left for the manager), never failed.
- **Land** — land `done` tasks that **already carry a passing verdict**, through the same
  pipeline as `ttorch land`. It never lands ungated work.
- **Supervise** — reclaim only **verifiably-dead** workers (a confirmed-gone window or an
  expired lease, re-checked under a write lock — never pane-output inference) and re-dispatch
  them within a **bounded retry ceiling** (default 3), poison-pilling a task that exceeds it
  to terminal `failed` with an actionable event.

**It auto-starts with the manager by default**, running all three passes
(`scheduler --singleton --dispatch --land --supervise`), logging to `~/.ttorch/scheduler.log`
— never the manager pane. Turn it off with a falsey **`TTORCH_SCHEDULER_AUTOSTART`**
(`0`/`false`/`no`/`off`). A `--singleton` `flock` ensures at most one daemon runs per
`~/.ttorch`.

> A bare `ttorch scheduler` run by hand defaults to **dispatch-only**; the auto-started
> daemon runs all three passes. The scheduler **lands only already-gated work** — the
> manager and the lead still own gating and (in non-trusted modes) approval.

To feed the autonomy loop, give each backlog task a **file-granular `--touches` footprint**
and a **stored brief** (`--brief-file`). Without both, the scheduler leaves the task for the
manager to dispatch by hand.

## The trust gate & delivery modes

Before anything merges, a worker's diff passes a trust gate: **independent AI reviewers plus
the repo's own build/test/lint**, enforced in Go at the merge point.

Review has **three blocking dimensions — correctness, scope, security** — each run by an
independent reviewer subagent, scaled to the diff size (docs-only and trivial single-file
changes review fewer dimensions; anything substantial or uncertain gets all three). The flow
is `ttorch trust prep` → reviewer subagents → `ttorch trust record`, which writes a durable
**verdict** that is **commit-pinned and content-pinned** (it never expires by age; a clean
rebase that keeps the diff byte-identical carries it forward, any content change forces a
re-gate). Any high/critical finding blocks, and a missing/malformed report fails closed. A
separate, advisory **security audit** (`ttorch security-review`) runs in *every* mode but
never blocks.

The **delivery mode** lives in the repo's `AGENTS.md`/`CLAUDE.md` ttorch-managed block (set
by `ttorch init --mode`) and defaults to `pr`:

| Mode | Integration | Authorization |
| --- | --- | --- |
| `pr` (default) | Open/merge a PR, then fast-forward the local default | GitHub review / branch protection |
| `local` | Approval-gated local fast-forward | The lead's `ttorch approve` |
| `validated` | Approval-gated local fast-forward (identical to `local` in v0.10.0) | The lead's `ttorch approve` |
| `trusted` | Approval-gated local fast-forward, full review + validate gate | A passing verdict + fresh green validate — **no separate human approval** |

`local` and `validated` behave the same today: an approval-gated local fast-forward. The
verdict + fresh-validate gate at the merge engages only in `trusted` mode or when you pass
`--require-verdict` to `ttorch merge-local`/`land` (which any of `local`/`validated`/
`trusted` accept). **Trusted mode is the only path that merges without a human reading the
diff.** A passing
commit-pinned verdict plus a fresh green validate auto-mints the approval. It is guard-railed:
it **requires a `.ttorch/validate.sh` on the default branch** (the gate validates the
committed sha with the default-branch script, so a worker can't weaken its own gate; "no
checks detected" is a hard block), and a trusted auto-merge **cannot change the gate itself**
(`.ttorch/validate.sh` or `AGENTS.md`) — that always requires a human. The delivery-mode
block is an explicit, repo-scoped decision; changing it requires a human.

## Footprints & the worktree pool

Each worker runs in a git worktree drawn from a per-repository **pool** under
`~/.ttorch/worktrees` (size **`TTORCH_MAX_WORKTREES`**, default 16). Teardown returns a slot
to the pool for reuse; the pool's free-slot count is the dispatch capacity the scheduler
respects.

A task's **footprint** (`--touches`) — a set of file paths/prefixes — is how parallel safety
is proven without reading code. `spawn` and the scheduler refuse to dispatch a task onto
files a live worker already holds (override with `--force-overlap`); `ttorch check-overlap`
previews the conflicts. Declare footprints at **file granularity** — a whole-package
footprint needlessly serializes disjoint tasks.

## Validation

`ttorch validate <id>` runs a repo's own checks against a worker's worktree:

- **Custom:** an executable-or-not `.ttorch/validate.sh` (one step) overrides detection.
- **Go:** `go build`, `go vet`, `gofmt`, `go test`.
- **Node:** the `build` / `lint` / `test` scripts present in `package.json`.

Each check runs under a timeout (`TTORCH_VALIDATE_TIMEOUT`, default 10m); a non-zero exit
fails that check, so the manager (and the gate) can gate on it. At a gated merge, the gate
re-validates the **immutable committed sha** against the **default-branch** definition, so a
worker cannot weaken its own gate, and a repo with no checks is a hard block.

> **Trust:** validation runs the repository's own commands on your machine with your
> credentials. Only run it against repositories and worker output you trust.

## Resuming after a reboot or upgrade

Your team survives a stop, a reboot, a crash, or a `ttorch update`. Three things persist on
disk independently of the running tmux session: ttorch's state (`~/.ttorch/state.db`), the
git worktrees, and Claude Code's conversation transcripts
(`~/.claude/projects/<dir>/<id>.jsonl`). At launch each session is given a stable session
id, so it can later be resumed to the exact conversation it had.

- **`ttorch`** — bare `ttorch` is all you normally need. If a saved session exists, it
  rebuilds the **manager window** and **every worker tab**, each resumed to its prior Claude
  conversation (`--resume`), then attaches you. If there's no saved session, it starts a
  fresh manager in the current folder.
- **`ttorch stop`** — a *resumable pause*. It ends the tmux session but keeps your saved
  session, so `ttorch` brings everything back.
- **`ttorch resume`** — force a rebuild of the manager + all worker tabs from saved state
  (useful if a window was closed), then attach.
- **`ttorch reset [--yes]`** — discard the saved session for a clean start. It kills the tmux
  session and removes the manager and task records. It **never** deletes worktrees or
  branches — your work is safe.

Restore is best-effort: if a worker's worktree is gone its tab is skipped (noted), and one
window that fails to rebuild never aborts the rest.

## Project setup (automatic)

On first use ttorch sets a repo up for you: both bare `ttorch` (starting the manager) and
`ttorch spawn` write the AGENTS.md managed block, the `CLAUDE.md` symlink, and the project
profile, so a worker always has project memory to read without you running `ttorch init`
first. The default delivery mode is `pr`. When it sets a repo up, it says so:

```
ttorch: set up /path/to/repo for ttorch (set TTORCH_NO_AUTOINIT=1 to skip)
```

Auto-init is **tracked-file-safe**: it writes only when doing so changes no git-tracked
file. The convention files it creates are untracked, which a clean local fast-forward
tolerates. If your repo already commits `AGENTS.md` or `CLAUDE.md`, ttorch declines to touch
it — injecting the block would dirty your checkout and block a local merge — and prints a
one-line nudge toward explicit setup instead:

```
ttorch: /path/to/repo not ttorch-init'd; using delivery-mode=pr (run "ttorch init" to persist).
```

The writes are clobber-safe (your own AGENTS.md content is preserved), idempotent, and a
no-op outside a git repo or on a repo that's already set up. Opt out entirely with
`TTORCH_NO_AUTOINIT=1`.

To choose a delivery mode other than `pr`, or to force setup on a repo that already tracks
`AGENTS.md` (where auto-init declines), set it up explicitly:

```sh
ttorch init [--mode pr|local|validated|trusted]   # set up the repo in your current dir
ttorch spawn <id> <repo> --init                   # force first-use setup, then dispatch a worker
```

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
internal sub-agents) instead of delegating. Per-task effort (`ttorch spawn --effort <level>`)
resolves as explicit `--effort` > `TTORCH_EFFORT` > the kind default (ship `ultracode`,
scout `high`), is persisted on the task, and is restored verbatim on resume.

```sh
TTORCH_EFFORT=max ttorch              # workers at highest raw reasoning, no nested orchestration
TTORCH_MANAGER_EFFORT=ultracode ttorch # opt the manager back into ultracode
```

## Worker visibility

Every worker runs as a window in a shared tmux session (default name `ttorch`).
`ttorch status`, `ttorch peek`, `ttorch send`, and `ttorch teardown` all drive those
windows, and you can navigate between them inside tmux with `Ctrl-b w`.

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

This is a macOS-only convenience and best-effort: on other platforms, or if it can't open a
tab, workers run in tmux exactly as before.

## Updating

```sh
ttorch update                 # self-update the binary, then re-apply managed content
ttorch update --content-only  # re-apply content only (no binary change)
```

Updates add newly shipped skills and upgrade files you have not touched, but **never
overwrite a file you edited** — your version is kept and the new one is parked beside it as
`<name>.ttorch-new` and reported. A per-file sha256 manifest (`~/.ttorch/manifest.json`)
distinguishes "ttorch wrote this and it's unchanged" from "you changed it". Your task state
under `~/.ttorch/state.db` and `~/.ttorch/data` is never touched.

## What gets installed

```
~/.ttorch/bin/ttorch                 # the binary (user-owned, for safe self-update)
~/.ttorch/manifest.json             # ledger of managed files
~/.ttorch/state.db                  # the SQLite store (tasks, events, verdicts, leases)
~/.ttorch/data/                     # per-task briefs and review inputs
~/.ttorch/worktrees/                # the per-repository worktree pool
~/.claude/skills/ttorch-manager/    # the manager role (also ttorch-validate, ttorch-review)
~/.claude/agents/ttorch-worker.md   # the worker brief contract (+ ttorch-reviewer-* agents)
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

Contributions keep a professional, neutral tone — no themed personas or role-play
vocabulary. See [`AGENTS.md`](AGENTS.md) for the full contributor conventions, and
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for how the system fits together.

## Releases & CI

- **CI** (`.github/workflows/ci.yml`) runs `go vet`, the `gofmt` check, and `go test` on
  macOS and Linux for every push and pull request.
- **Releases are automated** by [release-please](https://github.com/googleapis/release-please):
  as `feat:` / `fix:` commits land on `main` it maintains a release pull request; merging
  that PR tags the version, then the workflow cross-compiles the binaries, generates
  `checksums.txt`, **signs it with cosign (keyless)**, and attaches everything to the GitHub
  release.
- **Manual release** (fallback): `git tag vX.Y.Z && git push origin vX.Y.Z` runs the same
  build → sign → publish via `.github/workflows/release.yml`.
- Artifacts are named `ttorch-<version>-<os>-<arch>.tar.gz`; `install.sh`/`install.ps1` and
  `ttorch update` resolve the latest release automatically.

## License

MIT — see `LICENSE`.
