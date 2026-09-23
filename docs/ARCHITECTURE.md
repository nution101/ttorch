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

   A re-prep of the same commit supersedes the earlier reviews: they are archived under
   `superseded/`, and a report older than the new episode stamp counts as unreviewed, so stale
   clean reviews can never form a pass. One exception keeps a re-prep from loosening the
   verdict. A report pinned to the commit being prepped that carries a blocking finding (high,
   critical, or a severity the aggregator does not recognise) is carried into the new episode
   and still blocks. To clear one on an unchanged head, the manager moves the report out of
   `reports/` by hand and records why (`ttorch note --task <id> …`), or the worker pushes a new
   commit. The manual move is the same file operation a process running as the lead could
   perform, so it is a procedure, not a control.
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
  covered set is `.ttorch/**`, plus `AGENTS.md` and `CLAUDE.md` **at any depth** (the
  repo-local gate config and the agent instructions, present in every managed repo),
  `.claude/**` and `.mcp.json` (project-level agent config), `go.mod`, `go.sum`,
  `content/**` (every file `content.go` embeds and the installer lays down
  under `~/.claude` — the gate's live reviewer, manager, command and hook
  instructions, not documentation about the gate), `internal/review/**`,
  `internal/approval/**`, `internal/validate/**`, `internal/projectinit/**` (which parses
  `AGENTS.md` into the delivery mode) and
  `internal/orchestrator/{gate,merge,validate,validatecache,audit}.go` (the Go code that
  decides, including the audit record a trusted merge refuses to proceed without),
  `internal/installer/**` and `content.go` (which decide which embedded file becomes which
  installed reviewer definition), `internal/skills/**` (which turns one line into
  `npx skills add <ref>` into `~/.claude/skills` before every worker spawn),
  `internal/worktree/**` (the guard's evidence layer: `ChangedFiles` is its only input,
  and `CatBlobs` is the raw blob read the repository scope is resolved from, so an
  incomplete list or a desynced read there makes the guard match nothing at all),
  `Makefile`, `go.work`, `go.work.sum` and `vendor/**` (which
  between them decide what `make lint` and `make test-fast` actually compile and run), and
  `.github/workflows/**` (the full suite, which `.ttorch/validate.sh` defers to by name
  because it runs lint, the fast lane and the gate's own proofs rather than the whole
  suite), and `docs/install.sh` and `docs/install.ps1` (which
  README tells users to pipe into a shell). On
  a **gated** merge (trusted mode, or any mode with `--require-verdict`) a human approval does
  not wave it through either: it needs `ttorch approve <id> --allow-gate-change`, and the
  merge audit line names every file. The grant is **bound to the gate-definition paths that
  diff touches**, recorded on the token and printed at approve time, and the merge refuses
  any gate-definition path the grant does not name, including one that became covered after
  the approval. Approving for one file does not silently authorize another in the same
  commit. `--allow-gate-change` is refused at approve time for a diff with a blocking hit.
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
rather than by what sounds prudent.

<!-- gate-cost: generated by TestGateCostFiguresMatchTheDoc. Do not edit by hand. -->

Measured over the 196 non-merge commits reachable from `b642ba6`, counting a commit
once if it touches any covered path.

| Covered entry | commits touching it | marginal, given the rest |
|---|---|---|
| `.claude/` | 0 | +0 |
| `.gitattributes` | 0 | +0 |
| `.github/workflows/` | 9 | +4 |
| `.mcp.json` | 0 | +0 |
| `.ttorch/` | 2 | +0 |
| `.ttorch/validate.sh` | 2 | +0 |
| `AGENTS.md` | 9 | +5 |
| `CLAUDE.md` | 1 | +0 |
| `Makefile` | 4 | +0 |
| `content.go` | 2 | +0 |
| `content/` | 46 | +30 |
| `docs/install.ps1` | 4 | +1 |
| `docs/install.sh` | 6 | +1 |
| `go.mod` | 5 | +0 |
| `go.sum` | 3 | +0 |
| `go.work` | 0 | +0 |
| `go.work.sum` | 0 | +0 |
| `internal/approval/` | 6 | +0 |
| `internal/installer/` | 5 | +1 |
| `internal/orchestrator/audit.go` | 1 | +0 |
| `internal/orchestrator/gate.go` | 5 | +2 |
| `internal/orchestrator/gateattacks_test.go` | 0 | +0 |
| `internal/orchestrator/gateconfig_test.go` | 0 | +0 |
| `internal/orchestrator/merge.go` | 6 | +1 |
| `internal/orchestrator/validate.go` | 2 | +0 |
| `internal/orchestrator/validatecache.go` | 1 | +0 |
| `internal/projectinit/` | 9 | +3 |
| `internal/review/` | 7 | +4 |
| `internal/skills/` | 3 | +0 |
| `internal/validate/` | 6 | +2 |
| `internal/worktree/` | 18 | +11 |
| `vendor/` | 0 | +0 |
| **the whole set** | **95** | **48.5% of the corpus** |

Rejected, measured against that same set:

| Rejected | commits touching it | set would become | share |
|---|---|---|---|
| + `cmd/ttorch/` | 1 | 95 | 48.5% |
| + `internal/cli/` | 56 | 123 | 62.8% |
| + `internal/db/` | 23 | 110 | 56.1% |
| + `internal/harness/` | 25 | 105 | 53.6% |
| + `internal/manifest/` | 2 | 95 | 48.5% |
| + `internal/orchestrator/autostart.go` | 2 | 95 | 48.5% |
| + `internal/orchestrator/landqueue.go` | 2 | 97 | 49.5% |
| + `internal/orchestrator/orchestrator.go` | 55 | 120 | 61.2% |
| + `internal/orchestrator/overlap.go` | 4 | 99 | 50.5% |
| + `internal/orchestrator/spawn.go` | 6 | 98 | 50.0% |
| + `internal/scheduler/` | 22 | 112 | 57.1% |
| + `internal/selfupdate/` | 6 | 96 | 49.0% |
| + `internal/orchestrator/**` and `internal/review/**` wholesale | 78 | 134 | 68.4% |

<!-- /gate-cost -->

Two columns because one number cannot carry it. "Commits touching it" is that entry in
isolation; "marginal" is what it adds *given everything else already covered*, which is the
figure that matters when deciding whether to include something. They can differ widely: an
entry whose every commit also touched something else already covered adds nothing at the margin,
and a marginal SHRINKS as the rest of the set grows. An earlier version of this table mixed
two baselines row by row and was not coherent; these are all one definition.

The figures above are generated. The rule is that no other text states or ranks a coverage
cost; it points here instead. The hand-written copies this replaced were found wrong three
rounds running: first the percentages, then a marginal added to a base it was not measured
against, then five marginals in this table at once. `TestGateCostFiguresMatchTheDoc` enforces
less of that rule than the rule asks for, and "Keeping the figures honest" below says exactly
how much.

The last row of the rejected table is the answer to "why not just cover the packages".
`internal/orchestrator/` holds spawn, the land queue, the scheduler wiring and the overlap
planner alongside the five files that resolve, enforce, cache and record the decision. That
row is what covering all of it would do; the rows for the five files above are what they add
instead. The principle for reading it: a flag that fires on most commits is not a signal, it
is a formality, and it launders a real gate change through a habit.

A file list over a package is brittle in a way a directory prefix is not: this package has
already been re-split once (`140d2b91`), and a later split that moved `MergeLocal` into a new
file would drop it out of coverage silently. Two tests hold it, and between them they cover
less than the phrase "any deciding function" suggests:

- `TestGateConfigCoversTheDecidingCode` walks a hand-written list of deciding functions and
  fails if one has MOVED into a file `gateConfigFiles` does not name.
- `TestOrchestratorFilesAreClassified` walks the files instead and fails on a NEW file that
  appears in neither `gateConfigFiles` nor `nonDecidingFiles`, so a file nobody has looked at
  cannot default to uncovered. This is the check `audit.go` needed and did not have.

Neither notices a new deciding function added inside a file already classified as
non-deciding. That stays a review responsibility. An earlier version of this section claimed
the anchor test "fails if any deciding function lands in a file `gateConfigFiles` does not
name", which was not true of what the test does.

### Why `content/` is one prefix, and why it is not derived from the installer

`content/` was two narrower entries for most of this wave: the prefix `content/skills/` and
the *filename* prefix `content/agents/ttorch-reviewer-`. The argument was that the gate
dispatches those reviewers and nothing else in that directory, so the rest of `content/agents/`
was excluded on purpose.

That criterion was wrong. The question is not what the gate dispatches, it is what
`installer.desiredFiles` **installs**, because everything it writes into `~/.claude` is loaded
by the sessions that run and adjudicate the gate. And `desiredFiles` does not work from a
list — it walks the embedded tree with `fs.WalkDir` and has a case per subdirectory. Measured
through the real matcher, the two narrow prefixes covered **7 of the 42 embedded files** and
left 35 open:

| Left open | Installs as | Effect |
|---|---|---|
| `content/hooks/prompt-reminders.sh` | `~/.claude/hooks/` | runs on **every prompt** |
| `content/assets/AGENTS.global.md` | merged into the global `~/.claude/AGENTS.md` managed block | read by every session on the machine, the manager that adjudicates gates included |
| `content/commands/ttorch.md` | `~/.claude/commands/` | the `/ttorch` entry point |
| `content/agents/*.md` × 32 | `~/.claude/agents/` | `ttorch-worker.md` and the 31 curated profiles, installed exactly the way the reviewers are |

This was the third time an enumerated subset of this tree missed installed files, so the fix
is the same inversion already applied to `.ttorch/`: cover the tree. `content/` is a superset
of everything `desiredFiles` can install, and it is a superset *by construction* rather than
by coincidence. `content.go` embeds `all:content` and nothing else, and the embedded FS is
unexported behind an accessor, so there is no exported variable for another package to point
at a different tree. `TestEmbeddedPayloadIsNotAssignable` fails if an exported var comes
back. An earlier version of this paragraph cited
`TestContentPrefixCoversWhatDesiredFilesInstalls`, which had already been deleted, and which
scanned `//go:embed` spellings by hand: `all:cont*` and `"content"` both produce a usable FS
and both got past it. Assignability is the bound worth holding; directive spellings are not.

Deriving the list from `desiredFiles` at runtime was considered and rejected. It would couple
the guard to installer internals in the wrong direction: editing `desiredFiles` would then
change what the guard covers, and `internal/installer/**` is in the covered set precisely
because that mapping is worth attacking. A prefix cannot be narrowed by editing installer
code. `TestEveryInstalledContentFileIsCovered` walks the real `embed.FS` and asserts every
file under `content/` matches, which buys derivation's safety property — a new subtree cannot
be installed without the guard covering it — while leaving the guard's rule independent of the
installer's. That test reports the shortfall against the old prefixes, which is the check the
hand-maintained list never had.

### The installer's FS parameter, and the skills channel

Two bypasses sat one level out from the `content/` tree: the guard covered the tree the
installer reads, but not the code that chose which tree it read, and not a second installer
writing to the same destination.

**`installer.Apply` took an `fs.FS`, and `internal/cli` picked it.** `embedRoot` is the
constant `"content"`, so any FS with a top-level `content` directory installs. The bypass
needed no covered path at all:

| Leg | Covered? |
|---|---|
| `payload/content/agents/ttorch-reviewer-security.md` | no — the prefix is `content/`, not `*/content/` |
| `payload/embed.go` with `//go:embed all:content` | no |
| one line in `internal/cli` handing that FS to `Apply` | no — deliberately uncovered; see the generated table |

That installs a replacement security reviewer, and `collidesInTree` sees nothing because there
is no colliding pair. So the claim "the installer has no file outside `content/` to reach" was
conditional on a file the gate does not cover.

Covering `internal/cli/` would close it; the generated table shows what that would cost. The
fix taken instead removes the choice: `Apply` is now unexported `apply`, and `ApplyEmbedded`
picks `ttorch.Content` inside `internal/installer/`, which the gate already covers.
`internal/cli` no longer imports the payload at all.

Two tests hold the two halves, and they are separate on purpose, because the payload tree and
its delivery are separate legs:

- `TestInstallerExposesNoFSChoice` parses `internal/installer` and fails if any exported
  function takes an `fs.FS` again.
- `TestNoEmbedRootOutsideContent` scans every Go file in the repository and fails on a
  `//go:embed` directive whose root is `content` outside `content.go`. It replaces a version
  that read `content.go` alone and claimed more than it checked. Only a `content` root is a
  hazard: `apply` walks `embedRoot` in whatever FS it is given, so `internal/db`'s
  `//go:embed migrations/*.sql` has nothing at that path and the walk errors rather than
  installing.

It is a filesystem walk, not `git ls-files`, because `.ttorch/validate.sh` runs on a build
host that receives an rsync without `.git`, where git exits 128. `TestGateConfigFilesAreRealPaths`
takes its `t.Skipf` escape there, so it does not run in the lane the gate actually uses; it
still runs in CI, which is the required check. A guard test that skips where the gate runs is
not a guard.

The same reasoning put `skipIfShort` in `gateconfig_test.go`. `TestFSIdentityKeySweep` and
`TestGateCostFiguresMatchTheDoc` call it, and while it lived in `orchestrator_test.go`, which
the gate does not cover, a one-line edit there skipped both in every lane and merged unflagged.

That move does not close the wider gap. Each uncovered `_test.go` file in
`internal/orchestrator` compiles into the same test binary as the gate's proofs, and any of
them can end that binary early: an `init()` that calls `os.Exit(0)`, or a `TestMain` that never
calls `m.Run`, makes the package's `make test-gate` line report ok without running a proof.
`TestMain` and `deliveryHarness`, which the end-to-end attack tests all go through, are two
examples in `orchestrator_test.go`. Covering the package's test files would put e2e setup
behind `--allow-gate-change`; that was left as a decision, not an oversight.

**`internal/skills/` installs third-party code into `~/.claude/skills`.** `Recommended()`
returns refs, `InstallCmd` turns each into `npx skills add <ref>`, and `EnsureInstalled` runs
before every team launch and every worker spawn. One line there fetches and installs arbitrary
third-party skills into the same directory `content/skills/` is covered to protect, by a shorter
route — npx fetches at spawn time, with no ttorch build or install in between. The limits list
used to name `~/.claude/skills` only as an out-of-repo exposure no diff-channel guard could see,
which stopped being true once this route existed.

### Keeping the figures honest

The cost table used to be maintained by hand, and three rounds running a commit updated it and
left a figure stale somewhere else: the *Rejected* table, the prose beneath it, and a "roughly
double" claim in the skill that was the only stated cost reason for leaving `internal/cli/`
uncovered. Transcription was the failure mode each time.

`TestGateCostFiguresMatchTheDoc` now generates the gate-cost block from the live covered-set
lists and the real matcher, and fails if the block in this file differs from what it
generates. That part is complete: the block cannot drift from the guard.

It also looks for hand-written figures, and that part checks much less than the rule above asks
for. Outside the block, in this file, in `content/skills/ttorch-review/SKILL.md` and in the
comments of the covered `.go` files, it fails on text shaped like `<n>/<corpus size>`, `<n> of
<corpus size>`, `<n>%`, `<n> commits` or `<n> marginal` with a single space, or `(<n> commits)`.
The fraction is found across a line break on either side of the slash, and a percentage split
inside the number still trips on its trailing digits; the other three shapes need the number and
its word on one line. It does not catch those three split across a line break, a number with
other words between it and its noun ("costs 0 additional commits"), a number written as a word,
a cost stated as a comparison with no number ("the cheaper of the two"), a fraction over any
other denominator, or anything in a file outside that list. All of those except the
other-denominator case have occurred here and passed the check. They are held by the rule and by
review. A green run means none of the checked shapes is present, not that no figure is.

The whole test skips, rather than fails, when `gateCostBase` is unreachable. That includes CI's
depth-1 clone (`actions/checkout` defaults to depth 1) and any copy of the tree without its git
history, so it only proves anything where the full history is present. Raising `fetch-depth`
would fix CI but changes a covered file; it is a deliberate open item rather than an oversight.

### The covered set is not universal

`gateConfigPrefixes` was a package-level list, and `diffTouchesGateConfig` never asked which
repository it was gating. That is fine for entries every ttorch-managed repo shares, and
wrong for the ones that are ttorch's own source.

`content/` is the clearest case. Here it is the embedded payload the installer lays into
`~/.claude`, so covering it is right. It is also the conventional content directory for Hugo,
Next and most CMS layouts, so the unscoped rule demanded `--allow-gate-change` for every
ordinary change in a user's site repo. `internal/review/` and `internal/validate/` are the
same error more quietly: they are ordinary names for ordinary Go packages.

The set is now two tiers.

| Tier | Entries | Applies |
|---|---|---|
| universal | `.ttorch/**`, `AGENTS.md`, `CLAUDE.md` (and both by basename), `.claude/**`, `.mcp.json`, `Makefile`, `go.mod`, `go.sum`, `go.work`, `go.work.sum`, `vendor/**`, `.github/workflows/**` | every ttorch-gated repo |
| ttorch source | `content/**`, `content.go`, `docs/install.sh`, `docs/install.ps1`, `internal/{review,approval,validate,projectinit,installer,skills,worktree}/**`, the five deciding `internal/orchestrator/*.go`, the two proof test files | only ttorch's own repository |

`resolveGateScope` decides which applies, from the repository rather than from its name or
remote URL. The signal is a Go `//go:embed` directive rooted at `content`, which is the
property that makes `content/` load-bearing in the first place: `installer.apply` walks
`embedRoot` ("content") in the embedded FS, so a repo that embeds `content/` as a Go payload
is a repo where `content/` is what gets installed. A site repo with articles in `content/`
has no such directive.

**Why a worker's diff cannot spoof it.** The directive is read from BASE, the default-branch
tree, and never from `rev`. That is the rule `resolveGateDefinition` already applies to the
gate script, for the same reason. A worker cannot narrow the scope: deleting `content.go` on
its branch does not change what base says, so `content/` stays covered for that merge.
Reading `rev` would have been the hole, since one commit could drop the directive and
un-cover `content/` while rewriting it.

Detection fails **closed**: a git error returns `TtorchSource: true`, because the expensive
mistake is leaving ttorch's own deciding code uncovered. Scoping in wrongly costs one
unnecessary `--allow-gate-change`, the same over-match the case fold already accepts.
`TestTtorchRepoIsScopedIn` resolves the scope against this very tree, so the cheap direction
cannot regress silently, and
`TestGateScope_ContentOnlyGatesTheRepoThatEmbedsIt` builds both fixture repos and asserts
opposite verdicts on the same path.

This changes no figure in the cost table: ttorch's own repo is scoped in, so the covered set
measured over this history is what it was.

### The input set is the part that keeps being wrong

Five separate bypasses of this guard have now been defects in the **list of paths handed to the
matcher**, not in the matcher:

| defect | the set was short by |
|---|---|
| no case folding at all | every case variant |
| `strings.ToLower` instead of folding | U+017F and friends |
| `unicode.SimpleFold` instead of full folding | the multi-rune folds (`ﬁ` → `fi`) |
| blobs only, from `ls-tree -r` | the directories those blobs imply |
| rename detection on, the git default | a rename's **source** path |

Each time the comparison did exactly what it said and the set was incomplete. So the standing
instruction, recorded beside `worktree.ChangedFiles`: check what reaches the guard before
checking what it does with what reaches it. The three questions that have each caught a real
bypass are whether every path is present, whether it is spelled the way the matcher matches,
and whether it is the committed tree rather than the working one.

The rename case is the newest. `diff.renames` has defaulted **true** since git 2.9, so a
detected rename is reported as its destination only and the source never reaches the matcher.
That made four moves merge unflagged:

```
content/agents/ttorch-reviewer-security.md -> content/agents/security-review-guidance.md
internal/installer/installer.go            -> internal/installerx/installer.go
.ttorch/validate.sh                        -> scripts/validate.sh
AGENTS.md                                  -> docs/notes-about-agents.txt
```

The first two are the live ones. Renaming the security reviewer's definition needs no code
change at all, because `installer.desiredFiles` walks the embedded tree rather than working
from a list, so the file simply leaves the install set. And `internal/installer` has exactly
one non-test importer, `internal/cli/cli.go`, which is deliberately uncovered — so a diff
could move the whole package out of coverage, fix that one import, compile, merge unflagged,
and every later change to the package would be unflagged too. The collision check cannot
backstop either: a rename produces no colliding pair, because the source is gone from the tree.

`--no-renames` reports both sides, and the guard then trips on the source. It is also needed in
`orchestrator.diffFiles`, which feeds the reviewer-set classifier off the same command, and
there the consequence is sharper: moving a `.go` file to a `.md` one reported only the `.md`
destination, so `review.Classify` read the diff as **docs-only** and dropped the security
reviewer entirely.

Widening the set could have made every ordinary file move a flagged change, so
`TestGateGuard_OrdinaryRenameStillMerges` pins that a rename whose source and destination are
both uncovered still auto-approves and merges clean.

### Why name-matching is not the load-bearing control

Both the matcher and the collision check compare under one function, `fsIdentityKey`: NFD,
Unicode **full** case folding, then SimpleFold **orbit-minimum**. Three weaker versions
shipped first, and each was exploitable:

| version | missed | attack |
|---|---|---|
| `strings.ToLower` | simple lowercasing | U+017F `ſ` folds with `s` but lowercases to itself: `agentſ.md` overwrites `AGENTS.md`, which sets delivery-mode |
| `unicode.SimpleFold` | multi-rune FULL folds | U+FB01 `ﬁ` folds to `fi`: `Makeﬁle` substitutes the `Makefile` that `.ttorch/validate.sh` executes |
| NFD + fold + NFD | the 172 Cherokee runes | `cases.Fold` is an **involution** there — `fold(U+13A0)=U+AB70` and `fold(U+AB70)=U+13A0` — so the two spellings swap and never meet |

The second is worse than the first. `Makefile` is in the covered set *because* the validate
script does nothing but run `make lint`, `make test-fast` and `make test-gate`, so an attacker
who substitutes it substitutes what the gate runs, and the gate approves the substitution.
U+FB06 `ﬆ` → `st` reaches `internal/inﬆaller/` and `internal/orcheﬆrator/gate.go` the same way,
and U+00DF `ß` → `ss` reaches any covered path with a double s.

Folding correctly fixes those spellings. It does not fix the class, because the attacker picks
the spelling and the guard would otherwise have to anticipate every collapse rule of every
filesystem. So the guard also refuses **any newly introduced tree entry that collides with a
different entry in the same tree**, whether or not either one is in the covered set. An
earlier version keyed that on the same relation the matcher used, which made the backstop
blind to exactly what the matcher was blind to while its comment claimed the opposite.

"Entry" means blob **or directory**, and the directories are load-bearing. `git ls-tree -r`
lists blobs; a directory is never an entry, so an earlier version could not see this:

```
tree:     .github/workflows/ci.yml   .github/workflowſ   main.go
guard:    nil — matches no covered path, and no other BLOB
checkout: .github/workflowſ is a 7-byte file; .github/workflows/ is GONE
```

The whole covered directory vanishes from the bytes the gate validates, git warns about
nothing, and since a missing `.github/workflows/` breaks no Go build the validate then goes
green against a checkout that is not the tree — the exact outcome that running the guard
before the validate exists to prevent. `content/`, `.claude/` and
`internal/installer/` go the same way. The hostile spelling has to sort after the directory's
own entries to win the checkout, which plain ASCII cannot do (`/` sorts below every letter)
but a non-ASCII spelling does for free. The keyed set is therefore every blob plus every
ancestor directory those blobs imply.

Two limits, stated because an earlier version of this paragraph overstated it. It claimed
"the attack always needs two entries landing on one file, and that is observable regardless of
what either is called". The second half was false while the check was blind to directories,
and the general claim is still too strong: what the check detects is two entries in ONE tree
resolving to one path, and a substitution that never produces two entries is invisible to it.

A collision, and a control character in a path, are **blocking**: no approval clears them,
`--allow-gate-change` included. A gate change is a legitimate act that needs naming; a diff
whose checkout is not well-defined is not a change to anything, and offering a flag for it
would turn the attack into a formality a lead can wave through.

The ordering was settled by measurement, not by reading the tables. Every printable rune was
turned into pairs with its lower/upper/title/SimpleFold-orbit/NFC/NFD/fold images, and each
pair was created on real APFS and read back:

| pipeline | misses | false positives |
|---|---|---|
| **NFD, fold, orbit** (shipped) | **0** | **0** |
| NFD, fold, NFD | 172 (all Cherokee) | 0 |
| fold, NFD, fold | 172 (the involution cancels) | 0 |
| orbit, NFD, fold, NFD | 33 | 0 |
| SimpleFold orbit alone | 13,328 | 0 |

16,304 pairs. The orbit pass must run **last**: it is what settles an involution, by walking
to the orbit's lowest code point so both endpoints land on one key. Folding twice does not
help, because `fold(fold(x)) == x` is the problem itself.

There is deliberately **no trailing NFD**. An earlier version had one, justified as insurance
against folding producing characters that themselves decompose — which is not true of the fold
output. Once the orbit pass exists the pass stops being a no-op and becomes wrong, on exactly
two unordered pairs:

| shipped key separates | a trailing NFD merges |
|---|---|
| `U+1F9C` ᾜ | `U+1F28 U+038A` ἨΊ |
| `U+1F94` ᾔ | `U+1F28 U+038A` ἨΊ |

APFS keeps both pairs apart, so merging them is a false positive. The effect needs **two**
runes, which is why an exhaustive single-rune search finds nothing — a reviewer ran exactly
that search and could not reproduce the claim, and both results are correct.
`TestTrailingNFDWouldBeAFalsePositive` now holds both halves: the single-rune search returning
zero, and these pairs checked against the real filesystem. This comment has twice carried a
number that could not be reproduced; the number is now executable.

`TestFSIdentityKeySweep` keeps a bounded version of that sweep in the suite (~2,400 pairs,
including the whole Cherokee block) and `TestFSIdentityKeyMatchesTheFilesystem` keeps the hand
table. Three rounds of this work were lost to reasoning about Unicode instead of asking the
machine, so the tests ask the machine.

The full folding and normalization come from `golang.org/x/text` (`cases.Fold`, `unicode/norm`),
a new direct dependency. It is a real cost — a `golang.org/x` module in the gate's own path,
pinned at v0.41.0 because v0.42.0 raises the Go directive to 1.26. The alternative was
hand-rolling the CaseFolding full mappings, which is a bounded table, plus NFD, which is not;
and a hand-rolled Unicode table in a security control going quietly stale against a new
Unicode revision is the exact failure this section already documents twice.

The comparison is over the whole tree rather than the changed list, because in the attack only
the new entry is in the diff — `AGENTS.md` is untouched, so comparing changed paths against
each other sees one path and nothing to pair it with. Only entries **absent from the base
tree** are reported: a repository that already contains a colliding pair would otherwise be
unmergeable forever, with no flag to clear it and no way to land the rename that fixes it. So
the guard fires on the diff that INTRODUCES the collision, which is both the attack and the
thing a human can act on. That is also the answer to the false-positive worry on a
case-sensitive filesystem, where `AUTHORS` and `authors` really are two files: an existing
pair never blocks, and a diff that newly introduces one is producing a tree that cannot be
checked out correctly on macOS or Windows at all. There is deliberately no `runtime.GOOS`
gate.

Git will not do this for you. `git clone` prints "the following paths have collided";
`git worktree add --detach`, which is what the gate uses to materialize the checkout it
validates, exits 0 with nothing on stderr and silently drops the loser (pinned in
`worktree.TestAddDetached_DoesNotWarnOnCollision`, which reads the attacker's bytes back out
of `AGENTS.md` in that checkout). `worktree.gitRaw` also used to discard stderr on success,
throwing away even the warning git does give; it now returns stderr, and `checkedGitRaw` turns
a collision warning into `ErrPathCollision`. The gate-config guard runs **before** the
validate for the same reason: a colliding tree validates the wrong bytes and would cache that
result green under the real tree's hash.

Two further channels were opened by earlier rounds of this work and are closed at their sinks.
Reading the diff with `-z` removed git's C-quoting, which had been the only thing keeping a raw
newline out of a path name; a covered path containing a newline would write a second,
well-formed, fabricated merge record into the newline-delimited audit log. The guard refuses
control characters in a changed path, and `sanitizeAuditLine` escapes them in `writeAudit` so
every call site is covered. Separately, surfacing git's stderr put attacker-controlled bytes on
the lead's terminal — a committed `.gitattributes` with an invalid attribute name makes git
echo it back, ANSI escapes included, while exiting 0 — so `worktree.warnf` escapes control
bytes before printing, and the escaped form is what goes into `ErrPathCollision` too.

Both sinks were chosen over per-call-site escaping for the same reason: every caller shares the
exposure and no caller can be relied on to remember. `worktree.git`, the most-used wrapper in
that file, escapes its error text on the same argument — no worker-controlled route into a
`git()` argument is known, so what that closes is the asymmetry rather than a demonstrated
exploit. All three escapers cover C1 (U+0080–U+009F) as well as C0 and DEL, because U+009B is
CSI, the single-character form of `ESC [` that several terminals accept, and U+0085 NEL is a
line break in its own right.

An earlier `TestGateConfigPathsAreASCII` claimed the covered set was safe because "ASCII has no
decomposed form and no multi-character fold". That is true in one direction only — an ASCII
path has no non-ASCII spelling of its own, but it is *reachable by* one, which is the whole
`Makeﬁle` attack. The test passed while the property it named did not hold. It is replaced by
the filesystem-measured test above.

`Makefile`, `go.work`, `go.work.sum`, `vendor/**`, `content.go` and
`internal/orchestrator/audit.go` each decide the gate; the generated table has what each costs.
`.ttorch/validate.sh` is covered but does nothing except run `make lint`, `make test-fast` and
`make test-gate`, so redefining those targets redefines green without touching a covered script.
`content.go` is the `//go:embed` that maps a repo file onto an installed reviewer definition,
and `internal/installer/**` is the mapping itself: covering the content while leaving the
mapping open would let a file from an uncovered subtree be installed as a reviewer instead.
`audit.go` holds the only `writeAudit`, and `MergeLocal` refuses to merge when it fails, so one
line there strips the record from every trusted merge.

`.ttorch/**` is a **prefix**, not the single `.ttorch/validate.sh` it started as, and the
inversion closes a class rather than a file. What forced it: `.ttorch/learnings.jsonl` is the
per-repo lessons ledger, `learnings.Apply` renders it into `AGENTS.md` between markers,
`Promoted` admits any entry that is pinned or has been seen twice, `Render` emits
`"- " + e.Text` verbatim up to 20 entries, and the ttorch-manager skill has the manager run
`ttorch learn` at **every delivery**. So a committed ledger transplants attacker-chosen text
into `AGENTS.md` — the one file this guard treats as the gate's own configuration — on the
merge alone, with no build and no install, and the ledger is not gitignored.

Adding `learnings.jsonl` as a second exact path would have left the *next* `.ttorch/` file in
exactly the same position. This is the third time an enumerated subset has been the bug:
`content/` missed installed files twice, then `.ttorch/` missed the ledger. Covering the tree
covers what is not there yet. `.ttorch/task`, the one file besides `validate.sh` that
appears locally, is gitignored so it cannot reach a diff, and that had to be *fixed* here:
it was excluded only through `.git/info/exclude`, which is local to a clone and does not
travel. The manager writes that file into every worker worktree, so in a fresh clone a
worker's `git add -A` would have staged it and this prefix would have refused the merge on
every task. `TestTtorchRuntimeFileIsIgnored` holds the `.gitignore` line so the false
positive cannot come back quietly.

`docs/install.sh` and `docs/install.ps1` are in on a **different argument** from everything
else, named as such so the flag keeps one meaning. They do not decide how a change is reviewed
or validated. README publishes them by raw URL — `curl … /main/docs/install.sh | sh` and
`irm … /main/docs/install.ps1 | iex` — so a merge changes bytes users pipe straight into a
shell, with no build, no install and no release step in between. That is the same delayed
out-of-band effect that justifies the rest of the set, carrying the most severe consequence of
any file here, and covering one platform's installer but not the other would be an obvious
gap.

So the covered set answers **two** questions, not one: what decides how a change is reviewed
or validated, and what a merge publishes directly to users. Anything outside both stays out
however alarming it looks — that is what keeps the flag legible.

`.claude/**` and `.mcp.json` are the strongest form of the `content/` argument rather
than a weaker one. Claude Code loads **project-level** `.claude/agents/`, and project-level
takes documented precedence over `~/.claude/agents/` on a name collision, so a landed
`.claude/agents/ttorch-reviewer-security.md` replaces the security reviewer for every later
gate run in that repo. `content/agents/ttorch-reviewer-*.md` is already covered for the same
effect, but that route needs a build and an install first; this one takes effect on the merge.
`.mcp.json` adds tools to those same sessions. Neither exists in this repo. `.claude/` is not
in `.gitignore` here either, so nothing but this guard stands between a committed reviewer
override and a merge.

`go.mod` and `go.sum` are covered on the argument already carrying `go.work` and `vendor/`, and
this branch sharpened it against itself: `fsIdentityKey` now depends on `golang.org/x/text`, so
a `replace` on `x/text` redirects the guard's **own** Unicode folding. Unlike `go.work` these
actually exist and are authoritative. Step 6's not-covered list still names them; that line goes
when the two land together.

`AGENTS.md` and `CLAUDE.md` are matched by FILENAME at any depth, not only at the root. A
nested one loads on demand when a session reads files in its directory, which is the root
file's rationale one level down. Matching by name rather than enumerating the two paths that
could exist today is deliberate: the set of directories is open and a list would go stale the
first time someone adds a package. One consequence, recorded because it reverses an earlier
call: `docs/AGENTS.md` was pinned in the match table as a near-miss that must stay OUT, and is
now correctly IN — a session reading files under `docs/` loads it, so it is an instruction file
like any other.

`CLAUDE.md` is covered at the root for a second, separate reason: the guard matches a symlink
by its OWN path, never by what it
resolves to. In a managed repo `CLAUDE.md` is a symlink whose blob is the string `AGENTS.md`,
so a commit that deletes the link and writes a real `CLAUDE.md` reports the changed path
`CLAUDE.md` — which is not `AGENTS.md`, and matched nothing. It is the agent-instruction file
every Claude Code session in the repo loads, the manager session that adjudicates gates
included, which is the rationale that already covers all of `content/`; unlike `content/` it
needs no rebuild and no install, so it takes effect on the merge. `projectinit.ensureSymlink`
also refuses to clobber a real file, so `ttorch init` never undoes the swap.

The collision check does **not** back that up. The swap changes the blob and the file mode but
not the path, so the tree gains no entry and there is no pair to collide;
`TestGateGuard_SymlinkSwapNeedsAllowGateChange` asserts that the collision check stays silent,
so the covered-set entry is known to be the only thing standing rather than assumed to be
belt-and-braces. `CLAUDE.md` is the only symlink in this repo, and
`TestTreeHasNoUncoveredSymlinks` fails if another appears outside the covered set.

`go.work` and `vendor/` are covered although neither exists in this repo, and that is the
reason rather than an exception to it. `.ttorch/validate.sh` runs `make lint` and
`make test-fast`, which run `go test`, and three committed files change what `go test`
compiles without touching the script or the Makefile: the toolchain auto-discovers a `go.work`
at the repo root via `GOWORK` and its `replace` directives override `go.mod`; `go.work.sum` is
inert alone but covered so the pair cannot drift; and a consistent `vendor/` makes the
toolchain build from it instead of the module cache. Both mechanisms were verified by
building against a substituted dependency while `go.mod` and the real source stayed untouched.
Since none of them exists here, their appearance in a diff is exactly the event worth seeing,
and `absentByDesign` in the test file records that so the dead-coverage check does not demand
they exist.

Two packages that touch the decision are left out on cost, and both are recorded here and in
the skill so they read as decisions rather than omissions. What covering each would cost is
in the rejected table; this section does not restate or rank it.

`internal/db/` holds `Store.GetVerdict`, the row the merge trusts for `Overall == pass`.

`internal/cli/` wires the `--allow-gate-change` flag. It was also the caller that chose the
tree `installer.Apply` walked, which made the `content/` superset argument conditional on an
uncovered file; that is closed by `installer.ApplyEmbedded` instead. See "The installer's FS
parameter" below.

`.github/workflows/**` is in for a different reason. The trusted gate never consults CI, which
is the argument against it — but this repo's `.ttorch/validate.sh` runs `make lint`,
`make test-fast` and `make test-gate`, not the full suite, and says in its own header that the
full suite, including the orchestrator e2e tests, runs in CI. The gate runs the default
branch's copy of that script, never the branch under review's, so a worker cannot weaken its
own gate. The consequence is that until this branch lands, the gate still runs only `make lint`
and `make test-fast`, and the `make test-gate` step takes effect on the first merge after it. CI is therefore half of what "validated" means here, and weakening
`ci.yml` weakens every later change's validation through the same delayed diff channel that
put the skills on the list.

What the numbers do not fix: the flag takes no paths. A lead who passes it by reflex grants
every gate-definition path in the diff, the same set as one who read it. What the binding adds
is that approve prints that set and records it on the token, and the merge refuses any path
outside it. The audit line naming the files survives either way, which is the guard's durable
half.


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
- **The stall ladder** keeps reporting a worker that has gone quiet. While a worker's task is
  `active` and its pane sits at an unchanging, non-busy prompt, `watch` raises an actionable
  `stalled` update after `TTORCH_STALL_AFTER` (default 10m), repeats it every
  `TTORCH_STALL_REPEAT` (default 15m), and after `TTORCH_STALL_RERAISES` re-raises (default
  3) marks each further update `needs-inspection`. A pane change, a report or stage, a status
  change or re-dispatch, or a new commit in the worktree restarts the clock. The clock and
  the ladder are events in the store, so they carry across watcher re-arms. The ladder only
  reports: lease expiry and recovery are unchanged. `TTORCH_STALL_AFTER=0` turns it off.

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
