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
  manager by default. It deterministically **dispatches** ready backlog **in parallel
  (overlapping work included)**, **lands** work that already passed the gate, and
  **supervises** (recovers) crashed workers.
- **Workers.** Claude Code sessions, one per task, each in its own isolated git worktree.

The dividing line: the daemon only does work whose safety it can *prove* from the store (a
declared file footprint, free worktree capacity, a passing verdict, a clean land-rebase, a
confirmed-dead window).
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
3. **Dispatch (automatic).** The scheduler launches a worker for every briefed, footprinted
   backlog task within worktree capacity — **in parallel, even when their files overlap** — no
   manual `spawn` needed. You can still dispatch by hand (`ttorch spawn`) when you want to.
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
| `ttorch task add <id> --project <id>` | Create a pending backlog task (does not spawn). A supplied brief is lint-checked first. Flags: `--epic`, `--phase`, `--title`, `--touches`, `--brief`/`--brief-file`, `--citations-ref`, `--brief-lint-offline`, `--no-brief-lint` |
| `ttorch brief-lint <file>` | Check a brief before it is stored on a task. Flags: `--repo`, `--remote`, `--ref`, `--citations-ref`, `--offline` |
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
| `ttorch scheduler` | The deterministic dispatch+gate+land+supervise daemon. Flags: `--dispatch` (on), `--gate`, `--land`, `--supervise`, `--interval`, `--once`, `--singleton` |

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
| `ttorch skills [install]` | List/force-install recommended agent skills (e.g. `axi`, `ponytail`); ttorch also installs any missing ones automatically before a team launches |
| `ttorch init [--mode pr\|local\|validated\|trusted]` | Set up a repo's AGENTS.md + CLAUDE.md + delivery mode + profile |
| `ttorch profile [dir]` | Derive the repo's stack/commands/conventions into AGENTS.md |
| `ttorch version` / `help` | Version / full usage |

## The brief lint

A stored brief is a **snapshot**: `ttorch task add --brief-file` copies the file's contents into
the task, so editing the file afterwards reaches nobody, and a defective brief is discovered only
once a worker has already acted on it. `ttorch brief-lint <file>` runs the checks before that
happens, and `task add` runs the same checks automatically whenever a brief is supplied (an add
with no brief is unchanged). Five rules:

| Rule | What it requires |
| --- | --- |
| `target-branch` | The brief names its target as `origin/<branch>`, and that branch exists on the remote |
| `file-paths` | Every file path the brief cites exists at the ref the citation is about |
| `hard-counts` | A brief stating "there are 21 occurrences" carries verify-yourself wording near the number, and a reporting phrase somewhere |
| `prohibition` | A prohibition carries bounding wording in reach of the ban, and the brief states an allowed end state, rather than banning `push`/`merge`/PR outright |
| `standards` | The brief points at the standards the project expects |

Two of those are answered by git and the rest by vocabulary, which is the difference that
decides how far to trust a pass. See the note below the table.

Some details that matter in practice:

- **Which rules check a fact, and which check wording.** `target-branch` and `file-paths` are
  answered by git: the branch is on the remote or it is not, the path and the line are there or
  they are not. Everything else is answered by matching vocabulary, and a pass from those means
  the phrasing was present, not that the brief means it:

  - `hard-counts` looks for hedge vocabulary in the paragraph or list item holding the number,
    or the next one, plus a reporting phrase anywhere in the brief. It cannot tell a hedge from
    a sentence forbidding one: "I verified the count myself, so do not re-count it" carries the
    same words as a hedge and passes. No vocabulary fixes that, because the words are the same
    words.
  - `prohibition` looks for bounding vocabulary in the clause carrying the ban, or in a clause
    at the edge of the sentence that opens with the bound and bans nothing itself, and for an
    end-state phrase anywhere in the brief. It cannot tell whether the bound it found actually
    limits the ban.
  - `file-paths` skips its existence check where a create verb governs the mention, which is
    also a wording judgement, so a path that reads as being created is never looked for.
  - `standards` looks for the project's declared pointer, or for standards-shaped wording
    where no pointer is declared. It cannot tell a reference from a dismissal: "ignore the
    house conventions for this spike" mentions the conventions and passes.

  Each of these catches the bare form it was written for: a count stated with nothing around
  it, a blanket ban with nothing qualifying it, a path nobody asked for, a brief that never
  mentions standards at all. None of them certifies the brief is well written, and every note
  they print says wording rather than meaning.

- **Which ref a citation is resolved against.** A cited path *without* a line number is about the
  work's base, so it is resolved at `--ref` (the declared target by default). A `file:line`
  citation is about the commit it was *read at* — typically a gate finding quoting a worker's HEAD,
  where the file is longer than on the base — which is what `--citations-ref` names. When you name
  it, the answer is authoritative and a miss is a violation. When you do not, the citation is still
  resolved, against the base, so pointing a worker at a line of existing code needs no flag at all;
  but a miss there is reported as *unevaluable* rather than failed, because the line may perfectly
  well exist at the commit it was read at. Both refs, and whether the citations ref was given or
  defaulted, are named in the output.
- **A check that cannot run never passes.** Exit `0` means every rule was evaluated and passed,
  `1` that a rule was violated, `3` that at least one check *could not be evaluated* (an
  unreachable remote, a ref that does not resolve, a line citation the base cannot settle, or a
  project that disabled every rule), `4` that everything which ran passed but the project had
  disabled at least one rule, and `2` a usage error. One run reports every violation, with the
  rule named and the offending text quoted, and every summary line says how many of the rules
  ran, so an outcome cannot be read without its coverage.

  `--offline` skips the one rule that reaches the network (`target-branch`) and runs the
  other four, which is the answer to an unreachable remote that does not involve skipping
  every rule. The skip is counted as a rule that did not run, so an offline run reports
  reduced coverage rather than a pass. `task add` takes the same escape as
  `--brief-lint-offline`.

  `task add` treats `4` as a pass and proceeds, printing the coverage line. A project that
  disables a rule has declared that in its own `AGENTS.md`; refusing every briefed add there
  would break dispatch for the project and push people to `--no-brief-lint`, which skips all
  five rules instead of one. The separate status is for the caller who wants to insist on full
  coverage.
- **A brief is untrusted input**, pasted from issues and written by agents, and two rules query git
  once per item they find in it. So one run has an aggregate git budget (45s), the remote check
  verifies at most 3 distinct targets, and at most 64 distinct cited paths are resolved. Whatever a
  bound excludes is reported as unevaluable and named, never passed over. The budget is enforced
  rather than merely set: each git call runs in its own process group, which is killed when the
  budget is spent, and the wait on any pipe an escaped fork still holds is capped at 2s. Killing
  git alone is not enough, because the ssh or credential helper it forked keeps the output pipe
  open.

Per-project configuration lives in the repo's `AGENTS.md`, beside the delivery-mode line:

```markdown
- brief-standards: docs/standards/, CONTRIBUTING.md
- brief-lint-disable: hard-counts
```

`brief-standards` is the pointer a brief must cite to satisfy the `standards` rule — the rule is
configurable precisely so no repository is held to another's layout. A project that declares none
falls back to accepting any explicit standards reference; a project that declares the key with an
empty value is a broken declaration, reported as unevaluable rather than a pass.
`brief-lint-disable` turns individual rules off. Every override is echoed in the output, the
summary line always says how many rules ran out of how many exist, and a project that disables
*all* of them gets exit 3 and "nothing was checked" rather than a pass: the configuration lives in
the repository under review, ttorch writes that file itself through learnings promotion, and any
worker can commit it, so a run that evaluated no rule must not read as a clean gate.

## The scheduler daemon

`ttorch scheduler` is a deterministic Go loop (no LLM) that drains the task board on a
ticker (default every 5s). Each tick runs up to four independent, error-isolated passes
in fixed order — **supervise → dispatch → gate → land**:

- **Dispatch** — claim and launch every `pending` task that has a **declared footprint**, a
  **stored brief**, and free worktree capacity — **in parallel, even when footprints overlap**
  (each worker is git-isolated in its own worktree, so overlap costs only a rebase at land
  time). It claims atomically before spawning, so two ticks (or two daemons) can never
  double-dispatch. The `TTORCH_SERIALIZE_OVERLAP` off-switch restores the pre-parallel
  skip-on-overlap behavior. No-capacity, footprint-less, or brief-less tasks are **skipped**
  (left for the manager), never failed.
- **Gate** — gate `done` tasks in a **trusted** repo that don't yet carry a passing verdict:
  dispatch the adversarial reviewers and, **fail-closed**, record **only** an all-pass verdict
  (auto-minting the approval the land pass consumes). A blocking finding, prep refusal, or
  stalled reviewer records nothing and surfaces an actionable `gate_blocked` event for the
  manager; non-trusted repos and already-passing tasks are skipped. This takes hands-off gating
  off the manager so trusted done-work no longer waits for a manager turn.
- **Land** — land `done` tasks that **already carry a passing verdict**, through the same
  pipeline as `ttorch land`. It never lands ungated work.
- **Supervise** — reclaim only **verifiably-dead** workers (a confirmed-gone window or an
  expired lease, re-checked under a write lock — never pane-output inference) and re-dispatch
  them within a **bounded retry ceiling** (default 3), poison-pilling a task that exceeds it
  to terminal `failed` with an actionable event.

**It auto-starts with the manager by default**, running all four passes
(`scheduler --singleton --dispatch --gate --land --supervise`), logging to `~/.ttorch/scheduler.log`
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

A task's **footprint** (`--touches`) — a set of file paths/prefixes — declares the files it
will change. `spawn` refuses to dispatch a task onto files a live worker already holds
(override with `--force-overlap`); the **scheduler dispatches overlapping footprints in
parallel by default** (each worker isolated in its own worktree), serializing them only at
land time via rebase. `ttorch check-overlap` previews the overlap. Declare footprints at
**file granularity** — a whole-package footprint reports false overlap and inflates needless
land-rebases.

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
deep work happens in **workers**, which default to `--effort high`. `ttorch cc` also defaults
to `high`. `ultracode` (`xhigh` reasoning **plus** a session spinning up its own internal
sub-agent fleet) is **opt-in per task**, not a default — it is redundant with ttorch's own
orchestration and rarely earns its cost; reach for it with `--effort ultracode` on a single
task that genuinely needs it.

| Env | Applies to | Default | Effect |
| --- | --- | --- | --- |
| `TTORCH_MANAGER_EFFORT` | the manager | `high` | `low`…`max`, or `ultracode`/`off` |
| `TTORCH_EFFORT` | workers + `ttorch cc` | `high` | `ultracode`, a fixed `--effort` level (`max`…`low`), or `off` |

`ultracode` is not an `--effort` level — it is `xhigh` plus workflow orchestration (set via
`--settings`); the discrete levels go through `--effort`. The manager is deliberately *not*
ultracode by default, because that pushes a session to do deep work (and spawn its own
internal sub-agents) instead of delegating. Per-task effort (`ttorch spawn --effort <level>`)
resolves as explicit `--effort` > `TTORCH_EFFORT` > the classifier tier (below) > the kind
default (`high`), is persisted on the task, and is restored verbatim on resume.

```sh
TTORCH_EFFORT=max ttorch               # workers at highest raw reasoning, no nested orchestration
TTORCH_MANAGER_EFFORT=ultracode ttorch # opt the manager into ultracode
```

## Session model

Model is the **second dial**, orthogonal to effort: *which* model runs (Haiku → Sonnet →
Opus → Fable, or a full model id), independent of *how hard* it thinks. **Quality floor: code
is never written on a cheap model.** A task that writes code runs at **opus/high at minimum**
(equivalently fable/medium) — sonnet/haiku are never a default for code. Only **research** (a
read-only scout) may use sonnet, and the **manager** (planning) defaults to `opus`. The
savings come from confining sonnet to research, dropping ultracode, and escalating only on
failure — never from writing code with a weaker model.

| Env | Applies to | Default | Effect |
| --- | --- | --- | --- |
| `TTORCH_MANAGER_MODEL` | the manager | `opus` | an alias (`haiku`/`sonnet`/`opus`/`fable`/`opusplan`), a full model id, or `default`/`off` for claude's own default |
| `TTORCH_MODEL` | workers + `ttorch cc` | claude's default | as above; unset ⇒ no `--model` (claude's own default) |

Per-task model (`ttorch spawn --model <m>` / `ttorch task add --model <m>`) resolves as
explicit `--model` > `TTORCH_MODEL` > unset, is persisted on the task, and is restored
verbatim on resume — exactly like effort. Model and effort compose: `--model opus --effort
ultracode` is the top of the grid, `--model haiku --effort medium` the bottom.

**Automatic tiering.** When the scheduler auto-dispatches a backlog task that carries no
explicit model/effort *and* no `TTORCH_MODEL`/`TTORCH_EFFORT` override, it picks a tier from
the task's complexity signals (kind, footprint, title):

| Task class | Model | Effort |
| --- | --- | --- |
| scout / research (read-only) | `sonnet` | `medium` |
| ship (writes code) | `opus` | `high` |
| security · concurrency · migrations · finance (matched by footprint/title) | `opus` | `xhigh` |

Code is never assigned a model below `opus`; `sonnet` is confined to research and `haiku` is
not used at all. Precedence is **explicit per-task > `TTORCH_*` env > classifier tier > kind
default**, so an explicit `--model`/`--effort` or a global env always wins. Both the autonomous
dispatch path **and** a manual `ttorch spawn` route through this classifier.

**Escalation on failure.** A classifier-tiered task that fails and is retried bumps its model
one rung up the ladder each attempt — `sonnet → opus → fable` (`fable` is the top rung, ~2×
opus, reserved for work that could not be completed cheaper). So a ship task starts at `opus`
and only reaches `fable` on repeated failure, and a re-run scout that fills in or corrects its
research climbs from `sonnet` to `opus`/`fable`. A **pinned** `--model` never escalates
(explicit wins). The adversarial-review trust gate keeps reviewers on your most
capable default (it is deliberately *not* cheapened), since in trusted mode it can authorize a
merge unread.

```sh
TTORCH_MODEL=sonnet ttorch                 # fleet on Sonnet; escalate the hard ones with --model opus
ttorch task add fix-auth --project 1 --touches internal/auth --model opus --effort ultracode
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

**The view tab is read-only.** It shares the worker's actual pane rather than a copy of
it, so a writable tab would be a second keyboard on a running agent with nothing on
screen to say so. You can close the tab and scroll the worker's history, but you cannot
type into the worker from it. Steer a worker through the manager with
`ttorch send <id> <text>`, which addresses the pane with no client at all so a view tab
cannot intercept it.

This needs tmux 3.2 or newer, which is where read-only view clients arrive. On an older
tmux the tab opens writable and ttorch prints a warning naming your version; `ttorch
doctor` reports the same thing. If you would rather have no tab than a writable one, set
`TTORCH_WORKER_TABS=0` and watch with `ttorch peek`.

**Scrolling puts the worker's pane into copy-mode.** `Ctrl-b PgUp` in the view tab scrolls
the shared pane, not a copy of it, so the worker's own pane enters copy-mode and stays
there until you press `Escape`. While it is in copy-mode a steer cannot reach the agent, so
`ttorch send` refuses with a message telling you to press Escape rather than reporting a
success that never arrived. Scroll freely; just press `Escape` when you are done.

**Read-only is an accident guard, not a security boundary.** It stops a stray keystroke in a
watcher tab. It does not contain anyone: anybody with a shell on the machine can run `tmux
attach -t ttv-wk-<TASK>` (or attach to the `ttorch` session itself) and get a writable
client. Four further limits:

- `ttorch send` deliberately bypasses the read-only check, because that is what keeps
  steering working, so read-only says nothing about a *programmatic* steer. A send that lands
  on a worker sitting at a numbered menu is still consumed as a menu selection and still
  reports success.
- A tmux below 3.2 has no read-only view client, so the tab opens writable, as above.
- `ttv-` sessions created by a pre-fix ttorch persist writable until the worker is respawned.
- If you have bound a key to `switch-client -r`, pressing it in a view tab clears the
  read-only flag and the tab becomes writable, with nothing on screen to show it changed.
  `switch-client` is one of the few command families tmux lets a read-only client run. No
  stock binding reaches `-r` (the default bindings use `-p`, `-n`, `-l` and `-t`), so this
  needs a binding of your own. There is no fix available to ttorch: tmux key tables are
  server-global, so unbinding it in the view would unbind it for the worker session too.

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
make build      # build ./bin/ttorch
make test       # full suite — go test ./... (incl. the slow orchestrator e2e tests)
make test-fast  # fast lane — go test -short ./... (skips the slow e2e tests)
make lint       # go vet + gofmt check
make dist       # cross-compile all targets + checksums into ./dist
```

The test surface is split into a **fast lane** and the **full suite**. `make test-fast`
(`go test -short`) skips the slow `internal/orchestrator` integration (e2e) tests, which
drive real tmux/git/rebase/validate and dominate the wall-clock (~100s); it finishes in
seconds and is what the trusted gate runs locally (`.ttorch/validate.sh`). `make test` is
the full suite and is what CI runs on every change. The fast lane is a turnaround
optimization, never a replacement: the full suite (incl. the e2e tests) still runs in CI
before anything lands, so the gate is not weakened.

Contributions keep a professional, neutral tone — no themed personas or role-play
vocabulary. See [`AGENTS.md`](AGENTS.md) for the full contributor conventions, and
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for how the system fits together.

## Releases & CI

- **CI** (`.github/workflows/ci.yml`) runs `go vet`, the `gofmt` check, and the **full**
  `make test` (incl. the slow `internal/orchestrator` e2e tests) on macOS and Linux for
  every push and pull request. It installs tmux first so those integration tests actually
  execute rather than self-skip — CI is the one place the full suite runs, so it is the
  authoritative gate and should be a required check on the default branch.
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
