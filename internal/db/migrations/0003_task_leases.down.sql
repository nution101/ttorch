-- ttorch:rebuild-table  (the runner applies this with foreign_keys disabled — see migrate.go)
-- ==================== migration 0003 (down): drop leases + retry ====================
-- Schema-reversible only (§1.5): rebuilds the tasks table back to the 0001 shape,
-- DROPPING the lease/retry columns and their data. Like the up half this is a table
-- rebuild, so the marker on line 1 makes the runner disable foreign-key enforcement
-- around it (otherwise the DROP cascades into verdicts/notes) and re-check integrity
-- afterward. A task in the terminal 'failed' status (absent from the 0001 CHECK) is
-- mapped to 'abandoned' so the copy satisfies the restored constraint.

CREATE TABLE tasks_old (
    id             TEXT    PRIMARY KEY,
    project_id     INTEGER NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    epic_id        INTEGER NULL     REFERENCES epics(id)  ON DELETE SET NULL,
    phase_id       INTEGER NULL     REFERENCES phases(id) ON DELETE SET NULL,
    parent_task_id TEXT    NULL     REFERENCES tasks_old(id) ON DELETE SET NULL,
    created_by     TEXT    NOT NULL DEFAULT 'manager',

    title          TEXT    NOT NULL DEFAULT '',
    kind           TEXT    NOT NULL DEFAULT 'ship'
                      CHECK (kind IN ('ship','scout','cc')),
    status         TEXT    NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending','active','needs_input','blocked',
                                        'done','delivered','torn_down','abandoned')),
    stage          TEXT    NOT NULL DEFAULT '',
    owner          TEXT    NOT NULL DEFAULT '',

    window         TEXT    NOT NULL DEFAULT '',
    worktree       TEXT    NOT NULL DEFAULT '',
    harness        TEXT    NOT NULL DEFAULT '',
    session_id     TEXT    NOT NULL DEFAULT '',
    pr             TEXT    NOT NULL DEFAULT '',
    gate_passed    INTEGER NOT NULL DEFAULT 0,
    approved_by    TEXT    NOT NULL DEFAULT ''
                      CHECK (approved_by IN ('','human','auto')),
    reviewed_sha   TEXT    NOT NULL DEFAULT '',
    footprint      TEXT    NOT NULL DEFAULT '[]',

    last_pane_hash TEXT    NOT NULL DEFAULT '',
    idle_sweeps    INTEGER NOT NULL DEFAULT 0,

    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    last_progress_at  TEXT NULL
);

INSERT INTO tasks_old (
    id, project_id, epic_id, phase_id, parent_task_id, created_by,
    title, kind, status, stage, owner,
    window, worktree, harness, session_id, pr, gate_passed, approved_by, reviewed_sha, footprint,
    last_pane_hash, idle_sweeps, created_at, updated_at, last_progress_at)
SELECT
    id, project_id, epic_id, phase_id, parent_task_id, created_by,
    title, kind,
    CASE WHEN status = 'failed' THEN 'abandoned' ELSE status END,
    stage, owner,
    window, worktree, harness, session_id, pr, gate_passed, approved_by, reviewed_sha, footprint,
    last_pane_hash, idle_sweeps, created_at, updated_at, last_progress_at
FROM tasks;

DROP TABLE tasks;
ALTER TABLE tasks_old RENAME TO tasks;

CREATE INDEX idx_tasks_status  ON tasks(status);
CREATE INDEX idx_tasks_project ON tasks(project_id);
CREATE INDEX idx_tasks_owner   ON tasks(owner);
CREATE INDEX idx_tasks_parent  ON tasks(parent_task_id);
