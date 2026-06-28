-- ==================== migration 0004 (up): per-task effort ====================
-- Records the reasoning-effort level a worker was dispatched at (low|medium|high|
-- xhigh|max|ultracode|off; '' = unset → the launch path falls back to TTORCH_EFFORT /
-- the kind default), so the effort survives a stop/update/restart and ResumeCommand can
-- restore it. A plain ADD COLUMN with a constant default: it changes no CHECK
-- constraint, so — unlike 0003 — it needs no table rebuild and no foreign-key dance.
ALTER TABLE tasks ADD COLUMN effort TEXT NOT NULL DEFAULT '';
