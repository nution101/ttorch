-- ==================== migration 0004 (down): drop per-task effort ================
-- Schema-reversible only (§1.5): drops the effort column and destroys its data. The
-- column is referenced by no index or constraint, so a plain DROP COLUMN suffices
-- (SQLite >= 3.35) — no table rebuild and no foreign-key dance. Runs before 0003's
-- down rebuild, which copies an explicit column list that never named effort.
ALTER TABLE tasks DROP COLUMN effort;
