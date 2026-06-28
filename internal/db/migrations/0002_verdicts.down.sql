-- =========================== migration 0002 (down) ===========================
-- Schema-reversible only: drops the verdicts table and destroys its data (§1.5).
-- verdicts is a child of tasks (FK), and the reverse-order migrate-down runs this
-- before 0001 drops tasks, so foreign_keys=ON never rejects the parent drop.
DROP TABLE IF EXISTS verdicts;
