# Design spec — SQLite event-driven orchestration

**Status:** proposed (build-ready)
**Scope:** replace ttorch's JSON task records with a global SQLite database as the single
source of truth, and replace the supervisor's tmux `send-keys` "poke" waking with an
event-driven, manager-owned background watcher.
**Audience:** an engineer implementing this without further design input.

Every "change X" claim cites the current code by `file:line`. All citations are against the
tree at the time of writing (branch `ttorch/spec-sqlite-events`).

**Section map (single monotonic scheme):** `§0` summary · `§A` current architecture ·
`§B` hard Claude Code limits · `§C` target overview · then the brief's nine required
deliverables as `§1`–`§9`. Cross-references use these labels (e.g. `§A.5`, `§B.1`, `§2.3`).

---

## 0. Summary, goals, non-goals

### 0.1 What changes

1. **State store.** Today every task is a JSON file at `~/.ttorch/state/<id>.meta.json` and
   the manager record is `~/.ttorch/state/manager.json`, read/written by `internal/state`
   (`state.go:48` `Store struct{ Dir string }`; `state.go:61` `Save`; `state.go:118` `List`).
   This is replaced by a single global SQLite DB at `~/.ttorch/state.db`, the **source of
   truth** for all orchestration state, with a hierarchy of
   **projects → epics → phases → tasks**, an append-only **events/audit** table, and a
   **notes/activity** table.

2. **Waking.** Today a long-lived `internal/supervisor` daemon polls panes and, on an
   actionable change, **types a directive into the manager's tmux window** via
   `tmux.SendLine(...)` (`supervisor.go:108`, `supervisor.go:361` `pokeDirective`). That
   keystroke injection **into the manager window** is **removed entirely**. The manager
   instead arms a new blocking command, **`ttorch watch`**, as a *background task* each turn;
   when an actionable DB transition occurs, `watch` prints the coalesced batch and exits, and
   the harness re-invokes the manager through the **safe same-session background-task-completion
   channel** (not keystrokes). The manager re-arms after acting.

3. **Awaiting-lead is silent.** When the manager surfaces a decision/blocker to the lead it
   **cancels any in-flight watcher and does not arm a new one**; the window simply waits for
   the lead. The manager is never pulled off a pending decision. Even an urgent worker error
   is recorded in the DB and only surfaces when the lead returns and the manager re-arms.

4. **Workers report reliably.** New worker-facing CLI — `ttorch report|stage|note|follow-on`
   — writes transitions to the DB. The worker contract
   (`content/agents/ttorch-worker.md`) makes reporting mandatory.

### 0.2 Goals

- One durable, queryable, transactional store; restart-proof and crash-proof (the property
  `internal/state` already claims, `state.go:1`).
- **Zero keystroke injection into the manager session.** (`ttorch send` — manager→worker
  steering — intentionally survives; see §B.1. The narrow, enforceable invariant is: *no
  `tmux.SendLine`/`SendKey` ever targets the `"manager"` window*, §5.)
- A hierarchy that lets the manager model real work (epics/phases) and lets workers file
  follow-on/child tasks.
- A complete audit trail (events) for finance-grade reconstruction, superseding the flat
  `audit.log` (`paths.go:110`) while keeping it as defense-in-depth for trusted merges.

### 0.3 Non-goals / explicit constraints

- **The delivery-mode gate keeps reading `AGENTS.md`.** `projectinit.ReadMode(repo)` is the
  authority for the security-sensitive delivery mode (`orchestrator.go:974`, `:1282`,
  `:856`). The DB may *cache* a project's mode for display (and must be populated to be
  honest — §2.2/§3.4), but the merge/land gates continue to resolve mode from `AGENTS.md`,
  never from the DB. Moving the gate's mode source into a worker-writable store would be a
  security regression.
- **Footprint-overlap logic is unchanged.** The pure functions in `internal/state`
  (`footprint.go:33` `PathsOverlap`, `:53` `FootprintOverlap`) keep their behavior and tests.
  The *callers* of those helpers live in `internal/orchestrator/overlap.go` (not in
  `internal/state`); only persistence and the task-slice type they iterate move (§2.4).
- **No cgo, no network dependency.** The binary stays a single statically-linked,
  cross-compiled artifact (§A.5). Adding the SQLite driver introduces a transitive pure-Go
  module set (`modernc.org/{libc,memory,mathutil}`, `remyoudompheng/bigfft`,
  `dustin/go-humanize`, `google/uuid`, `mattn/go-isatty`, `ncruces/go-strftime`,
  `golang.org/x/sys`) — `go.sum` grows and the binary grows somewhat, but it remains cgo-free
  and `make dist` is unaffected (verified: a `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` build of
  the driver succeeds).

---

## A. Current architecture (what we are replacing), grounded

### A.1 The JSON state store — `internal/state`

- `Store struct{ Dir string }` (`state.go:48`); callers build `state.Store{Dir: p.StateDir()}`
  at exactly two sites: `orchestrator.go:49` and `supervisor.go:92`.
- One file per task: `<Dir>/<id>.meta.json` (`state.go:50` `suffix=".meta.json"`,
  `state.go:56` `path`). Manager singleton: `<Dir>/manager.json` (`state.go:54`, `:58`),
  never returned by `List`.
- Atomic writes via temp-then-rename (`state.go:141-163` `writeJSON`): the durability
  guarantee SQLite replaces with transactions.
- API: `Save` (`:61`), `Load` (`:66`), `Remove` (`:77`, idempotent), `SaveManager` (`:86`),
  `LoadManager` (`:92`, returns `(zero,false,nil)` when absent), `RemoveManager` (`:108`),
  `List` (`:118-138`, oldest-first by `Created`, silently drops unloadable files).
- **The durable record** (`state.go:15`):

  ```go
  type Task struct {
      ID        string    `json:"id"`
      Window    string    `json:"window"`
      Worktree  string    `json:"worktree"`
      Project   string    `json:"project"`   // repo root path
      Harness   string    `json:"harness"`
      Kind      string    `json:"kind"`      // ship | scout | cc
      Created   time.Time `json:"created"`
      PR        string    `json:"pr,omitempty"`
      SessionID string    `json:"sessionId,omitempty"`
      GatePassed  bool   `json:"gatePassed,omitempty"`
      ApprovedBy  string `json:"approvedBy,omitempty"`  // human | auto
      ReviewedSHA string `json:"reviewedSha,omitempty"`
      Footprint []string `json:"footprint,omitempty"`
  }
  type Manager struct { Dir string `json:"dir"`; SessionID string `json:"sessionId"` }
  ```

- **There is no persisted lifecycle status.** State is *derived at runtime from tmux*:
  `DeriveState(live bool, pane string)` returns `"gone" | "working" | "idle"`
  (`orchestrator.go:352-360`); `TaskState` wraps it with a live pane capture
  (`orchestrator.go:364-370`). The only persisted enum-like field is `Kind`
  (`"ship" | "scout" | "cc"`, declared in the comment at `state.go:21`, produced at
  `orchestrator.go:149/151/693`, promoted at `:1577`). This is the central gap the DB fills:
  **the new store persists lifecycle status, so a restarted manager and a blocking watcher
  do not have to scrape panes to know a worker is done/blocked.**

- **Task IDs are caller-chosen free-form strings** used directly as the filename stem, the
  tmux window (`"wk-"+taskID`, `orchestrator.go:145`), the branch (`taskBranch="ttorch/"+id`,
  `orchestrator.go:246`), and the brief path (`paths.go:70`). The DB must keep `tasks.id` as a
  caller-supplied `TEXT` primary key — it cannot become a surrogate integer without breaking
  these couplings. The one auto-generated id is `cc-HHMMSS` (`orchestrator.go:669`), which is
  collision-prone within a second — under a `TEXT PRIMARY KEY` this becomes a hard
  INSERT-collision, so §3.4/§8 require widening it.

### A.2 The wake/poke mechanism — `internal/supervisor` + `internal/wake`

The supervisor is a long-lived daemon (`supervisor.go:1-14` package doc) that:

- **Polls** on a ticker (`Cfg.Poll=5s`, `supervisor.go:47`) and on `fsnotify` writes to the
  state dir (`supervisor.go:157-189`).
- `scanSignals` (`supervisor.go:231-254`): turns new `<id>.turn-ended` / `<id>.status` file
  writes (touched by the worker's Stop hook, §A.4) into `signal` wakes, and on a
  `.turn-ended` calls `requestPoke()` (`:251`).
- `scanStale` (`supervisor.go:266-293`): captures each worker pane; when a pane stops changing
  and shows no busy indicator for two sweeps, appends a `stale` wake and `requestPoke()`
  (`:286`).
- `scanChecks` (`supervisor.go:204-227`): rate-limited `gh pr view` poll; on `MERGED` appends a
  `check` wake and `requestPoke()` (`:224`).
- `scanLabels` (`supervisor.go:316-338`): cosmetic tmux tab glyphs (🔵/🟡).
- `heartbeat` (`supervisor.go:346-351`): silent liveness beacon; **never** pokes.
- **The injection itself:** `requestPoke` (`:373-379`) → `flushPoke` (`:387-410`) →
  `s.sendPoke()`, wired at `supervisor.go:108` to
  `tmux.SendLine(s.Session, managerWindow, pokeDirective)`. `managerWindow="manager"`
  (`:355`); `pokeDirective` is a one-line instruction typed into the manager window
  (`:361`). `inspectManager` (`:109-120`) reads the manager pane to debounce/avoid
  interrupting. **This `tmux.SendLine` to the manager window is the keystroke injection being
  deleted.**
- Durability lives in the **wake-queue** (`internal/wake`): an append-only file
  (`paths.WakeQueue()`, `paths.go:80`) the supervisor appends (`wake.go:47` `Append`) and the
  manager drains (`wake.go:62` `Drain`, dedup by `kind+key`, heartbeats collapse). Consumed by
  `ttorch wake drain` (`cli.go:711`) and the blocking `ttorch wait` (`cli.go:744-808`).

The injection primitive is `tmux.SendLine` (`tmux.go:155-167`: `send-keys -t … -l text`, settle,
then `send-keys -t … Enter`); `tmux.SendKey` (`tmux.go:170-173`) sends a single named key. These
are the only ways anything types into a session. The supervisor also uses the proven flock
singleton (`supervisor.go:441-499 acquire`, `lockedLiveFile` — flock-as-truth, *not* the pid file
contents, precisely to dodge pid reuse / unlink-vs-lock races) — the watcher reuses this pattern.

### A.3 The command engine — `internal/orchestrator` and dispatch — `internal/cli`

- `Manager struct{ P paths.Paths; Session string; Store state.Store; Pool worktree.Pool }`
  (`orchestrator.go:37-42`), built by `New(p)` (`:45-52`, **returns `*Manager` with no
  error**). `Store` is the JSON store.
- Public methods the spec touches (all in `orchestrator.go`): `Spawn`/`SpawnWithFootprint`
  (`:110`/`:127`), `Status` (`:341`, just `Store.List()`), `TaskState`/`DeriveState`
  (`:364`/`:352`), `Live` (`:336`, takes `state.Task`), `Peek` (`:373`), `Send`
  (`:384`, `tmux.SendLine` into the **worker** window, `:392`), `Teardown` (`:397`,
  **hard-deletes** the record via `Store.Remove`, `:418`), `ReviewDiff` (`:701`), `Validate`
  (`:712`), `Approve` (`:726`), `TrustPrep` (`:759`), `TrustRecord` (`:819`), `TrustShow`
  (`:889`), `SecurityReview` (`:916`), `MergeLocal` (`:965`), `Land` (`:1265`), `Promote`
  (`:1569`), `ArmPRCheck` (`:1582`, sets `t.PR`), `FleetSync` (`:1593`), `Recovery` (`:1628`),
  `StartManager`/`restore` (`:481`/`:542`, `restore` skips `Kind=="cc"` at `:545-547`),
  `Reset` (`:615`), `StopSession` (`:640`), `OpenCC` (`:664`).
- Delivery audit: `audit`/`writeAudit` (`:1654-1677`) append to `paths.AuditLog()`
  (`paths.go:110`); trusted merges call `writeAudit` and **abort if it fails** (`:1105`).
- **Overlap detection** lives in `internal/orchestrator/overlap.go`: `computeConflicts`
  (`overlap.go:26`, takes `[]state.Task`), `footprintCandidate` (`:46`, takes `state.Task`),
  `liveFootprintTasks` (`:56`, calls `m.Store.List()` at `:57` and `m.Live(t)` at `:60`),
  `CheckOverlap` (`:70`). It *imports* the pure helpers from `internal/state` (`footprint.go`).
- **Command dispatch** is a single `switch` in `cli.Main` (`cli.go:52-145`). Adding a
  subcommand = add a `case` here + a `cmdXxx` function + a usage block line
  (`cli.go:1180-1258`). Flags use the stdlib `flag` package per-command
  (e.g. `cli.go:315`). `mgr()` (`cli.go:306`) is `func mgr() *orchestrator.Manager { return
  orchestrator.New(paths.Default()) }` — a **fresh `Manager` per call**, called ~22 times
  across the switch; `internal/orchestrator` is imported only by `internal/cli`.
  `run(err)` (`cli.go:147`) is the shared error-to-exit-code path.

### A.4 The worker turn-end hook and charter — `internal/harness`

- `InstallTurnEndHook` (`harness.go:235-270`) writes a worktree-local
  `.claude/settings.local.json` with a `Stop` hook of `touch <markerPath>` and
  `IncludeCoAuthoredBy:false`, then git-excludes it (`:269`, `excludeInWorktree`). The marker
  is `paths.TurnEndMarker(id)` (`paths.go:87`) — the `<id>.turn-ended` file `scanSignals`
  watches. Spawn installs it at `orchestrator.go:189`.
- `BriefCommand` (`harness.go:161-170`) launches `claude … --session-id <sid> "$(cat brief)"`.
  The brief path is `paths.BriefPath(id)` (`paths.go:70`); a stub is written when absent
  (`orchestrator.go:199-201`, `writeBriefStub` `:1693`).
- **Manager charter** (`harness.go:93`, `const managerCharter`): a single long line appended to
  the manager's system prompt, written to `paths.ManagerCharterFile()` (`paths.go:55`) by
  `WriteManagerCharter` (`harness.go:135`) and passed via `--append-system-prompt-file`
  (`harness.go:125-130` `managerCharterArg`, used by `ManagerCommand` `:112` and
  `ManagerResumeCommand` `:144`). Rule (5) of the charter literally says *"the supervisor pokes
  you on actionable wakes"* — this text must change (§6).

### A.5 Build & dependency constraints (decisive for driver choice)

- `go.mod`: module `github.com/nution101/ttorch`, **Go 1.23**, only direct dep
  `github.com/fsnotify/fsnotify v1.10.1` (+ indirect `golang.org/x/sys`). No `database/sql`,
  no sqlite anywhere in the tree.
- **`CGO_ENABLED=0` everywhere.** `make build` (`Makefile:11`), `make install`
  (`Makefile:16`), and crucially `make dist` (`Makefile:34-44`) cross-compile to
  `darwin/amd64 darwin/arm64 linux/amd64 linux/arm64` (`Makefile:6`) all with `CGO_ENABLED=0`.
  Releases are tar+sha256 (`Makefile:44`). **The SQLite driver must therefore be pure Go**
  (see §0.3 for the transitive-dependency note).

---

## B. Hard Claude Code limits the design must respect (confirmed)

These are stated up front because the whole watcher design exists to live within them.

1. **There is no non-TTY way to push input into a running `claude` session.** The only
   programmatic channel today is `tmux send-keys` (`tmux.go:155`/`:170`), i.e. keystrokes.
   We remove the *supervisor→manager* poke (`tmux.SendLine` to the `"manager"` window,
   `supervisor.go:108`); the manager→worker `ttorch send` path (`orchestrator.go:392`)
   intentionally survives (the manager still steers workers by keystroke). **Consequence:** we
   cannot have an external daemon notify the manager. Instead the manager starts a *background
   task* (`ttorch watch`) and is re-invoked by the **harness's own background-task-completion
   notification** when that task exits — the same in-session mechanism a Claude Code session
   uses for any backgrounded shell command; not a keystroke, not injectable from outside.
   **Precondition (load-bearing):** a background-completion notification causes a manager turn
   only when the session is *idle / awaiting input*; if the manager is mid-generation the
   completion is delivered at the next turn boundary (it does not preempt). This is acceptable
   because the manager arms the watcher **only at end-of-turn, just before going idle**.

2. **Cross-session notification is unsupported.** A worker session cannot signal the manager
   session directly. **Consequence:** workers communicate only by writing rows to the DB
   (`ttorch report/stage/note/follow-on`); the manager learns of them only through its own
   `ttorch watch` reading those rows. The DB is the sole cross-session channel.

3. **`claude --resume` does not reliably re-apply `--append-system-prompt-file`.** The manager
   is relaunched on restore with `ManagerResumeCommand` (`harness.go:144-156`), which *does*
   re-pass `--append-system-prompt-file` (`managerCharterArg`, `:125`), but the resumed session
   may not re-read it. **Consequence:** any change to the manager's standing instructions must
   ship through the **`CLAUDE.md` managed block** — i.e. `content/assets/AGENTS.global.md`,
   installed to `~/.claude/AGENTS.md` (`paths.GlobalAgentsMD()`, `paths.go:129`) with
   `CLAUDE.md` symlinked (`paths.go:132`), which **every session reloads from disk on
   launch** — *plus a manager relaunch* (`ttorch stop` then `ttorch`, or `ttorch resume`,
   `orchestrator.go:602`). The charter (`harness.go:93`) is updated too, but treated as
   best-effort; the managed block is the durable channel.

4. **`AskUserQuestion` pickers cannot be hardened against injected stdin** — but **we are
   removing manager-targeted injection**, so the picker becomes safe. The spec includes an
   **empirical validation step** (§4.6) and a **prose-only fallback**.

---

## C. Target architecture overview

```
            writes (own task only)                    reads/writes (all state)
 worker ──ttorch report/stage/note/follow-on──┐     ┌── ttorch status/tasks/spawn/land… ── manager
 worker ──────────────────────────────────────┤     │
 worker ──────────────────────────────────────┤     │   arms each turn (background task):
                                               ▼     ▼     ttorch watch --since <N>
                                       ┌───────────────────┐        │ blocks on actionable
                                       │  ~/.ttorch/state.db│◄───────┘ events (+PR poll +liveness)
                                       │  (SQLite, WAL)     │        │ exits → harness re-invokes
                                       │  projects/epics/   │────────┘ manager (completion channel)
                                       │  phases/tasks/     │
                                       │  events/notes      │   AWAITING LEAD ⇒ watcher cancelled,
                                       └───────────────────┘   not re-armed (window waits silently)
```

- **No process types into the manager session.** The supervisor daemon is retired (§5); its
  only non-injection jobs (stale/gone detection, PR-merge polling) fold into `ttorch watch`,
  which the manager owns.
- The **events table is both the audit spine and the watcher's signal**: a monotonically
  increasing `events.id` is the watermark; `ttorch watch --since <N>` blocks until an
  actionable event with `id > N` exists.

---

# The nine required deliverables

---

## 1. SQLite schema (DDL)

PRAGMAs are applied via the DSN (§2.1). The schema is created by migration `0001` (§1.5). All
timestamps are **`TEXT` RFC3339 / RFC3339Nano in UTC** (sortable, stable, no driver time
coercion); booleans are `INTEGER` 0/1. Every entity carries `owner`, `status`, and
`created_at`/`updated_at` per the brief.

### 1.1 Tables

```sql
-- ============================ migration 0001 (up) ============================
PRAGMA foreign_keys = ON;

-- Migration ledger (§1.5). Hand-managed, never dropped.
CREATE TABLE schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    applied_at TEXT    NOT NULL
);

-- Singleton manager record (replaces state.Manager / manager.json).
CREATE TABLE manager (
    id                 INTEGER PRIMARY KEY CHECK (id = 1),   -- exactly one row
    dir                TEXT    NOT NULL DEFAULT '',
    session_id         TEXT    NOT NULL DEFAULT '',
    watch_watermark    INTEGER NOT NULL DEFAULT 0,   -- last actionable events.id the manager consumed
    awaiting_lead      INTEGER NOT NULL DEFAULT 0,   -- 1 ⇒ manager is awaiting the lead (§4.6 backstop)
    updated_at         TEXT    NOT NULL
);

CREATE TABLE projects (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_path     TEXT    NOT NULL UNIQUE,           -- = state.Task.Project (repo root)
    name          TEXT    NOT NULL DEFAULT '',
    delivery_mode TEXT    NOT NULL DEFAULT 'pr',      -- DISPLAY CACHE ONLY (authority = AGENTS.md, §0.3); populated per §3.4
    status        TEXT    NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active','archived')),
    owner         TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL
);

CREATE TABLE epics (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    title       TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    status      TEXT    NOT NULL DEFAULT 'planned'
                   CHECK (status IN ('planned','in_progress','blocked','done','cancelled')),
    owner       TEXT    NOT NULL DEFAULT '',
    position    INTEGER NOT NULL DEFAULT 0,           -- manual ordering within a project
    created_at  TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL
);

CREATE TABLE phases (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    epic_id     INTEGER NOT NULL REFERENCES epics(id) ON DELETE RESTRICT,
    title       TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    status      TEXT    NOT NULL DEFAULT 'planned'
                   CHECK (status IN ('planned','in_progress','blocked','done','cancelled')),
    owner       TEXT    NOT NULL DEFAULT '',
    position    INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL
);

CREATE TABLE tasks (
    id             TEXT    PRIMARY KEY,               -- caller-chosen id (KEEP as TEXT, §A.1)
    project_id     INTEGER NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    epic_id        INTEGER NULL     REFERENCES epics(id)  ON DELETE SET NULL,
    phase_id       INTEGER NULL     REFERENCES phases(id) ON DELETE SET NULL,
    parent_task_id TEXT    NULL     REFERENCES tasks(id)  ON DELETE SET NULL,  -- follow-on/child
    created_by     TEXT    NOT NULL DEFAULT 'manager', -- manager | lead | worker:<id>

    title          TEXT    NOT NULL DEFAULT '',
    kind           TEXT    NOT NULL DEFAULT 'ship'
                      CHECK (kind IN ('ship','scout','cc')),               -- = state.Task.Kind
    status         TEXT    NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending','active','needs_input','blocked',
                                        'done','delivered','torn_down','abandoned')),
    stage          TEXT    NOT NULL DEFAULT '',         -- fine progress (free text, non-actionable)
    owner          TEXT    NOT NULL DEFAULT '',         -- worker:<id> | manager | '' (unassigned)

    -- runtime/coupling fields carried verbatim from state.Task
    window         TEXT    NOT NULL DEFAULT '',         -- "wk-<id>"
    worktree       TEXT    NOT NULL DEFAULT '',
    harness        TEXT    NOT NULL DEFAULT '',
    session_id     TEXT    NOT NULL DEFAULT '',
    pr             TEXT    NOT NULL DEFAULT '',
    gate_passed    INTEGER NOT NULL DEFAULT 0,          -- bool
    approved_by    TEXT    NOT NULL DEFAULT ''          -- '' | human | auto
                      CHECK (approved_by IN ('','human','auto')),
    reviewed_sha   TEXT    NOT NULL DEFAULT '',
    footprint      TEXT    NOT NULL DEFAULT '[]',       -- JSON array of strings (see note)

    -- liveness bookkeeping for the watcher's stale-detection across watch invocations (§4.4)
    last_pane_hash TEXT    NOT NULL DEFAULT '',
    idle_sweeps    INTEGER NOT NULL DEFAULT 0,

    created_at        TEXT NOT NULL,                    -- = state.Task.Created
    updated_at        TEXT NOT NULL,
    last_progress_at  TEXT NULL                         -- touched by report/stage; powers stale detection
);

-- Append-only audit spine AND the watcher's signal.
CREATE TABLE events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,    -- monotonic watermark (id-order == commit-order, §1.4)
    ts          TEXT    NOT NULL,                      -- RFC3339Nano
    entity_type TEXT    NOT NULL
                   CHECK (entity_type IN ('project','epic','phase','task','manager','system')),
    entity_id   TEXT    NOT NULL,                      -- task id, or stringified surrogate id
    type        TEXT    NOT NULL,                      -- see §1.3
    actor       TEXT    NOT NULL,                      -- manager | lead | worker:<id> | system
    from_status TEXT    NULL,
    to_status   TEXT    NULL,
    actionable  INTEGER NOT NULL DEFAULT 0,            -- 1 ⇒ should wake the manager
    payload     TEXT    NOT NULL DEFAULT ''            -- freeform / JSON detail
);

-- Freeform activity/commentary, append-only, never actionable.
CREATE TABLE notes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         TEXT    NOT NULL,
    task_id    TEXT    NULL REFERENCES tasks(id) ON DELETE CASCADE, -- usually task-scoped
    author     TEXT    NOT NULL,                       -- worker:<id> | manager | lead
    body       TEXT    NOT NULL
);

-- Indexes ---------------------------------------------------------------------
CREATE INDEX idx_events_actionable   ON events(actionable, id);        -- the watch hot path
CREATE INDEX idx_events_entity       ON events(entity_type, entity_id, id);
CREATE INDEX idx_tasks_status        ON tasks(status);
CREATE INDEX idx_tasks_project       ON tasks(project_id);
CREATE INDEX idx_tasks_owner         ON tasks(owner);
CREATE INDEX idx_tasks_parent        ON tasks(parent_task_id);
CREATE INDEX idx_epics_project       ON epics(project_id);
CREATE INDEX idx_phases_epic         ON phases(epic_id);
CREATE INDEX idx_notes_task          ON notes(task_id, id);
```

### 1.2 Status enums (per entity)

| entity | status values |
| --- | --- |
| projects | `active`, `archived` |
| epics, phases | `planned`, `in_progress`, `blocked`, `done`, `cancelled` |
| tasks | `pending` (backlog), `active`, `needs_input`*, `blocked`*, `done`* (work complete, awaiting manager), `delivered`, `torn_down`, `abandoned` |

`*` = **actionable** task statuses (the brief's "done/blocked/needs-input"). When a task enters
one of these *via a worker actor*, the writer inserts an `events` row with `actionable = 1`.
`stage` is orthogonal free text (e.g. `ramping`, `implementing`, `testing`, `validating`,
`addressing-review`) and never actionable. **The canonical "in-flight working" status is
`active` — `running` is not a status value** (used nowhere; example output in §4.3 uses
`active`).

### 1.3 Event-type vocabulary

`events.type` (extensible): `created`, `spawned`, `status_changed`, `stage_changed`, `note`,
`follow_on_created`, `validated`, `review_recorded`, `security_recorded`, `approved`, `merged`,
`delivered`, `torn_down`, `promoted`, `pr_armed`, `pr_merged`, `window_gone`, `idle_unreported`.

**Actionability invariant (§4.2, tested in increment 5):** `actionable=1` is written **only for
worker-actor `status_changed` transitions into {needs_input, blocked, done}**, plus the
watcher-generated external/liveness events `pr_merged`, `window_gone`, `idle_unreported`. Every
**manager-authored** lifecycle event (`spawned`, `validated`, `review_recorded`,
`security_recorded`, `approved`, `merged`, `delivered`, `promoted`, `torn_down`) is
`actionable=0`, so the manager's own actions never self-trigger the watcher. `watch` also
**defensively ignores** any `status_changed` row whose `actor != worker:*`.

**Footprint as JSON text.** `tasks.footprint` stores the `[]string` as a JSON array so the
existing pure overlap logic (`footprint.go:53` `FootprintOverlap`, which takes `[]string`) is
reused unchanged. *Alternative considered:* a `task_footprint(task_id, path)` child table —
rejected for v1 (forces a join + reassembly per overlap check for no behavioral gain; §9).

### 1.4 Parameterized queries, transactions, WAL + concurrency

- **Parameterized queries only.** Every statement uses `?` placeholders via `database/sql`
  (`QueryContext`/`ExecContext`); no value is ever string-concatenated into SQL. (Also
  satisfies the pre-submit DB reminder.)
- **Transactions.** Every multi-statement write (e.g. *update task status + append event*, or
  *create follow-on task + append event*) runs inside one transaction (`store.withTx`, §2.1/§2.3)
  so an observer never sees a status change without its event, and a crash leaves neither.
- **The transactions are `IMMEDIATE`** — see §2.1: `database/sql`'s `BeginTx` issues a *deferred*
  `BEGIN` by default, which (verified against modernc v1.53.0) lets two read-then-write
  transactions race to upgrade their snapshot and one fails *immediately* with
  `SQLITE_BUSY_SNAPSHOT (517)` that `busy_timeout` does **not** retry. The DSN therefore sets
  `_txlock=immediate` so every transaction begins `IMMEDIATE`, grabbing the write lock up front.
  This also **guarantees `events.id` order == commit order** (serialized immediate writers), so
  the watcher tracking `MAX(id)` can never skip a lower-id event.
- **WAL + concurrency.** `PRAGMA journal_mode=WAL` allows many concurrent readers plus one
  writer without readers blocking the writer — essential because several workers, the manager,
  and the watcher all touch the DB concurrently. `PRAGMA busy_timeout=5000` makes a writer that
  hits a *held* write lock retry for up to 5 s. Writes are tiny single-row statements held only
  for the duration of `withTx`. `PRAGMA synchronous=NORMAL` is durable under WAL and faster than
  `FULL`. Locking strategy in one line: **WAL for read concurrency + `BEGIN IMMEDIATE`
  (via `_txlock=immediate`) + `busy_timeout` to serialize concurrent writers.** (Networked/NFS
  home dirs break SQLite locking — §9; the current flock supervisor has the same limit.)

### 1.5 Reversible migrations / versioning

- A migration is `{Version int, Name string, Up string, Down string}`. SQL is embedded via
  `//go:embed migrations/*.sql` (`NNNN_<name>.up.sql` / `NNNN_<name>.down.sql`). `0001_initial`
  = the §1.1 DDL. Its `.down.sql` drops every table **child-first** (FK-enforced under
  `foreign_keys=ON`, verified error 1811 otherwise): `notes`, `events`, `tasks`, `phases`,
  `epics`, `projects`, `manager`, `schema_migrations`.
- The runner (`store.Migrate`) **bootstraps correctly for the table-absent case**: it first
  probes `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`
  and treats `0` as version 0 (a literal `SELECT MAX(version) FROM schema_migrations` on a fresh
  DB *errors* with `no such table`, verified — do **not** rely on catching that). It then reads
  `SELECT COALESCE(MAX(version),0)` and applies each pending `Up` **inside a transaction**,
  inserting the `schema_migrations` row in the same tx. `store.MigrateDown(to)` applies `Down` in
  reverse. Migrations never edit a shipped migration; new schema = a new numbered pair.
- **"Reversible" means schema-reversible, not data-reversible:** `MigrateDown` drops tables and
  destroys data. The **data-rollback path is the preserved `state.migrated/` directory** (§2.5),
  not `MigrateDown`.

---

## 2. The `internal/db` package (Go)

New package `internal/db`. It **absorbs persistence** from `internal/state`; the pure footprint
helpers stay in `internal/state` (`footprint.go`).

### 2.1 Open + DSN + PRAGMAs

```go
// import _ "modernc.org/sqlite"  // pure-Go driver, registered as "sqlite"

type Store struct { db *sql.DB; now func() time.Time }  // now is injectable for tests

func Open(path string) (*Store, error) {
    dsn := "file:" + path +
        "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
        "&_pragma=foreign_keys(on)&_pragma=synchronous(NORMAL)" +
        "&_txlock=immediate"     // REQUIRED: makes database/sql begin every tx IMMEDIATE (§1.4)
    sdb, err := sql.Open("sqlite", dsn)
    if err != nil { return nil, err }
    sdb.SetMaxOpenConns(1)   // serialize in-process access; cross-process via WAL+busy_timeout
    s := &Store{db: sdb, now: time.Now}
    if err := s.Migrate(context.Background()); err != nil { _ = sdb.Close(); return nil, err }
    return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
```

- **Driver:** `modernc.org/sqlite` — **pure Go, cgo-free**, the only viable choice given
  `CGO_ENABLED=0` cross-compilation (§A.5). `mattn/go-sqlite3` is rejected: it needs cgo and
  would break `make dist`. Registers under driver name `sqlite`. DSN `_pragma(...)` /
  `_txlock=immediate` forms verified to take effect against modernc v1.53.0.
- `SetMaxOpenConns(1)`: a single writer connection removes in-process self-contention;
  cross-process concurrency is handled by WAL + `busy_timeout`. **See §2.3 for the
  single-connection transaction discipline this mandates.**

### 2.2 The store interface (CRUD + events + watch)

```go
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error  // begins IMMEDIATE (DSN)

// --- hierarchy ---
func (s *Store) UpsertProject(ctx, repoPath, name string) (Project, error) // by UNIQUE repo_path
func (s *Store) GetProjectByRepo(ctx, repoPath string) (Project, bool, error)
func (s *Store) ListProjects(ctx) ([]Project, error)
func (s *Store) SetProjectMode(ctx, id int64, mode string) error           // display cache (§0.3/§3.4)
func (s *Store) CreateEpic(ctx, projectID int64, title, desc string) (Epic, error)
func (s *Store) CreatePhase(ctx, epicID int64, title, desc string) (Phase, error)
func (s *Store) SetEntityStatus(ctx, kind EntityKind, id int64, status, actor string) error

// --- tasks ---
func (s *Store) CreateTask(ctx, t Task, actor string) (Task, error)        // INSERT + 'created' event
func (s *Store) UpsertTask(ctx, t Task, actor string) (Task, error)        // INSERT or, if id exists, UPDATE fields (§3.4 spawn-from-backlog)
func (s *Store) GetTask(ctx, id string) (Task, bool, error)
func (s *Store) ListTasks(ctx, filter TaskFilter) ([]Task, error)          // subsumes the old state.List() semantics
func (s *Store) ListChildren(ctx, parentID string) ([]Task, error)
func (s *Store) SetTaskFields(ctx, id string, f TaskFields) error           // window/worktree/pr/etc (no event)
func (s *Store) ReportStatus(ctx, id, status, actor, msg string) (Event, error) // status + event (+ note if msg) — ONE tx
func (s *Store) SetStage(ctx, id, stage, actor string) (Event, error)       // stage + non-actionable event — ONE tx
func (s *Store) RecordDelivery(ctx, id string, d Delivery) error            // gate_passed/approved_by/reviewed_sha + event
func (s *Store) SetLiveness(ctx, id, paneHash string, idleSweeps int) error // watcher stale bookkeeping (§4.4)

// --- events + notes ---
func (s *Store) AppendEvent(ctx, e Event) (int64, error)                    // returns new events.id
func (s *Store) EventsSince(ctx, sinceID int64, onlyActionable bool) ([]Event, error)
func (s *Store) MaxActionableEventID(ctx) (int64, error)                    // COALESCE(MAX(id),0) WHERE actionable=1
func (s *Store) Timeline(ctx, taskID string) ([]TimelineItem, error)        // events ∪ notes by ts
func (s *Store) AddNote(ctx, taskID, author, body string) error

// --- manager singleton ---
func (s *Store) GetManager(ctx) (Manager, bool, error)
func (s *Store) SetManager(ctx, m Manager) error
func (s *Store) SetWatermark(ctx, eventID int64) error
func (s *Store) SetAwaitingLead(ctx, awaiting bool) error                   // §4.6 backstop

// --- migrations ---
func (s *Store) Migrate(ctx) error
func (s *Store) MigrateDown(ctx, toVersion int) error
```

`TaskFilter` carries `Status []string` (so `--status done,blocked,needs_input` →
`status IN (?,?,?)`), `ProjectID`, `Owner`, `ParentID`, and `ExcludeKind` (e.g. exclude `cc`).
Models (`internal/db/model.go`): `Project`, `Epic`, `Phase`, `Task`, `Event`, `Note`,
`Manager`, plus `TaskFilter`, `TaskFields`, `Delivery`, `TimelineItem`. `Task` is a **superset of
`state.Task`** exposing the same field names the orchestrator uses today (`ID, Window, Worktree,
Project` (= the joined repo path), `Harness, Kind, Created, PR, SessionID, GatePassed,
ApprovedBy, ReviewedSHA, Footprint`) plus `ProjectID/EpicID/PhaseID/ParentTaskID/CreatedBy/Title/
Status/Stage/Owner/UpdatedAt/LastProgressAt`.

### 2.3 Single-connection transaction discipline (mandatory)

Because `Open` sets `SetMaxOpenConns(1)`, **any statement issued against `s.db` while a `withTx`
transaction is open will block until the context deadline and deadlock the process** (the tx
holds the only connection; verified: a 2 s hang / `context deadline exceeded`). Therefore:

- Inside a `withTx` callback, **every statement must use the passed `*sql.Tx`, never `s.db`**.
- Composite operations are performed as **one** `withTx` that owns the whole sequence, passing
  the tx to lower helpers (`appendEventTx(tx,…)`, `noteTx(tx,…)`, `setStatusTx(tx,…)`). E.g.
  `ReportStatus` with a `-m` message does *status + event + note* in a single tx via tx-scoped
  helpers; it must **not** call `s.AddNote(...)` (which opens its own tx) from inside.
- Increment 0 adds a regression test that a nested `s.db` call inside `withTx` is caught (and the
  positive path uses only `*sql.Tx`). A `//go:build debug` assertion may flag accidental `s.db`
  use during an open tx.

### 2.4 Replacing / absorbing `internal/state`

- `internal/state` **keeps** `footprint.go` (pure overlap helpers, `:33`/`:53`). The overlap
  *callers* are in `internal/orchestrator/overlap.go`; the migration:
  - changes `computeConflicts` (`overlap.go:26`) and `footprintCandidate` (`:46`) to take
    `db.Task` (they only read `ID, Window, Project, Footprint, Kind`);
  - repoints `liveFootprintTasks` (`overlap.go:56`) from `m.Store.List()` (`:57`) to
    `m.Store.ListTasks(...)`;
  - changes `Manager.Live(t)` (`orchestrator.go:336`) to take `db.Task`.
- **Delete** the JSON persistence from `internal/state`: `Store`, `Save`, `Load`, `Remove`,
  `List`, `Manager`, `SaveManager`, `LoadManager`, `RemoveManager`, `writeJSON`
  (`state.go:42-163`) — once call sites move to `db.Store` (increment 1).
- The two `state.Store{Dir:…}` constructions (`orchestrator.go:49`, `supervisor.go:92`) become a
  `*db.Store` opened from `paths.StateDB()`. **`orchestrator.New` gains an error return**:
  `New(p paths.Paths) (*Manager, error)` (because `db.Open` runs `Migrate` and can fail). The
  ripple is enumerated in increment 1: `mgr()` (`cli.go:306`) becomes `mgr() (*orchestrator.Manager,
  error)` and each `cmdXxx` propagates via the existing `run(err)` path; the ~8
  `New(...)` sites in `orchestrator_test.go` (e.g. `:228`) update to handle the error. CLI
  commands are short-lived: one `Open` + `defer store.Close()` per process; the long-blocking
  `ttorch watch` holds its store for its lifetime.
- The manager record: `StartManager`/`restore`/`Reset` (`orchestrator.go:493`, `:551`, `:624`)
  call `GetManager/SetManager` instead of `LoadManager/SaveManager`.

### 2.5 State-migration plan for existing on-disk JSON

One-shot, idempotent importer `db.ImportLegacy(store, stateDir)`:

1. On `Open` (after `Migrate`), if `COALESCE(MAX(id),0)==0` on `events` **and** `tasks` is empty
   **and** legacy files exist under `paths.StateDir()` (`*.meta.json` / `manager.json`), run the
   import (else skip). (Use `COALESCE(MAX(id),0)` / `sql.NullInt64`, never a bare `MAX(id)==0`:
   `MAX` on an empty table is `NULL`, verified.)
2. For each `<id>.meta.json` (parsed with the old `state.Task` JSON tags, carried as a private
   struct): `UpsertProject` from `t.Project`; `CreateTask` copying
   `kind/window/worktree/harness/session_id/pr/gate_passed/approved_by/reviewed_sha/footprint/
   created_at` verbatim. **Initial status:** `active` if the task's tmux window is live at import
   time, else `torn_down`. Emit a `created` event (`actor='system'`, `payload='imported …'`).
3. Import `manager.json` → `SetManager`; set `watch_watermark = MaxActionableEventID()`.
4. **Do not delete** the legacy dir; rename it to `state.migrated/` (reversible; rollback +
   inspection). Log a one-line note.
5. Idempotent: re-running is a no-op (step 1's guard fails after the first import).

---

## 3. Command surfaces (exact CLI)

New `paths` helper, honoring the existing `envOr` override pattern (`paths.go:19`):

```go
func (p Paths) StateDB() string { return envOr("TTORCH_DB", filepath.Join(p.Home, "state.db")) }
```

All new commands are added the same way as today: a `case` in `cli.Main` (`cli.go:52-145`), a
`cmdXxx` function, and a usage line (`cli.go:1180`).

### 3.1 Worker-facing (write the calling worker's own task)

**Task + DB identity for workers.** At spawn, the orchestrator writes a git-excluded
`<worktree>/.ttorch/task` file (same exclusion mechanism as the Stop hook,
`harness.go:269 excludeInWorktree`) containing `task_id` and the DB path, **and exports
`TTORCH_TASK_ID=<id>` and `TTORCH_DB=<path>`** into the worker's launch environment (prepended to
the harness launch command built in `harness.BriefCommand`, `harness.go:161`). `ttorch report/
stage/note/follow-on` resolve **the task** by `--task` → `$TTORCH_TASK_ID` → walking up from
`cwd` to find `.ttorch/task`, and resolve **the DB** from `$TTORCH_DB` (falling back to
`StateDB()`'s default). This is TTY-independent and survives subshells.

| command | effect |
| --- | --- |
| `ttorch report <done\|blocked\|needs-input\|active> [--task id] [-m "msg"]` | `ReportStatus`: set `tasks.status`, touch `last_progress_at`, append a `status_changed` event (one tx; `-m` writes a note in the same tx). `done/blocked/needs-input` (worker actor) ⇒ `actionable=1`. |
| `ttorch stage <text> [--task id]` | `SetStage`: set `tasks.stage` + `last_progress_at`, append `stage_changed` event, `actionable=0`. |
| `ttorch note <text…> [--task id]` / `note --message-file <p>` / `note -` | `AddNote`. Reuses the safe message resolution of `ttorch send` (`cli.go:540 resolveSendMessage`: inline / stdin / `--message-file`). `actionable=0`. |
| `ttorch follow-on <new-id> --title "…" [--touches "a,b"] [--task parent]` | `CreateTask{status:pending, parent_task_id:<parent>, created_by:worker:<parent>, kind, footprint}` + `follow_on_created` event (`actionable=0` — backlog, surfaced on the manager's next re-derive, §9). Does not spawn. |

Workers never pass an actionability flag; it is fixed by status, so a worker cannot spam the
manager except by genuinely transitioning to done/blocked/needs-input.

### 3.2 The watcher

```
ttorch watch [--since <eventId>] [--timeout <dur>] [--coalesce <dur>]
ttorch watch --reset        # reap any orphan watcher, then return (used on manager restart, §4.5)
```

`--since` defaults to the manager's stored `watch_watermark`. Protocol in §4.

### 3.3 Manager query/management surfaces (read the DB)

| command | effect |
| --- | --- |
| `ttorch status` | still derives live tmux state (`working/idle/gone` via `DeriveState`, `orchestrator.go:352`) for each task with a window, but the row set and lifecycle columns come from the DB (`tasks.status`, `stage`, `owner`, `footprint`). Renders the existing table (`cli.go:407 renderStatus`) plus `STATUS`/`STAGE`/`OWNER`. |
| `ttorch tasks [--project p] [--epic e] [--status s[,s…]] [--tree] [--timeline <id>]` | DB-backed query incl. `pending` backlog. `--status` accepts a **comma-separated list** (→ `TaskFilter.Status []string`). `--tree` prints projects→epics→phases→tasks; `--timeline <id>` prints `Timeline` (events ∪ notes by ts). |
| `ttorch project add <repo> [--name n]` · `project ls` | `UpsertProject` (also `SetProjectMode(projectinit.ReadMode(repo))` to populate the display cache), `ListProjects`. |
| `ttorch epic add --project <id> --title "…"` · `epic ls [--project p]` · `epic set-status <id> <s>` | manage epics. |
| `ttorch phase add --epic <id> --title "…"` · `phase ls [--epic e]` · `phase set-status <id> <s>` | manage phases. |
| `ttorch task add <id> --project <p> [--epic e] [--phase ph] [--title "…"] [--touches "a,b"]` | create a `pending` backlog task **without spawning**. |

### 3.4 Lifecycle recording in existing verbs

| verb (file:line) | DB effect |
| --- | --- |
| `SpawnWithFootprint` (`orchestrator.go:127`) | `UpsertProject` (+`SetProjectMode`); **`UpsertTask`** — if a `pending` backlog row with this id exists, `SetTaskFields` + `ReportStatus('active')`; else `CreateTask(status='active')`. Sets owner/window/worktree/harness/session_id/footprint; event `spawned` (manager, `actionable=0`). Writes `.ttorch/task` + env (§3.1). |
| `OpenCC` (`orchestrator.go:664`) | widen the id to `cc-HHMMSS-<4hex>` (`orchestrator.go:669`) to avoid a `TEXT PRIMARY KEY` collision (two cc sessions in one second); `CreateTask(kind='cc', status='active')`. |
| `Teardown` (`orchestrator.go:397`) | **No longer hard-deletes.** Sets `status='torn_down'`; on `Pool.Release` (`:412`) **blank `worktree`** so the retained row doesn't alias a worktree the pool reassigns; event `torn_down`. |
| `Validate` (`:712`) | event `validated`, payload pass/fail counts; `actionable=0`. |
| `TrustRecord` (`:819`) / `SecurityReview` (`:916`) | `RecordDelivery` + event `review_recorded`/`security_recorded`. |
| `Approve` (`:726`) | event `approved` (actor=lead). |
| `MergeLocal` (`:965`) / `Land` (`:1265`) | `status='delivered'`; event `delivered`/`merged`. **Keep `writeAudit`** (`:1660`) for trusted merges — the events row is a best-effort mirror; `audit.log` stays the must-succeed finance record and the abort-on-failure semantics (`:1105`) are unchanged. |
| `Promote` (`:1569`) | `kind='ship'`; event `promoted`. |
| `ArmPRCheck` (`:1582`) | `tasks.pr=url`; event `pr_armed`. PR-merge detection now lives in `ttorch watch` (§4.4). |

---

## 4. The manager-watcher protocol (precise)

### 4.1 Arming (each turn)

At the **end of every turn in which it is not awaiting the lead**, the manager runs, as a
**background task** (the harness's own background-command facility — not a keystroke):

```
ttorch watch --since <watch_watermark>
```

`--since` defaults to `manager.watch_watermark`, so a bare `ttorch watch` is equivalent. The
manager **cancels any prior `watch` background task before arming a new one** (harness-native
cancellation), so exactly one is in flight; the watch flock (§4.5) is the crash backstop. Per §B.1
the completion notification wakes the manager only when it is idle — which holds because arming is
the last thing a turn does.

### 4.2 Blocking + the event watermark

```
ttorch watch loop:
  acquire watch singleton flock (paths.WatchPIDFile)            // §4.5
  since := flag or manager.watch_watermark
  loop:
    // each sweep opens and CLOSES its own short read tx (no read tx held across wait — §9 WAL)
    if GetManager().awaiting_lead:  continue waiting (do NOT surface/exit)   // §4.6 backstop
    pollArmedPRs()      // §4.4: side-effecting — persists pr_merged events, returns nothing
    pollLiveness()      // §4.4: side-effecting — persists window_gone / idle_unreported, returns nothing
    rows := EventsSince(since, onlyActionable=true)              // indexed; ignores actor!=worker status_changed
    if rows non-empty:
        sleep(coalesce)                                          // default 750ms: absorb a burst
        rows = EventsSince(since, true)                          // single source of the batch (re-read)
        batch := dedupeByEntityKeepingMaxID(rows)                // §4.3
        max := maxID(rows)
        print batch + "WATCH_WATERMARK=<max>"
        SetWatermark(max)                                        // BEFORE releasing the flock (invariant)
        release flock; exit 0
    if timeout exceeded: print "WATCH_TIMEOUT"; release flock; exit 0
    wait(pollInterval, or fsnotify on state.db-wal to react in ~ms)   // §9
```

- The **watermark** is `MAX(events.id)` among the returned actionable rows. `watch` prints it
  (`WATCH_WATERMARK=<n>`) **and** persists it via `SetWatermark` **before releasing the flock**
  (so the next armed watcher reads a watermark already reflecting the surfaced batch). On restart
  the manager re-arms with the stored value.
- Manager-authored events are `actionable=0` (§1.3) so the actionable filter **skips** them; the
  watermark is the max actionable id consumed, and `EventsSince(since, true)` re-applies the
  filter each read.

### 4.3 Coalesced batch format

`watch` **dedups by `entity_id` alone, keeping the row with the maximum `events.id`** (latest
transition wins; superseded earlier transitions for the same task are dropped from the surfaced
batch but remain in `events` for audit). It prints from/to status from that latest event:

```
ttorch watch: 3 actionable update(s) since #142 (now #150)
  task=auth-refactor    active → done            owner=worker:auth-refactor   (#147)
  task=api-gateway      active → blocked          "needs schema decision"      (#149)
  pr-merged             task=logging-cleanup      <pr-url>                      (#150)
next: ttorch tasks --status done,blocked,needs_input ; then land / answer / dispatch
WATCH_WATERMARK=150
```

(Dedup by `entity_id` — not `(entity_id, to_status)` — so a task that went
`blocked → active → done` while the manager awaited the lead surfaces **one** current row
(`→ done`), never a stale `→ blocked` alongside it.)

### 4.4 What `watch` polls (folded in from the retired supervisor, zero injection)

Poll functions are **side-effecting only**: they persist actionable events to the DB (committed
before the `EventsSince` re-read) and return nothing to merge; `EventsSince` is the single source
of the surfaced batch.

- **Armed-PR merges** (replaces `scanChecks`, `supervisor.go:204-227`): for each task with
  `pr != ''` and no prior `pr_merged` event, rate-limited `gh pr view <pr> --json state`; on
  `MERGED`, append `pr_merged` (actionable). Requires `gh` (degrade silently if absent, as
  `supervisor.go:205`).
- **Liveness safety net** (replaces `scanStale`, `supervisor.go:266-293`): scoped to tasks in
  status `active` **whose `last_progress_at` is stale** (a task already in `needs_input`/`blocked`,
  or with an open unresolved actionable event, is **excluded** — it is already surfaced, and a
  worker correctly awaiting a relayed answer must not be re-flagged as stalled). Capture the pane
  (`tmux.CapturePane` + `supervisor.Busy`). If the window is gone → `window_gone` (actionable).
  Else compute the pane hash and compare to the row's persisted `last_pane_hash`/`idle_sweeps`
  (`SetLiveness`): unchanged + not busy for **two** sweeps → `idle_unreported` (actionable).
  **Persisting `last_pane_hash`/`idle_sweeps` on the task row is what lets the two-sweep count
  survive across short-lived `watch` invocations** (a long-blocking `watch` also accumulates it on
  its internal poll ticker before it ever returns). A pane change resets the count.
- Tab-coloring (`scanLabels`, `supervisor.go:316`) is cosmetic; move into `watch` best-effort or
  drop (§5).

### 4.5 Orphan-watcher cleanup + singleton

- `watch` acquires an exclusive `flock` on `paths.WatchPIDFile()` (new; mirrors
  `supervisor.go:441-499`, **flock-as-truth**, not pid-as-truth).
- **On manager (re)start** (`StartManager`/`restore`) the manager runs `ttorch watch --reset`:
  it SIGTERMs any recorded watch pid **only after verifying the process is a `ttorch watch`**
  (guard against pid reuse), then **blocks until it can itself acquire the flock** (bounded
  retry, as `supervisor.acquire`, `supervisor.go:449 maxAcquireAttempts`) — confirming the orphan
  released before returning. This closes the SIGTERM-is-async race where a freshly-armed watcher
  would otherwise fail to acquire the lock and silently exit.
- A newly-armed `watch` that finds the lock held **retries briefly** (rather than exit-on-contention)
  so a slow orphan release never drops the wake.
- `watch` also self-exits if the `manager` tmux window is absent (`tmux.WindowExists`,
  `tmux.go:105`) for N consecutive sweeps, so a crash that skips `--reset` can't leave a watcher
  blocking forever.

### 4.6 Awaiting-lead = silent (the core rule)

When the manager surfaces a **needs-your-decision / blocker / question** to the lead it must, as
the first action of that turn:

1. **Cancel any in-flight `ttorch watch` background task** (harness-native cancel), and
2. **Set `manager.awaiting_lead=1`** (`ttorch await-lead` verb or `SetAwaitingLead(true)`), and
3. **Not arm a new watcher.**

The turn ends; the window waits for the lead (the lead is the interrupt). No process types
anything into the manager; nothing fills the window; the manager cannot be pulled off the pending
decision. Actionable events that arrive meanwhile are recorded (`actionable=1`) but **not
surfaced** — they are delivered the moment the lead returns and the manager clears
`awaiting_lead` and re-arms `watch` (which then returns the accumulated backlog from the old
watermark).

**Code backstop (so a missed protocol step degrades to silence, not an interrupt):** a running
`watch` checks `manager.awaiting_lead` each sweep (§4.2); if set, it **keeps blocking** — it does
not surface or exit — until the flag clears. Thus even if a prior turn's watcher was not cancelled,
it will not pull the manager off the decision; when the lead returns and the manager clears the
flag, the still-blocking watch fires on its next sweep.

**AskUserQuestion empirical validation (required before relying on pickers):**

1. In a live manager session, open an `AskUserQuestion` picker.
2. From another shell, insert an actionable event (`ttorch report blocked --task <x>`); separately
   arm `ttorch watch` and let it fire.
3. **Confirm the watcher's background-completion does not auto-select or dismiss the picker**
   (expected: it cannot — completion is the harness's own notification, not stdin/keystrokes, §B.4).
4. **Also test the mid-generation case** (§B.1): let `watch` exit while the manager is actively
   generating; confirm the completion is queued and surfaced at the next turn boundary with no
   disruption.
5. **Prose-only fallback:** if any firing ever disturbs an open picker, the manager protocol
   switches to plain-text questions (no picker) while awaiting the lead. Record the test result in
   the PR that lands the watcher.

---

## 5. Supervisor changes

**Recommendation: retire the supervisor daemon entirely.** Its only justification was the poke
(now removed) plus polling the manager-owned `watch` now does with zero injection.

**Delete:**
- The entire auto-driver: `sendPoke`/`inspectManager` (`supervisor.go:73-74`, wired `:108-120`),
  `pokePending`/`lastPoke` (`:71-72`), `requestPoke` (`:373-379`), `flushPoke` (`:387-410`),
  `autodriveDisabled` (`:366`), `pokeDirective` (`:361`), `managerWindow` poke use (`:355`), and
  every `requestPoke()` call (`:224`, `:251`, `:286`). **This removes the only
  `tmux.SendLine`-into-the-manager path** (`supervisor.go:108`).
- `internal/wake` package, the wake-queue file (`paths.WakeQueue()`, `paths.go:80`), and the CLI
  `ttorch wake drain` (`cli.go:711`) and `ttorch wait` (`cli.go:744`). (`wait` may be kept as a
  thin deprecated alias printing "use `ttorch watch`".)
- `scanSignals` + the turn-end marker mechanism (`supervisor.go:231-254`, `paths.TurnEndMarker`,
  `paths.go:87`) — workers report via the DB now. The `Stop` hook
  (`harness.InstallTurnEndHook`, `harness.go:235`) no longer needs to `touch` the marker; keep
  only its `IncludeCoAuthoredBy:false` setting (`harness.go:249`) by writing a trimmed settings
  file.
- `ensureSupervisor` (`orchestrator.go:321`) and its calls (`:194`), `ttorch supervise`
  (`cli.go:703`), `ttorch daemon …` (`cli.go:624-643`) and `daemonStart/Stop/Status`
  (`cli.go:1107-1152`). The PID file / beacon (`paths.go:73`/`:76`) retire with the daemon.

**Moves into `ttorch watch`** (zero injection, manager-owned): PR-merge poll (was `scanChecks`)
and stale/gone liveness (was `scanStale`) — §4.4.

**Keep + relocate:** `supervisor.Busy` (`supervisor.go:611`) is shared with `DeriveState`
(`orchestrator.go:357`) and the watcher. **Relocate it in increment 0** to a neutral home (e.g.
`internal/tmux` or a small `internal/livestate`) — a pure leaf move with no orchestrator-logic
coupling — so the later watcher/supervisor work does not collide with `orchestrator.go`. Its
current internal callers are `supervisor.go:119/276/327/611`.

**Net:** afterwards **no code path types into the manager session**; an increment-6 test asserts
no `tmux.SendLine`/`SendKey` targets the `"manager"` window anywhere.

---

## 6. Manager protocol changes (content + charter)

All four surfaces change; per §B.3 the **`CLAUDE.md` managed block is the durable channel** and
shipping requires `ttorch update`/`install` + a manager relaunch.

### 6.1 `content/skills/ttorch-manager/SKILL.md`

- **Rule 5 "Run the autonomy loop"** (`SKILL.md:53-57`) currently says *"the supervisor pokes
  you on actionable wakes"*. Rewrite to: *"After each turn in which you are not awaiting the lead,
  arm `ttorch watch` as a background task; when it returns an actionable batch, re-derive from the
  DB and advance all actionable state, then re-arm. When you surface a decision to the lead,
  **first cancel any in-flight watcher and do not arm a new one** — the window waits silently."*
- **Rule 1** (`SKILL.md:32-36`) and the loop's **Re-derive** step (`SKILL.md:64`): add the DB as
  the primary source (`ttorch tasks`/`ttorch status`); `ttorch peek` and git/PR remain for live
  detail. Introduce the **verified task list**: reconstruct the task list from the DB
  (`ttorch tasks`) on every restart, not from memory.
- **Pre-yield checklist** (`SKILL.md:88-103`): add **hold-on-blocker** — surface a decision once,
  then wait silently (watcher cancelled + not re-armed); do not re-poll or re-ask.
- **Commands table** (`SKILL.md:105-121`): add `ttorch tasks`, `ttorch watch`, and the
  project/epic/phase verbs; replace `wake drain`/`wait`.
- Drop "the supervisor" framing throughout (there is no daemon).

### 6.2 `content/assets/AGENTS.global.md` (managed block → `~/.claude/AGENTS.md`)

- The loop bullet (`AGENTS.global.md:25-27`) becomes: *"Arm `ttorch watch` after each non-blocking
  turn; when it returns, advance every actionable task — land green, unblock/redispatch stuck,
  dispatch disjoint backlog — then re-arm. **When awaiting a lead decision, cancel the watcher and
  do not re-arm; wait silently.**"* Add a source-of-truth bullet naming the DB
  (`ttorch tasks`/`ttorch status`).
- Installed to `paths.GlobalAgentsMD()` (`paths.go:129`) with `CLAUDE.md` symlinked (`:132`);
  reloads each session — the channel that survives §B.3.

### 6.3 The charter (`internal/harness/harness.go:93`)

- Rewrite rule (5) of `managerCharter` to the watch-loop + cancel-on-awaiting-lead wording (drop
  *"the supervisor pokes you"*). `WriteManagerCharter` (`:135`) re-emits it. Best-effort per §B.3;
  the managed block is authoritative.

### 6.4 Worker contract (`content/agents/ttorch-worker.md`)

- Replace *"Report status through the channels in your brief"* (`ttorch-worker.md:23-24`) with a
  **mandatory reporting** section: the worker MUST call `ttorch report done|blocked|needs-input`
  at the matching transition; use `ttorch stage` for progress, `ttorch note` for activity, and
  `ttorch follow-on` to file child tasks; state that *the manager is woken only by these reports*
  (the §4.4 liveness net is a backstop, not a substitute).
- Update the spawn brief stub (`orchestrator.go:1693 writeBriefStub`) with the same instruction.

---

## 7. Recovery semantics

A restarted manager reconstructs **full state from the DB** — no pane scraping:

1. **Reap orphan watcher:** `ttorch watch --reset` (§4.5).
2. **Load singleton:** `GetManager` → dir, session_id, `watch_watermark`, `awaiting_lead`
   (replaces `LoadManager`, `orchestrator.go:493/551`).
3. **Rebuild tmux from DB rows:** `restore` (`orchestrator.go:542-598`) iterates
   `ListTasks(status ∈ {active, needs_input, blocked, done} **AND kind != 'cc'**)` — preserving
   today's intentional cc exclusion (`orchestrator.go:545`: ad-hoc cc sessions are lead-driven and
   not auto-restored) — and rebuilds the manager window + each worker window whose worktree still
   exists, resuming each session (`WorkerResumeOrFresh`, `harness.go:203`). Ownership/window/
   worktree come straight from the row.
4. **Re-derive the verified task list:** `ttorch tasks` gives projects→epics→phases→tasks incl.
   `pending` backlog; the manager surfaces the standing actionable backlog (tasks in
   `done/blocked/needs_input`) to itself/the lead.
5. **Re-arm at the right watermark:** every transition (even those while the manager was down) is
   in `events` and reflected in `tasks.status`, so step 4 surfaces the standing backlog and the
   manager arms `ttorch watch --since <watch_watermark>` to block only on future events.
6. **Reconcile DB vs live tmux:** keep `Recovery` (`orchestrator.go:1628-1652`), DB-backed:
   `active` task with no window → emit `window_gone`; orphan `wk-*` window with no row → note.

Crash-consistency: every status change and its event are written in one IMMEDIATE transaction
(§1.4), so recovery never sees a half-applied transition.

---

## 8. Sequenced build plan (landable increments)

**Hot files that force serialization:** `internal/orchestrator/orchestrator.go` (increments 1, 2,
4, 5) and `internal/cli/cli.go` (2, 3, 4, 6). The `Busy` relocation is pulled into increment 0 (a
pure leaf move) precisely so the watcher work (increment 3) does **not** touch `orchestrator.go`.
Where parallel increments would still collide on `orchestrator.go`, prefer doing those
orchestrator edits in one combined PR.

| # | increment | footprint (files) | parallel? | deterministic tests |
| --- | --- | --- | --- | --- |
| 0 | **`internal/db` foundation + `Busy` relocation** — add `modernc.org/sqlite`; `Open`+DSN(incl. `_txlock=immediate`)+`SetMaxOpenConns(1)`; migration runner (sqlite_master bootstrap) + `0001` up/down; models; `Store` CRUD/events/notes; `EventsSince`/`MaxActionableEventID`/watermark; the §2.3 tx discipline. **Relocate `supervisor.Busy`** to a shared home. **No orchestrator-logic changes.** | `go.mod`, `go.sum` (+~9 transitive modules), `internal/db/**` (new), `internal/paths/paths.go` (+`StateDB`), `internal/tmux` or `internal/livestate` (Busy), `internal/supervisor/supervisor.go`+`internal/orchestrator/orchestrator.go` (Busy import only) | **base — must land first** | migrate up→down→up (FK-on, child-first drop); fresh-DB⇒v0 bootstrap; CRUD round-trips (footprint JSON ↔ `[]string`); FK + CHECK rejections; **two-`*sql.DB` (separate pools) read-then-upgrade contention** test (catches a regression away from `_txlock=immediate`; a single shared pool with MaxOpenConns(1) can't exercise it); nested-`s.db`-in-`withTx` deadlock guard; `EventsSince` watermark + actionable filter. |
| 1 | **Flip persistence + import** — `orchestrator.New(p) (*Manager, error)`; `mgr()` opens DB (`defer Close`); `overlap.go`/`Live` → `db.Task`; repoint `liveFootprintTasks` to `ListTasks`; **repoint supervisor's 3 `List()` sites** (`supervisor.go:212/267/317`) to `db.ListTasks` so the daemon keeps compiling/running until increment 6; delete JSON persistence from `internal/state` (keep `footprint.go`); `db.ImportLegacy`. | `internal/orchestrator/orchestrator.go`, `overlap.go`, `internal/state/state.go` (delete persistence), `internal/db/import.go`, `internal/cli/cli.go` (`mgr()` ripple, ~22 sites), `internal/orchestrator/orchestrator_test.go` (~8 `New` sites), `internal/supervisor/supervisor.go` (Store type + 3 `List()` sites) | **serial (riskiest)** | import fixture `*.meta.json`/`manager.json` → rows + `created` events; live-window→`active` vs absent→`torn_down`; idempotent re-import; **`make build` + supervisor still compiles/runs**; existing orchestrator suite green against DB (`t.Setenv("TTORCH_HOME", t.TempDir())`, `orchestrator_test.go:224`). |
| 2 | **Worker reporting CLI** — `report/stage/note/follow-on`; spawn writes `.ttorch/task` + env; `UpsertTask` backlog→active; `OpenCC` id widening; worker-contract content. | `internal/cli/cli.go`, `internal/orchestrator/orchestrator.go` (Spawn/OpenCC; `writeBriefStub`), `content/agents/ttorch-worker.md` | parallel w/ 3,4 **after 1**; serialize the `orchestrator.go` edits vs 4,5 | each verb mutates DB + correct event/actionable; task-id+DB discovery (flag/env/cwd walk-up); `note` reuses `resolveSendMessage` cases; two cc spawns in one second get distinct ids; backlog-id spawn UPDATEs (no PK collision). |
| 3 | **`ttorch watch`** — block on actionable events + coalesce + dedup-by-entity + watermark(persist-before-unlock); PR-merge poll; stale/gone liveness (DB-persisted sweeps; excludes needs_input/blocked); flock singleton + `--reset` (flock-as-truth, pid-reuse guard); `awaiting_lead` backstop; **disable the legacy poke** via the `TTORCH_NO_AUTODRIVE` seam (`supervisor.go:366`) so the transitional window (incs 3–6) has exactly one wake mechanism. | `internal/cli/cli.go` (cmdWatch, await-lead), `internal/db` (`EventsSince`/liveness/awaiting_lead), `internal/watch/**` (new), `internal/paths` (`WatchPIDFile`) | parallel w/ 2,4 **after 1** | injected clock + DB: returns on inserted actionable event; coalesce + **dedup-by-entity keeps latest** (blocked→active→done ⇒ one `→done` row); watermark print+persist-before-unlock; timeout; flock singleton (mirror supervisor flock tests); `--reset` waits for flock; liveness emits `window_gone`/`idle_unreported` via fake-pane seam and is suppressed for `needs_input`; `awaiting_lead` keeps watch blocking. |
| 4 | **Query surfaces** — DB-backed `ttorch status` (+tmux liveness) and `ttorch tasks` (`--tree`/`--timeline`/multi-`--status`); project/epic/phase/task verbs (+`SetProjectMode`). | `internal/cli/cli.go`, `internal/db` (queries), `internal/orchestrator/orchestrator.go` (`Status` join) | parallel w/ 2,3 **after 1**; coordinate `orchestrator.go` w/ 2,5 | render from seeded rows; tree/timeline ordering; multi-status filter `IN (?,…)`; `pending` backlog included; mode cache populated. |
| 5 | **Lifecycle recording** in spawn/teardown/validate/trust/security/approve/merge/land/promote/arm-pr (§3.4); teardown sets `torn_down` + blanks worktree on release. | `internal/orchestrator/orchestrator.go` | **serial vs 1,2,4** (same file) | each verb writes expected status+event; **every manager-authored event is `actionable=0`** (the §1.3 invariant); teardown preserves row + blanks worktree; trusted-merge still writes `audit.log` + abort-on-failure intact (`:1105`); delivery sets `delivered`. |
| 6 | **Retire supervisor + wake + injection** (§5). | `internal/supervisor/**` (delete), `internal/wake/**` (delete), `internal/cli/cli.go`, `internal/orchestrator/orchestrator.go` (ensureSupervisor), `internal/harness/harness.go` (Stop hook), `internal/paths/paths.go` | **after 2 + 3** (replace before remove) | no `tmux.SendLine`/`SendKey` targets `"manager"` anywhere (grep + unit assertion); spawn starts no daemon; `wait` alias (if kept) points to `watch`. |
| 7 | **Manager protocol content + empirical picker test** (§6 + §4.6). | `content/skills/ttorch-manager/SKILL.md`, `content/assets/AGENTS.global.md`, `internal/harness/harness.go` (`managerCharter`), `docs/**` | **after 3,6** | `content_test.go`: managed-block markers present; required phrases ("ttorch watch", "cancel the watcher", "do not re-arm"); no "supervisor pokes". The AskUserQuestion test is a documented manual/integration step whose result is recorded in the PR. |

**Critical path:** 0 → 1 → {2,3,4 in parallel, coordinating `orchestrator.go`} → 5 → 6 → 7.
Every increment keeps `make lint && make test` green (the repo gate, `AGENTS.md:14`).

---

## 9. Risks & open questions (honest)

**Concurrency**
- *`SQLITE_BUSY` under heavy fan-out.* Many workers + manager + watcher writing at once can
  exceed `busy_timeout`. Mitigation: tiny IMMEDIATE transactions, WAL, `SetMaxOpenConns(1)`.
  Surface a clear retryable error, never a silent drop.
- *In-process single-connection deadlock.* `SetMaxOpenConns(1)` + a `s.db` call inside an open
  `withTx` deadlocks the process (verified). Mitigation: the §2.3 tx discipline (tx-handle only)
  + an increment-0 guard test.
- *Networked home dir.* SQLite locking is unreliable on NFS/SMB; `~/.ttorch` on a network mount
  is unsupported (the current flock supervisor has the same limit, `supervisor.go:454`).
- *WAL growth / checkpointing.* `PRAGMA wal_checkpoint(TRUNCATE)` returns `busy` (no truncation)
  while **any** read snapshot is open (verified). The watcher is the one long-lived reader, so it
  **must open and close its read tx each sweep** (no read tx held across `wait()`); a periodic
  checkpoint on manager start then works. Holding a read tx across the blocking wait would let
  the WAL grow unbounded.

**The residual hidden poll inside the watcher (honest)**
- SQLite has **no cross-process change notification**, so `ttorch watch` *must poll* the `events`
  table (and `gh`, and panes). We have not eliminated polling — we **moved it from a separate
  daemon into the manager-owned watcher and removed all manager-targeted injection**. Mitigation:
  `fsnotify` on `state.db-wal` to wake the poll loop (long steady-state interval, ~ms reaction);
  the indexed `idx_events_actionable` query is O(new rows). *Open question:* is `fsnotify` on the
  `-wal` file reliable across macOS/Linux for our write pattern, or fall back to a fixed short
  interval?

**Watcher orphaning**
- A crashed manager can leave a blocked `watch`. Mitigated by flock singleton + `--reset`
  (flock-as-truth, pid-reuse guard, blocks until it owns the lock) + self-exit when the manager
  window is absent (§4.5). Residual: a `watch` holding a DB connection during the gap; bounded by
  self-exit.

**Migration risk**
- The JSON→DB import is one-shot. Mitigations: idempotent guard; **keep** the source by renaming
  to `state.migrated/`; `MigrateDown` (schema only); `integrity_check` on open. *Open question:*
  ship `ttorch db verify`/`ttorch db export` for support and rollback?

**UX**
- *Unreported worker.* Invisible until the §4.4 liveness net fires (`idle_unreported`). The net is
  **required**, not optional — dropping it to chase "no polling" reinstates manager blindness.
- *Liveness false positives.* A worker legitimately waiting on a relayed answer looks idle; §4.4
  excludes `needs_input`/`blocked` and tasks with an open actionable event, and the manager should
  move such a worker to `needs_input`. Residual: a worker idle *before* it reports needs-input.
- *Coalesce latency.* The 750 ms window slightly delays surfacing a burst; tunable via `--coalesce`.
- *Actionability of follow-ons.* `follow_on_created` is **non-actionable** (backlog, not an
  interrupt). *Open question:* if leads want follow-ons surfaced sooner, flip to actionable — at
  the cost of risking pulling the manager off a decision.

**Source-of-truth split**
- `delivery_mode` is a display cache; the gate reads `AGENTS.md` (§0.3). The cache is populated via
  `SetProjectMode(projectinit.ReadMode(repo))` on project-add/spawn/restore so it is not
  permanently the `'pr'` default (otherwise a trusted repo would mis-display). Gates ignore the
  cache, so any residual drift is cosmetic — documented so no one "optimizes" the gate to read it.

**Audit duality**
- Trusted merges write both `audit.log` (must-succeed, abort-on-failure, `orchestrator.go:1105`)
  and a best-effort `events` row. *Open question:* once `events` is proven durable, retire
  `audit.log`? Keep both for at least one release.

**AskUserQuestion**
- Safe *because* manager-targeted injection is gone, but unproven until the §4.6 test (incl. the
  mid-generation case) passes. Prose-only fallback specified. *Open question:* does the
  background-completion re-invocation ever disturb an open picker?

**Event/notes retention**
- Append-only tables grow unbounded. *Open question:* retention/compaction (e.g.
  `ttorch db archive` for events of `delivered`/`torn_down` tasks older than N days).

**Backlog→spawn & retained rows**
- `task add` then `spawn <id>` must `UpsertTask` (UPDATE), not INSERT, to avoid a PK collision
  (§3.4). Teardown retains the row as `torn_down` and blanks `worktree` on `Pool.Release` so
  history never aliases a reassigned worktree path.
