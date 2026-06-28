-- ttorch:rebuild-table  (the runner applies this with foreign_keys disabled — see migrate.go)
-- ===================== migration 0003 (up): task leases + retry =====================
-- Durable lease + retry bookkeeping on the tasks table (roadmap item 2). A dispatched
-- task carries an owner and a lease with an expiry; the worker's progress path
-- (ReportStatus/SetStage) extends the lease as a heartbeat. A lease that has GENUINELY
-- expired is verifiable ground truth that the worker is gone — unlike pane-output
-- inference — so the task can be reclaimed back to a re-dispatchable state. retry_count
-- is bounded by max_retries (poison-pill: a repeatedly-failing task lands in the terminal
-- 'failed' status instead of being re-dispatched forever).
--
-- SQLite cannot ALTER a CHECK constraint in place, so adding the terminal 'failed' status
-- to tasks.status (and the new lease/retry columns) requires REBUILDING the table:
-- create tasks_new, copy, drop tasks, rename. The marker on line 1 makes the migration
-- runner apply this with foreign-key ENFORCEMENT disabled, because the DROP would
-- otherwise fire ON DELETE CASCADE on dependent rows (verdicts, notes) and silently
-- destroy them; the runner re-validates referential integrity with foreign_key_check
-- before re-enabling enforcement. (SQLite "Making Other Kinds Of Table Schema Changes".)

CREATE TABLE tasks_new (
    id             TEXT    PRIMARY KEY,
    project_id     INTEGER NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    epic_id        INTEGER NULL     REFERENCES epics(id)  ON DELETE SET NULL,
    phase_id       INTEGER NULL     REFERENCES phases(id) ON DELETE SET NULL,
    -- Self-reference uses the table's CURRENT (build-time) name; the RENAME below
    -- rewrites it to 'tasks', matching the 0001 definition.
    parent_task_id TEXT    NULL     REFERENCES tasks_new(id) ON DELETE SET NULL,
    created_by     TEXT    NOT NULL DEFAULT 'manager',

    title          TEXT    NOT NULL DEFAULT '',
    kind           TEXT    NOT NULL DEFAULT 'ship'
                      CHECK (kind IN ('ship','scout','cc')),
    -- Terminal 'failed' added to the 0001 set (the poison-pill state for a task that has
    -- exhausted its retries). This is the only constraint change versus 0001.
    status         TEXT    NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending','active','needs_input','blocked',
                                        'done','delivered','torn_down','abandoned','failed')),
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
    last_progress_at  TEXT NULL,

    -- lease + retry bookkeeping (roadmap item 2).
    lease_owner       TEXT    NOT NULL DEFAULT '',   -- worker:<id> holding the lease; '' = none
    lease_expires_at  TEXT    NULL,                  -- RFC3339Nano expiry; NULL = no active lease
    retry_count       INTEGER NOT NULL DEFAULT 0,    -- reclaims so far
    max_retries       INTEGER NOT NULL DEFAULT 3,    -- ceiling (= db.DefaultMaxRetries); retry_count >= ⇒ poison-pill
    attempt           INTEGER NOT NULL DEFAULT 0     -- dispatches so far (bumped when a lease is granted)
);

-- Copy every 0001/0002 column verbatim; the five new columns take their defaults
-- (no lease, retry_count 0, max_retries 3, attempt 0) for pre-existing rows.
INSERT INTO tasks_new (
    id, project_id, epic_id, phase_id, parent_task_id, created_by,
    title, kind, status, stage, owner,
    window, worktree, harness, session_id, pr, gate_passed, approved_by, reviewed_sha, footprint,
    last_pane_hash, idle_sweeps, created_at, updated_at, last_progress_at)
SELECT
    id, project_id, epic_id, phase_id, parent_task_id, created_by,
    title, kind, status, stage, owner,
    window, worktree, harness, session_id, pr, gate_passed, approved_by, reviewed_sha, footprint,
    last_pane_hash, idle_sweeps, created_at, updated_at, last_progress_at
FROM tasks;

DROP TABLE tasks;
ALTER TABLE tasks_new RENAME TO tasks;

-- Recreate the 0001 indexes (dropped with the old table).
CREATE INDEX idx_tasks_status  ON tasks(status);
CREATE INDEX idx_tasks_project ON tasks(project_id);
CREATE INDEX idx_tasks_owner   ON tasks(owner);
CREATE INDEX idx_tasks_parent  ON tasks(parent_task_id);
-- Reclaim hot path: scan leased tasks whose lease has expired. Partial so it indexes
-- only the few rows that actually hold a lease.
CREATE INDEX idx_tasks_lease   ON tasks(lease_expires_at) WHERE lease_expires_at IS NOT NULL;
