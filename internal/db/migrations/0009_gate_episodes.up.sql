-- ============================ migration 0009 (up) ============================
-- Moves the daemon gate's per-episode state out of the review-inputs directory and
-- into the store.
--
-- The record holds which dimensions an episode dispatched, how many launch attempts
-- each has cost, and when the episode opened. It lived beside the review inputs as a
-- file. Here it is a row in a database owned by the same uid the worker runs as, so a
-- process running as the lead can still rewrite it with a sqlite3 shell. The gate does
-- not rest a verdict on it for that reason: it folds the reports rather than this
-- row's dimensions, and cross-checks started_at against evidence the row does not hold.
PRAGMA foreign_keys = ON;

-- One row per task, replaced in place each tick and removed with the task (CASCADE, so
-- a teardown never strands an episode). A task with no row has no episode in flight.
CREATE TABLE gate_episodes (
    task_id             TEXT    PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    head                TEXT    NOT NULL,                -- the commit this episode gates
    dims                TEXT    NOT NULL DEFAULT '[]',   -- JSON array: the set decided at the boundary
    attempts            TEXT    NOT NULL DEFAULT '{}',   -- JSON object: dimension -> launch attempts charged
    started_at          TEXT    NOT NULL,                -- when the episode opened; the stall clock runs from here
    dispatched_at       TEXT    NOT NULL DEFAULT '',     -- first CHARGED dispatch; '' until one
    outcome             TEXT    NOT NULL DEFAULT ''      -- '' in flight | recorded | blocked
                            CHECK (outcome IN ('','recorded','blocked')),
    last_dispatch_error TEXT    NOT NULL DEFAULT '',     -- most recent charged launch failure
    created_at          TEXT    NOT NULL,
    updated_at          TEXT    NOT NULL
);
