-- ======================= migration 0010 (up): the gate's base ========================
-- Records, in the store, the three things the trust gate used to derive from refs a worker
-- can write from its own worktree.
--
-- projects.default_branch is the branch the gate reads for this project. It was derived on
-- every gate run from refs/remotes/origin/HEAD, then a local main or master, and a worker can
-- repoint the first and create or move the other two. It is set when a project is registered,
-- and changed only by the lead (`ttorch project set-branch`).
--
-- projects.default_branch_seed tracks how a row got its branch: '' when it was recorded at
-- registration or by the lead, 'pending' for a row that existed before this migration and has
-- not been seeded yet, and 'notice' for a row the seed filled in whose branch has not been
-- shown to the lead yet. Rows created from here on start at ''; the UPDATE below marks the
-- existing ones. Seeding needs git, so it runs from Go on the next open, not in this SQL.
--
-- projects.last_landed_sha is the commit the most recent successful land left the default
-- branch at. A gate run warns when the branch no longer contains it.
--
-- verdicts.base_sha is the commit the reviewers' diff was staged against, so a merge can
-- refuse a verdict whose review started somewhere other than the branch it merges into.
--
-- review_preps holds the base each prep staged, keyed by task, for the record step to pin on
-- the verdict. It lives here rather than beside the review inputs because that directory is
-- the worker's to write. The database belongs to the same uid, so this is a cost, not a
-- boundary (see migration 0009).
--
-- Plain ADD COLUMNs with constant defaults and a new table: no CHECK constraint changes, so no
-- table rebuild and no foreign-key dance.
PRAGMA foreign_keys = ON;

ALTER TABLE projects ADD COLUMN default_branch      TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN default_branch_seed TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN last_landed_sha     TEXT NOT NULL DEFAULT '';
UPDATE projects SET default_branch_seed = 'pending';

ALTER TABLE verdicts ADD COLUMN base_sha TEXT NOT NULL DEFAULT '';

-- One row per task, replaced by each prep and removed with the task (CASCADE).
CREATE TABLE review_preps (
    task_id    TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    head       TEXT NOT NULL,   -- the commit the prep staged
    base_sha   TEXT NOT NULL,   -- the commit the reviewers' diff was staged against
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
