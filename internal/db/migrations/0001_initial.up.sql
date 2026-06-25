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
