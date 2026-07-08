# Architecture

This is the as-built reference for ttorch v0.10.0: what the system is made of, how the
pieces talk, and — most importantly — **where the deterministic engine ends and LLM
judgment begins**. If you want to *use* ttorch, start with [`ONBOARDING.md`](ONBOARDING.md);
read this when you want to know *how it works* or you're changing it.

> The older [`design/sqlite-event-architecture.md`](design/sqlite-event-architecture.md) is
> a historical design proposal written before the SQLite migration landed. This document
> supersedes it as the description of the running system.

## 1. The operating model: lead ↔ manager ↔ daemon ↔ worker

ttorch runs a team of Claude Code agents under four roles with a strict division of labor.

```
   you (lead)
      │  plain language, in one manager tab
      ▼
  ┌─────────┐   plans, briefs, gates, approves      ┌──────────────────┐
  │ manager │ ───────────────────────────────────►  │   SQLite store    │
  │  (LLM)  │ ◄─── wakes on actionable events ─────  │ (source of truth) │
  └─────────┘                                        └──────────────────┘
                                                        ▲   ▲        │
                          dispatch / land / supervise   │   │        │ leases,
                       ┌──────────────────────────────┐ │   │        ▼ events
                       │   scheduler daemon (no LLM)   │─┘   │   ┌──────────┐
                       └──────────────────────────────┘     └───│ workers  │
                                spawns / reclaims              │  (LLMs)   │
                                                               └──────────┘
                                                          one isolated git worktree each
```

- **The lead (you).** A human. You talk *only* to the manager, in plain language, in a
  single manager tab. You never drive a worker directly, and you are the one who approves
  delivery (except in trusted mode — see §4).
- **The manager.** A Claude Code session running the `ttorch-manager` skill. It does the
  work that needs judgment: break a goal into tasks, write each task a precise brief, run
  the adversarial-review gate to *produce* a verdict, answer blocked workers, and surface
  decisions to you. It is deliberately *not* a coder — it delegates all substantive work,
  including review, to workers. It carries no durable memory of its own; it re-derives
  state from the store every time it wakes.
- **The scheduler daemon.** A plain Go loop with **no LLM in it**. It mechanically
  dispatches ready work, lands already-gated work, and recovers crashed workers. It
  auto-starts with the manager by default (§3).
- **Workers.** Claude Code sessions, one per task, each in its own isolated git worktree.
  A worker executes exactly one task, reports status to the store, and never touches
  another worker's files.

Everything coordinates through **one SQLite database** (§5). No component holds shared
in-memory state; the store is the single source of truth, which is why a manager, the
daemon, and a recovery command can all act on the same fleet without stepping on each
other.

### The deterministic / judgment boundary

This is the central design idea. Two kinds of work happen in a ttorch session:

| Deterministic (the daemon, in Go) | Judgment (the manager, an LLM) |
| --- | --- |
| Dispatch briefed, footprinted backlog in parallel (overlap and all) | Decide *what* the tasks are and write their briefs |
| Land work that **already** carries a passing verdict | **Produce** that verdict (run the review gate) |
| Reclaim a worker that **verifiably** died, within a retry budget | Decide whether a blocked worker should change course |
| Hold the worktree pool (never two workers in one worktree) | Approve a non-trusted merge; talk to the lead |

The daemon never makes a judgment call: it only does work whose safety it can *prove* from
the store (a declared footprint, free worktree capacity, a passing verdict, a clean
land-rebase, a confirmed-dead window). Anything it cannot prove, it leaves for the manager. That boundary is what lets
the fleet run hands-off without an LLM in the dispatch loop — and is why "gating" (a human
or the manager recording a verdict) is always separate from "landing" (the daemon merging
gated work).

## 2. The task lifecycle: dispatch → gate → land → supervise

A task moves through the store like this:

```
  task add ──► pending ──► active ──► done ──► (gated) ──► delivered
   (brief +      │  ▲         │         │                      
    footprint)   │  │ reclaim │ report  │ trust record         
                 │  └─────────┘  done   │ (passing verdict)    
                 │   (supervise)        ▼                      
                 └──────────────────► failed (retries exhausted)
```

1. **Plan.** You tell the manager a goal; it creates tasks (`task add`), each with a
   file-granular **footprint** (`--touches`) and a stored **brief** (`--brief-file`). A
   task with both can be dispatched hands-off.
2. **Dispatch.** The scheduler's dispatch pass (or the manager, or you with `spawn`) claims
   a `pending` task atomically and launches a worker in a pooled worktree on its full
   brief. The task is now `active` and holds a lease.
3. **Work.** The worker syncs (fetch + rebase onto the default branch), implements the
   brief, runs the repo's build/test/lint, and reports `done`. Each report extends its
   lease (a heartbeat). Reporting is enforced, not just asked: every worker session carries a
   worker-only Stop hook (`ttorch stop-hook`, installed in its worktree-local settings) that
   fires when the worker goes idle and **blocks the stop while the task is still `active`**,
   reminding it to run `ttorch report done|blocked|needs-input`. Without it, a worker that
   commits and idles without reporting leaves finished work invisible to the land/gate
   machinery — the "committed but stuck at active" dead zone. Opt out with `TTORCH_NO_STOP_REPORT`.
4. **Gate.** Finished work is reviewed: the manager (or you) runs `ttorch validate` and the
   adversarial-review gate (`trust prep` → reviewers → `trust record`), which writes a
   durable, commit-pinned **verdict**. Gating is a judgment step; it never merges.
5. **Land.** Only *gated* work lands. The scheduler's land pass (or `ttorch land`) fetches,
   rebases onto the current default, re-validates the committed sha, integrates per the
   repo's delivery mode, and fast-forwards the local default branch. The task becomes
   `delivered`.
6. **Supervise.** If a worker verifiably dies, the supervise pass reclaims its task back to
   `pending` (so dispatch restarts it) up to a bounded retry count, then poison-pills it to
   terminal `failed` with an actionable event.

## 3. The scheduler daemon

`ttorch scheduler` is a deterministic Go loop that drains the task board on a ticker
(default every 5s). Each tick runs up to three independent passes **in fixed order —
supervise, then dispatch, then land** — each guarded by its own toggle and error-isolated
so a transient failure in one never stops the others.

- **Dispatch** (`--dispatch`). Re-derive the ready backlog and dispatch every `pending`
  task that declares a non-empty footprint, has a stored brief, and whose repo has a free
  worktree slot — **in parallel, even when footprints overlap**. Overlap across separate
  worktrees is not a dispatch hazard: each worker is git-isolated in its own pooled worktree,
  so two tasks editing one file never see each other; the only cost is a rebase when the
  second one lands, which the land pass serializes (below). The off-switch
  `TTORCH_SERIALIZE_OVERLAP` restores the pre-parallel behavior of skipping an overlapping
  task until the holder finishes. Each selected task is claimed atomically (a `BEGIN
  IMMEDIATE` status re-check) *before* the expensive spawn, so two ticks — or two daemons, or
  the daemon and the manager — can never double-dispatch. The "never two workers in the same
  worktree" invariant is held by per-tick capacity accounting and the file-locked worktree
  pool, **not** by the footprint check. A task that **declares no footprint**, **has no stored
  brief**, or has no free capacity is silently skipped and left for the manager; it is never
  failed.
- **Land** (`--land`). Find `done` tasks that **already carry a passing verdict** and land
  them through the same pipeline as `ttorch land` (with the verdict requirement on). It
  never lands ungated work: a missing or non-passing verdict is skipped, and the
  commit-pinned verdict + single-use approval check inside the land path is the real
  authority. Overlapping lands serialize here: each task rebases onto the current default
  before its fast-forward, and a non-clean rebase is always aborted (never force-merged) and
  surfaced as an actionable `land_rebase_conflict` for the manager to resolve.
- **Supervise** (`--supervise`). Reclaim only **verifiably-dead** workers — a
  watcher-recorded "window gone" event that is still the task's latest sign of life, or a
  genuinely-expired lease re-checked under a write lock. It is *never* pane-output
  inference: a heartbeat or a re-dispatch cancels a pending reclaim. A reclaimed task goes
  back to `pending` (`retry_count++`) for the dispatch pass to restart, or — once it
  exceeds the retry ceiling (default 3) — to terminal `failed` with an actionable event.
  Supervise itself never restarts a worker; it only moves tasks. The bounded retries turn a
  flapping task into a single terminal failure instead of an infinite restart storm.

**Auto-start (default on).** When the manager starts (fresh, re-attach, or restore) it
forks the installed binary as `ttorch scheduler --singleton --dispatch --land --supervise`
— **all three passes** — detached, logging to `~/.ttorch/scheduler.log` (never the manager
pane). Disable it with a falsey **`TTORCH_SCHEDULER_AUTOSTART`** (`0`/`false`/`no`/`off`);
any other value or unset leaves it on. Auto-start launches the *installed* binary, so it is
a quiet no-op in a from-source dev tree.

> **Two different defaults.** A bare `ttorch scheduler` you run by hand defaults to
> **dispatch-only** (`--land` and `--supervise` default off). The manager's *auto-started*
> daemon runs all three. (The in-binary `ttorch help` text still describes the scheduler as
> opt-in — that wording predates v0.10's default-on auto-start.)

**Singleton.** `--singleton` takes a `flock`-as-truth advisory lock on
`~/.ttorch/state/scheduler.pid`. At most one daemon holds it; a crashed holder's lock frees
automatically; the launcher probes before forking. So even a race between two manager
starts can never run two daemons against one store.

## 4. The trust gate and delivery modes

The trust gate lets a repository merge a worker's diff gated on **independent AI reviewers
plus the repo's own build/test/lint**, enforced in Go at the merge point rather than by
convention.

### Adversarial review

Review has **three blocking dimensions — correctness, scope, security** — each run by an
independent reviewer subagent that only ever writes findings, never edits code. (A fourth,
**QA / test-adequacy**, exists but is advisory and *not* part of the gate.) The reviewer
set **scales to the diff size**, computed from the authoritative `git diff --name-only -z`
file list (never scraped from the patch body, so a worker can't hide a code file behind a
quoted name):

- **docs-only** (every changed file is inert prose) → correctness + scope (security dropped).
- **trivial** (one non-binary code file, ≤ 20 added+removed lines) → correctness + security
  (scope dropped).
- **substantial** (anything else, or any uncertainty / git-stat failure) → the full three.

The flow is `trust prep` → reviewer subagents → `trust record`:

1. **`trust prep`** materializes the reviewers' inputs — the committed **three-dot diff**
   (`git diff <default>...<head>`, so it contains only the branch's own changes), the
   brief, a fresh validate of the committed sha, the reviewed HEAD, and the scaled
   dimension set — refusing a dirty worktree or a stale base.
2. **Reviewers** read statically, trust the green validate rather than re-running the
   suite, and each emit per-dimension JSON findings. Any **high or critical** finding
   blocks; low/medium are advisory.
3. **`trust record`** aggregates the reports — in Go, not free-typed by an LLM, so a missing
   or malformed report **fails closed** to "block" — into a single **verdict**. The verdict
   is **commit-pinned** (`reviewedSha`) and **content-pinned** (`diffId`, a hash of the
   reviewed patch bytes), stored as a durable DB row with **no TTL**: it never lapses by
   age, only by a content change or a consuming merge. A clean rebase that keeps the diff
   byte-identical can carry the verdict (and its approval) forward; any content change
   forces a full re-gate.

### Delivery modes

The mode lives in the repo's `AGENTS.md`/`CLAUDE.md` ttorch-managed block (set by
`ttorch init --mode`); it defaults to `pr`. **Changing the gate itself — that block or
`.ttorch/validate.sh` — always requires a human.**

| Mode | How work integrates | Who authorizes the merge |
| --- | --- | --- |
| `pr` (default) | Push a branch, open/merge a PR, then fast-forward the local default | GitHub review / branch protection |
| `local` | Approval-gated local fast-forward | The lead's `ttorch approve` |
| `validated` | Approval-gated local fast-forward (identical to `local` in v0.10.0) | The lead's `ttorch approve` |
| `trusted` | Approval-gated local fast-forward, full review + validate gate | **A passing verdict + fresh green validate — no separate human approval** |

`local`, `validated`, and `trusted` all integrate through the same approval-gated local
fast-forward; the verdict + fresh-validate gate is layered on only when the repo is
`trusted` *or* the lead passes `--require-verdict` (`gated := requireVerdict || mode ==
"trusted"`). So `validated` is an accepted mode but, in v0.10.0, behaves identically to
`local`; there is no `validated`-specific merge path in the code today.

**Trusted mode is the only path where work merges without a human reading the diff.** A
passing commit-pinned verdict plus a fresh green validate auto-mints the approval token
(`approved_by = "auto"`). Guardrails make this safe:

- It requires a **`.ttorch/validate.sh` on the default branch** — the gate validates the
  immutable committed sha using the *default-branch* gate script (a worker can't weaken its
  own gate), and a repo with **no checks detected is a hard block**, never a pass. Without
  the script, the trusted auto-merge is refused and a human `ttorch approve` is required.
- A trusted auto-merge **cannot change a gate-definition file** (`.ttorch/validate.sh` or
  `AGENTS.md`); such a diff is refused and needs a human.
- The audit record is written and flushed **before** the irreversible fast-forward; an
  unauditable merge aborts.

The **approval token** (in `internal/approval`) and the **review verdict** (in the DB) are
deliberately distinct authorizations — "a human read this" vs. "the reviewers passed it" —
so an audit can always tell them apart. The token is time-boxed (`--ttl`, default 10m),
single-use, and bound to the reviewed commit: if the worker's HEAD moved after approval,
the merge is refused.

### Advisory audits

A **standalone security audit** (`ttorch security-review`) runs the security reviewer in
**every** delivery mode (including `pr`) and is meant to run before delivery; an **optional
QA audit** (`ttorch qa-review`) checks test adequacy. Both write file verdicts and surface
findings, but they **never** mint an approval, touch the gate's verdict, or block a merge.

## 5. Durable state (SQLite)

All orchestration state lives in one SQLite database (`~/.ttorch/state.db`), accessed
through `internal/db.Store`. The schema is built by ordered, embedded migrations
(`internal/db/migrations/NNNN_*.{up,down}.sql`). Concurrency safety rests on a single
writer connection (`SetMaxOpenConns(1)`) plus WAL and `BEGIN IMMEDIATE` transactions, so
the claim/reclaim primitives re-read a row under the write lock and a single winner emerges
(SQLite has no `SKIP LOCKED`).

- **Work hierarchy.** `projects → epics → phases → tasks`. The **task** is the unit of
  work, carrying status, owner, free-text stage, the **footprint** (a JSON file-touch set),
  lease/retry bookkeeping, the resolved reasoning effort, and a delivery summary. A
  project's cached `delivery_mode` is **display only** — the gate always re-reads
  `AGENTS.md`.
- **Task status** is one of `pending`, `active`, `needs_input`, `blocked`, `done`,
  `delivered`, `torn_down`, `abandoned`, plus the terminal **`failed`** poison-pill state.
- **Append-only event log.** Every state change appends an immutable event whose
  autoincrement `id` is the monotonic **watermark**. The log is both the audit history and
  the live signal that wakes the manager. Only transitions into `needs_input`/`blocked`/
  `done` caused by a *worker* actor are flagged **actionable**; the manager records its own
  events as non-actionable so it can never self-wake.
- **Durable verdicts.** One commit/content-pinned verdict row per task (§4); no TTL. A gated
  merge consumes (deletes) it so the same verdict can't authorize a second merge.
- **Leases.** Each active task holds a lease (`lease_owner`, `lease_expires_at`, default 2h)
  and a bounded `retry_count`/`max_retries` (default 3). A worker's `report`/`stage`
  extends the lease in the same transaction (a heartbeat). Reclaim re-reads the lease under
  the write lock and either returns the task to `pending` (retry) or poison-pills it to
  `failed`.
- **Delivery provenance.** When work lands, the task's summary columns (`gate_passed`,
  `approved_by`, `reviewed_sha`) and a `delivered`/`merged` event are written in one
  transaction, so the verdict row and the summary can never drift apart.

Migrations, in order: **0001** initial hierarchy + events + manager singleton; **0002**
durable verdicts; **0003** task leases + the terminal `failed` status; **0004** the
per-task reasoning-effort column.

## 6. The event-driven watcher (zero-token supervision)

An idle team must cost nothing, and a manager that polls would burn tokens forever. So the
manager does not poll — it arms **`ttorch watch`** as a background task on every turn in
which it is not awaiting the lead. `watch` blocks on the store until an **actionable** event
(or a timeout), absorbs a short burst (`--coalesce`, default 750ms), prints the coalesced
batch and a machine-readable `WATCH_WATERMARK=<n>` line, and **exits**. Its exit is a
background-task completion, and **the harness re-invokes the manager through its own
completion channel** — no process ever types into the manager session.

That last property is the **picker-safety** guarantee: because the wake is the harness's own
notification and never stdin or a keystroke, a watcher firing cannot disturb an open
`AskUserQuestion` picker or a mid-generation turn. (The watcher *does* nudge a stalled
*worker* window with a plain "continue" to recover it from an API stall — but never the
manager.)

- **`ttorch await-lead`** sets a flag that keeps a running watcher **silent** while a
  decision sits with the lead, so the manager isn't pulled off a pending question. Arming
  `watch` clears the flag (the manager is back in the loop).
- **`ttorch watchdog`** is an *external* liveness net (run from launchd/cron) for the case
  where the manager's own LLM turn died with actionable work waiting. It re-pokes the
  manager **through the same DB-event channel** `watch` uses — never a keystroke — and is
  idle-aware, so it no-ops when nothing is waiting.

## 7. Worktrees, footprints, and isolation

Each worker runs in its own **git worktree** drawn from a per-repository **pool** under
`~/.ttorch/worktrees` (size **`TTORCH_MAX_WORKTREES`**, default 16). `Acquire` reuses a
clean idle slot or creates a new one, always re-anchored on the freshly-fetched default tip;
`Release` resets a finished slot clean and **keeps it for reuse** (so teardown returns the
slot to the pool rather than destroying it). The pool's free-slot count is the dispatch
**capacity** the scheduler respects.

A task's **footprint** (`--touches`, a set of file paths/prefixes) declares the files it will
change. `spawn` refuses to dispatch a task onto files a live worker already holds unless you
pass `--force-overlap`; the **scheduler dispatches overlapping footprints in parallel by
default** (each worker isolated in its own worktree), serializing them only at land time via
rebase. `ttorch check-overlap` previews the overlap for a proposed footprint. Footprints must
be declared at **file granularity** — a whole-package footprint reports false overlap and
inflates needless land-rebases (and, under `TTORCH_SERIALIZE_OVERLAP`, idles scheduler slots).

## 8. Sessions and reasoning effort

Every session is a `claude --dangerously-skip-permissions` process (work is confined to
isolated worktrees). They differ only in system prompt, session id, and **reasoning
effort**:

- The **manager** runs at `TTORCH_MANAGER_EFFORT` (default `high`), deliberately *not*
  ultracode — ultracode would push it to do deep work itself instead of delegating.
- **Workers** and `ttorch cc` run at `TTORCH_EFFORT` (default `high`).

`ultracode` is **not** an `--effort` level — it's a Claude Code session feature (xhigh
reasoning *plus* dynamic workflow orchestration) enabled via `--settings`. It is **opt-in per
task**, not a default: it is redundant with ttorch's own orchestration and rarely earns its
cost. The discrete `--effort` levels are `low|medium|high|xhigh|max`; `off`/`none`/`default`
add no flag. Per-task effort resolves as **explicit `--effort` > `TTORCH_EFFORT` > classifier
tier > kind default** (`high`), is persisted on the task row, and is restored verbatim on resume
— so changing the environment later doesn't change an already-spawned worker.

**Model** is the orthogonal second dial — *which* model (`haiku`/`sonnet`/`opus`/`fable`/
`opusplan` or a full id) versus *how hard* it thinks. It mirrors effort end-to-end: a per-task
`model` column, `harness.ModelArgs`/`ResolveWorkerModel`, `TTORCH_MODEL` / `TTORCH_MANAGER_MODEL`,
`spawn --model` / `task add --model`, persisted and restored on resume. Unset passes no
`--model` (claude's own default), except the manager defaults to `opus` — planning (and
follow-up that fills in or corrects research) must not be under-powered. **Quality floor: code
is never written on a cheap model.** When the scheduler auto-dispatches a backlog task — **or**
a manual `ttorch spawn` runs — whose model/effort are unset (and no env override applies), a
small classifier (`internal/scheduler/tier.go`) picks a tier from complexity signals — a
read-only scout (research) ⇒ `sonnet`/`medium`, a security/concurrency/migration/finance
footprint or title ⇒ `opus`/`xhigh`, every other ship (it writes code) ⇒ `opus`/`high`. Code
is never assigned below `opus`; `sonnet` is confined to research and `haiku` is unused. Precedence is
**explicit per-task > `TTORCH_*` env > classifier > kind default**; the pairs it emits are valid `(model, effort)` combinations (claude silently
downgrades an unsupported effort, and fast mode is opus-only). A classifier-tiered dispatch is
flagged (`tasks.auto_tiered`); on a retry such a task re-derives its tier and **escalates the
model one rung up the ladder** (`sonnet→opus→fable`, clamped at fable) per `retry_count`, so a
ship task starts at `opus` and only repeated failure reaches `fable`, while a re-run scout that
corrects its research climbs from `sonnet` to `opus`/`fable` — a user/env pin
(`auto_tiered=0`) never escalates. The autonomous dispatch also now
forwards the persisted effort, closing a gap where it fell back to the kind default. The
adversarial-review gate keeps its reviewers on claude's default model (not cheapened), since a
trusted-mode verdict can authorize a merge unread.

Sessions resume by stable `--session-id` (`--resume`), with `--continue` and a fresh
re-launch from the brief as fallbacks, so neither lead nor worker is ever stranded at a dead
shell. See the README's "Resuming after a reboot or upgrade" for the user-facing behavior.

## 9. Validation

Two distinct validation surfaces exist, and they must not be confused:

- **`ttorch validate <id>`** (advisory) resolves a task's worktree and runs its checks: a
  repo-provided **`.ttorch/validate.sh`** if present (one `sh .ttorch/validate.sh` step),
  else ecosystem defaults — Go (`go build`/`go vet`/`gofmt`/`go test`) or the Node
  `build`/`lint`/`test` scripts that exist. Each step runs under a per-step timeout
  (`TTORCH_VALIDATE_TIMEOUT`, default 10m); all steps run to completion; any non-zero exit
  fails that check.
- **The merge gate** (enforcing) validates the **immutable committed sha** against the
  validation definition resolved from the **default branch** — so a worker cannot weaken its
  own gate by editing the script on its branch — and treats "no checks detected" as a hard
  block.

`ttorch ci-parity [dir]` is a separate tool that reproduces the repo's actual GitHub Actions
run-steps locally so "green here" matches "green in CI". It fails **closed**: it auto-runs
only an allowlist of bare build/test/lint entrypoints and skips (and reports) anything else.

> **Trust:** validation and ci-parity execute the repository's own commands (and any
> `.ttorch/validate.sh`) on your machine with your credentials. Only run them against
> repositories and worker output you trust.

## 10. On-disk layout

```
~/.ttorch/
  bin/ttorch            the binary (user-owned, so macOS self-update works)
  manifest.json         sha256 ledger of managed files (clobber-safety)
  state.db              the SQLite store (single source of truth)
  state/                watch.pid, scheduler.pid, per-task approval tokens
  data/<id>/            a task's stored brief.md and review inputs
  worktrees/            the per-repository worktree pool
  audit.log             approvals + merges
  scheduler.log         the auto-started daemon's output
~/.claude/
  skills/ttorch-manager|ttorch-validate|ttorch-review/
  agents/ttorch-worker.md, ttorch-reviewer-{correctness,scope,security,qa}.md, …
  commands/ttorch.md    the /ttorch slash command
  hooks/prompt-reminders.sh
  AGENTS.md             managed global guidance block; CLAUDE.md symlinks to it
~/.agents/skills/…      vendor-neutral mirror
```

Managed content is reconciled clobber-safely (`internal/manifest`): an update upgrades files
you haven't touched but never overwrites one you edited — the new version is parked beside
it as `<name>.ttorch-new`. Your state under `~/.ttorch/state.db` and `~/.ttorch/data` is
never touched by an update. See the README's "What gets installed" and "Updating" for the
user-facing detail.

## Further reading

- [`ONBOARDING.md`](ONBOARDING.md) — install and the daily loop.
- [`../README.md`](../README.md) — command reference, install verification, configuration.
- [`design/sqlite-event-architecture.md`](design/sqlite-event-architecture.md) — the
  historical design proposal that preceded the SQLite + event-watch migration.
