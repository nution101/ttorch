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
                   dispatch / gate / land / supervise   │   │        │ leases,
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
  dispatches ready work, gates trusted done-work, lands already-gated work, and recovers
  crashed workers. It auto-starts with the manager by default (§3).
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
(default every 5s). Each tick runs up to four independent passes **in fixed order —
supervise, then dispatch, then gate, then land** — each guarded by its own toggle and
error-isolated so a transient failure in one never stops the others.

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
- **Gate** (`--gate`). Find `done` tasks in a **trusted** repo that do not yet carry a
  passing verdict, dispatch the adversarial reviewers, and — **fail-closed** — record
  **only** an all-pass verdict, auto-minting the approval token the land pass consumes. A
  blocking finding, prep refusal, missing/mismatched report, or stalled reviewer records
  nothing and instead surfaces an actionable `gate_blocked` event for the manager to
  adjudicate; non-trusted repos and already-passing tasks are skipped. An atomic `gater:`
  claim (distinct from the land pass's `lander:`) means exactly one actor gates a task per
  tick. This takes the mechanical gate off the LLM manager so trusted done-work no longer
  waits for a manager turn — the merge/land authority (a fresh, commit-pinned passing
  verdict + single-use approval consumed at the fast-forward) is unchanged.
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
forks the installed binary as `ttorch scheduler --singleton --dispatch --gate --land --supervise`
— **all four passes** — detached, logging to `~/.ttorch/scheduler.log` (never the manager
pane). Disable it with a falsey **`TTORCH_SCHEDULER_AUTOSTART`** (`0`/`false`/`no`/`off`);
any other value or unset leaves it on. Auto-start launches the *installed* binary, so it is
a quiet no-op in a from-source dev tree.

> **Two different defaults.** A bare `ttorch scheduler` you run by hand defaults to
> **dispatch-only** (`--gate`, `--land`, and `--supervise` default off). The manager's
> *auto-started* daemon runs all four. (The in-binary `ttorch help` text still describes the
> scheduler passes as opt-in — that wording predates the default-on auto-start.)

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
- A trusted auto-merge **cannot change a gate-definition file**; such a diff is refused. The
  covered set is `.ttorch/validate.sh` and `AGENTS.md` (the repo-local gate config, present in
  every managed repo), `content/skills/**` and `content/agents/ttorch-reviewer-*` (which
  `content.go` embeds and the installer lays down under `~/.claude`, so they are the gate's
  live reviewer and manager instructions, not documentation about the gate), `internal/review/**`,
  `internal/approval/**`, `internal/validate/**`, `internal/projectinit/**` (which parses
  `AGENTS.md` into the delivery mode) and
  `internal/orchestrator/{gate,merge,validate,validatecache}.go` (the Go code that decides),
  and `.github/workflows/**` (the full suite, which `.ttorch/validate.sh` defers to by name
  because it runs only the fast lane). Paths are matched case-insensitively on both sides. On
  a **gated** merge (trusted mode, or any mode with `--require-verdict`) a human approval does
  not wave it through either: it needs `ttorch approve <id> --allow-gate-change`, and the
  merge audit line names the file.
- An ungated `local`/`validated` merge **does not run the check at all**. That path lands an
  `AGENTS.md` change on a plain human approval with no gate-config check and no audit line
  naming it, and `AGENTS.md` is what `projectinit.ReadMode` reads to decide trusted mode — so
  an ungated merge can flip a repo into auto-merge, unaudited. This is pre-existing and is not
  fixed by the gate-change scope.
  The scope lives on the approval token, so an expired token re-minted from the durable
  verdict comes back without it and the merge refuses again (fail closed). This makes a gate
  change an explicit, audited act; it does not make the token unwritable by a process running
  as the lead.
- The audit record is written and flushed **before** the irreversible fast-forward; an
  unauditable merge aborts.

### Why the gate-config set stops where it does

The guard's cost is a flag on every merge that trips it, so the set is bounded by measurement
rather than by what sounds prudent. Measured over the **196 non-merge commits reachable from
`b642ba6`**, counting a commit once if it touches any covered path:

| Set | Commits | Share |
|---|---|---|
| `.ttorch/validate.sh` + `AGENTS.md` (the original guard) | 10 | 5% |
| + `content/skills/**`, `content/agents/ttorch-reviewer-*` | 44 | 22% |
| + `internal/review/**` | 48 | 25% |
| + `internal/approval/**` | 47 | 24% |
| + `internal/validate/**` | 46 | 23% |
| + `internal/projectinit/**` | 47 | 24% |
| + `.github/workflows/**` | 49 | 25% |
| + `internal/orchestrator/{gate,merge,validate,validatecache}.go` | 52 | 27% |
| **the set above, all together** | **68** | **35%** |
| ~~+ `internal/orchestrator/**` and `internal/review/**` wholesale~~ | 109 | 56% |

(Each middle row is the landed 22% set plus that one addition, so the rows overlap and do not
sum. An earlier count of the same corpus reported 9 and 43 for the first two rows; the
difference is the root commit, which `git diff-tree` skips without `--root`. It changes no
conclusion.)

The last row is the answer to "why not just cover the packages". `internal/orchestrator/`
alone is 78 of the 196 commits — spawn, the land queue, the scheduler wiring, the overlap
planner — and covering it wholesale puts **more than half of every change** behind the flag. A
flag that fires on most commits is not a signal; it is a formality, and it launders a real
gate change through a habit. The four files that actually resolve, enforce and cache the
decision cost 8 commits instead of 62.

A file list over a package is brittle in a way a directory prefix is not: this package has
already been re-split once (`140d2b91`), and a later split that moved `MergeLocal` into a new
file would drop it out of coverage silently. `TestGateConfigCoversTheDecidingCode` parses the
package and fails if any deciding function lands in a file `gateConfigFiles` does not name, so
that decay is loud.

`.github/workflows/**` is in for a different reason. The trusted gate never consults CI, which
is the argument against it — but this repo's `.ttorch/validate.sh` runs only `make test-fast`
and says in its own header that the full suite, including the orchestrator e2e tests, runs in
CI as the required check. CI is therefore half of what "validated" means here, and weakening
`ci.yml` weakens every later change's validation through the same delayed diff channel that
put the skills on the list. It costs 5 commits.

What the numbers do not fix: at 35%, roughly one merge in three in this repo needs
`--allow-gate-change`, and the flag is a boolean. A lead who passes it by reflex authorizes
exactly as much as one who read the diff. The audit line naming the file survives either way,
which is the guard's durable half. Making the flag take the expected paths — so a bare
`--allow-gate-change` stops working and the approval names what it covers — is the obvious
next step and is not done here.


The **approval token** (in `internal/approval`) and the **review verdict** (in the DB) are
deliberately distinct authorizations — "a human read this" vs. "the reviewers passed it" —
so an audit can always tell them apart. The token is time-boxed (`--ttl`, default 10m),
single-use, and bound to the reviewed commit: if the worker's HEAD moved after approval,
the merge is refused.

`ttorch approve` refuses two shapes an approval takes when it is not the lead running it by
hand: a non-interactive invocation (stdin is not a character device, or is `/dev/null` — what
a wrapper, a script or an agent's shell tool produces), and an invocation from inside a
worker's context (`$TTORCH_TASK_ID` is set, or a `.ttorch/task` file exists at or above the
cwd). **This narrows a class; it does not close one.** A worker session runs as the lead with
the same filesystem authority: it can write `~/.ttorch/state/<id>.approve` directly without
calling `ttorch approve` at all, and if it does call it, `env -u TTORCH_TASK_ID`, a `cd`, and
a pty defeat all three checks. The reason to have the guard is that the realistic cases are
accidental and injected approvals, and it stops both of those.

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

### The brief lint

Validation gates a worker's output; the brief lint (`internal/brieflint`) gates its **input**. A
stored brief is a snapshot — `task add` copies it into the task, so a later edit of the file
reaches nobody — which means a defective brief is only discovered once a worker has acted on it.
So the checks run at `ttorch task add` (whenever a brief is supplied) and standalone as
`ttorch brief-lint <file>`: a declared target branch that resolves on the remote, cited paths that
exist, a hard count with verify-yourself wording near it, a prohibition carrying bounding wording
in reach of the ban plus an allowed end state, and a pointer to the project's standards.

These properties are load-bearing:

- **Each outcome has its own exit status.** Passed (0), a rule violated (1), a check that COULD
  NOT be evaluated (3), and everything that ran passing while the project had disabled a rule (4)
  are distinct. An unreachable remote, a ref that does not resolve, or a `file:line` citation with
  no ref to resolve it against is reported, never passed. 0 and 4 are separate because a caller
  reading only the status could otherwise not tell five rules passing from one rule passing;
  `task add` proceeds on 4, so a project's declared disable narrows what is checked without
  breaking its own dispatch.
- **The ref a citation is resolved against is explicit, and its default is useful.** A bare path
  is resolved at the base (`--ref`, defaulting to the brief's declared target). A `file:line`
  citation is about the commit it was read at, which `--citations-ref` names; a gate finding
  legitimately cites a line that exists at the reviewed commit and is past end-of-file on the
  base. Naming that ref makes the answer authoritative, so a miss is a violation. Leaving it
  unset resolves the citation against the base anyway, because citing a line of existing code is
  the most ordinary thing a brief does and must not need a flag, and downgrades a miss there to
  CANNOT-EVALUATE: on the base a miss is ambiguous, and calling it a violation is the very false
  positive the rule exists to avoid.
- **Brief-driven work is bounded.** A brief is untrusted input, and two rules query git once per
  item they find in it, one of them over the network. One run therefore shares a single aggregate
  git deadline (`Options.Budget`, 45s by default, which every call derives from alongside the 20s
  per-call ceiling, the sooner of the two winning), the remote check verifies at most `maxRemoteTargets`
  distinct branches, at most `maxCitations` distinct paths are resolved, line counts are memoized
  per path, and a blob over `maxBlobBytes` is declined rather than read. Everything a bound
  excludes is reported as CANNOT-EVALUATE and named.

  The deadline is enforced rather than declared. `exec.CommandContext` kills the process it
  started and nothing below it, and with a buffer sink `Wait` also waits on the goroutines
  copying the child's pipes, so a forked `ssh` holding the write end keeps the caller blocked
  however long ago the deadline passed. So the lint starts git in its own process group, kills
  the group when the deadline fires, and caps the post-kill wait on those pipes at two seconds
  (`gitCommand`, `internal/brieflint/git.go`). `internal/validate` handles its own checks the
  same way.
- **Per-project configuration that cannot disarm the gate silently.** `- brief-standards:` and
  `- brief-lint-disable:` lines in the repo's `AGENTS.md` (read anywhere in the file, like
  `- auto-mint-max-age:`) set the expected standards pointer and turn individual rules off. Every
  disable is echoed, every summary line states how many rules ran out of how many exist, and a
  run that evaluated none of them is CANNOT-EVALUATE rather than a pass. That configuration
  belongs to the repository under review and any worker can commit it, so it can narrow what is
  checked but it cannot produce a green result from checking nothing. Values read out of that
  file are printed quoted, like every brief-derived quote.
- **Exemptions are earned, not inherited.** A cited path is exempt from the existence check only
  where a create verb governs that mention: the verb must precede the path within a few words of
  it, must not be passive ("is generated by <path>" asserts the path exists), and must not have
  the path as its source rather than its object ("add the case from <path>"). A later reference
  to the same path is checked even if an earlier mention asked for it to be written, and the
  exemption covers existence only, so a cited line is still bounded whenever the file is there.
- **Two rules check a fact; the rest match wording, and say so.** `target-branch` and
  `file-paths` are answered by git. `hard-counts`, `prohibition`, `standards` and the create exemption
  inside `file-paths` are answered by vocabulary, and each carries a false accept that extending the
  vocabulary cannot remove, because the wording that satisfies the rule is the same wording that
  defeats it. A sentence forbidding a recount is built from the words of a hedge. A bound in the
  ban's clause may qualify something else in that clause. A create verb governing a mention may
  be describing a file that already exists. A brief that mentions the standards may be telling
  the worker to ignore them. Each rule stops the bare form it was written for,
  and every note says wording rather than meaning, because a reader who takes "bounded" as a
  finding about the brief has been told something the check never established.

  The scope those wording rules search is deliberately narrow and has been wrong in both
  directions. A hedge is looked for in the block holding the count or the next one; a bound in
  the clause carrying the ban, or in a clause at the edge of the sentence that opens with the
  bound and bans nothing itself. Searching the whole sentence let an unrelated trailing clause
  bound a blanket prohibition once soft-wrapped lines were joined; searching too little pushed
  hedges out of reach of their counts.

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
