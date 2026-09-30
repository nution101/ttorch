-- ====================== migration 0010 (down): drop the gate's base ======================
-- Schema-reversible only (§1.5): drops the review_preps table and the four columns 0010
-- added, destroying their data. No index or constraint references the columns, so a plain
-- DROP COLUMN suffices (SQLite >= 3.35). Runs before 0002's down, which drops verdicts whole.
PRAGMA foreign_keys = ON;
DROP TABLE IF EXISTS review_preps;
ALTER TABLE verdicts DROP COLUMN base_sha;
ALTER TABLE projects DROP COLUMN last_landed_sha;
ALTER TABLE projects DROP COLUMN default_branch_seed;
ALTER TABLE projects DROP COLUMN default_branch;
