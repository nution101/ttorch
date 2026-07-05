-- ==================== migration 0008 (up): auto-tiered flag ====================
-- Records whether a task's model+effort were assigned by the dispatch-time complexity
-- classifier (1) rather than pinned by the user/env (0). The dispatch path re-derives the
-- tier for an auto-tiered task on every retry, escalating the model up the ladder
-- (sonnet -> opus -> fable) as retry_count climbs — the escalation-on-failure safety net
-- for a mis-classified small-but-hard task. A user/env pin stays 0 and is never escalated,
-- preserving the "explicit wins" invariant. A plain ADD COLUMN with a constant default:
-- it changes no CHECK constraint, so — like 0004/0007 — it needs no table rebuild.
ALTER TABLE tasks ADD COLUMN auto_tiered INTEGER NOT NULL DEFAULT 0;
