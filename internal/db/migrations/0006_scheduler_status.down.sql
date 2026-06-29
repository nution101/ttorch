-- ==================== migration 0006 (down): drop daemon observability ===========
-- Schema-reversible only (§1.5): drops the scheduler_status table and its single row. The table
-- is referenced by no index, foreign key, or other table, so a plain DROP TABLE reverses the up
-- migration cleanly — no table rebuild and no foreign-key dance.
DROP TABLE scheduler_status;
