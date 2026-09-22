-- ============================ migration 0009 (up) ============================
-- Moves the daemon gate's per-episode state out of the review-inputs directory and
-- into the store.
--
-- The record decides what a gate episode must still wait for: which dimensions it
-- dispatched, how many launch attempts each has cost, and when the episode opened,
-- which is what bounds it. It lived beside the review inputs, in the one directory
-- the gate explicitly does not vouch for, so a worker could rewrite it. A parseable
-- record naming no dimensions erased the memory that a security reviewer had been
-- dispatched and let an already-pinned critical finding fall out of the fold; zeroing
-- the start time held the stall bound open indefinitely; making the file unwritable
-- stopped the bound accumulating at all. Validating the file harder was the wrong
-- answer, since each shape needed its own check. This is the same move the reviewer's
-- cwd already made for the same reason.
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
