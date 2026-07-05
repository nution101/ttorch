-- ==================== migration 0008 (down): drop auto-tiered flag ================
-- Schema-reversible only (§1.5): drops the auto_tiered column and destroys its data. The
-- column is referenced by no index or constraint, so a plain DROP COLUMN suffices
-- (SQLite >= 3.35) — no table rebuild and no foreign-key dance. Runs before 0003's down
-- rebuild, which copies an explicit column list that never named auto_tiered.
ALTER TABLE tasks DROP COLUMN auto_tiered;
