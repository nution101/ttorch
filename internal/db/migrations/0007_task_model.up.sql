-- ==================== migration 0007 (up): per-task model ====================
-- Records the model a worker was dispatched on (a claude alias like haiku|sonnet|opus|
-- fable|opusplan, or a full model id; '' = unset → the launch path passes no --model and
-- claude uses its own default), so the model survives a stop/update/restart and
-- ResumeCommand can restore it. It is orthogonal to the effort column added in 0004: model
-- is WHICH brain, effort is HOW HARD it thinks. A plain ADD COLUMN with a constant default:
-- it changes no CHECK constraint, so — like 0004 — it needs no table rebuild and no
-- foreign-key dance.
ALTER TABLE tasks ADD COLUMN model TEXT NOT NULL DEFAULT '';
