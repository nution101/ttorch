-- ==================== migration 0007 (down): drop per-task model ================
-- Schema-reversible only (§1.5): drops the model column and destroys its data. The
-- column is referenced by no index or constraint, so a plain DROP COLUMN suffices
-- (SQLite >= 3.35) — no table rebuild and no foreign-key dance. Runs before 0003's
-- down rebuild, which copies an explicit column list that never named model.
ALTER TABLE tasks DROP COLUMN model;
