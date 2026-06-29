-- ==================== migration 0006 (up): daemon observability =================
-- A single, durable row recording the dispatch daemon's liveness + cumulative work, so an
-- idle-but-HEALTHY daemon is distinguishable from a WEDGED one (the scheduler was previously a
-- black box: an idle daemon logged nothing, swallowed errors were invisible, and there was no
-- heartbeat the manager's watchdog could read to tell a DAEMON stall from a MANAGER stall). The
-- daemon upserts this row once per tick: last_tick_at advances every tick (even a no-op idle
-- tick — that is what proves liveness), tick_count counts ticks, and the counters ACCUMULATE the
-- work each pass did (dispatched / landed / gated / recovered, the load-backpressure-deferred
-- count, and swallowed errors). last_error[_at] surface the most recent handled/swallowed error
-- so its occurrence is no longer invisible. `ttorch scheduler status` reads this row (plus the
-- live board and the singleton lock) so a human or the watchdog can see "daemon last ticked Ns
-- ago, queue depth N, M active, last error …".
--
-- The row is a SINGLETON: id is pinned to 1 by a CHECK, so the once-per-tick upsert is exactly
-- one small write on the single-writer DB (no per-task/per-pair writes). A plain CREATE TABLE
-- with constant defaults: it adds a standalone table referenced by no index or foreign key, so —
-- like 0004/0005 — it needs no table rebuild and no foreign-key dance.
CREATE TABLE scheduler_status (
    id            INTEGER PRIMARY KEY CHECK (id = 1), -- the one singleton row
    last_tick_at  TEXT    NOT NULL,                   -- when the daemon last completed a tick (the liveness signal)
    tick_count    INTEGER NOT NULL DEFAULT 0,         -- ticks completed (cumulative)
    dispatched    INTEGER NOT NULL DEFAULT 0,         -- tasks dispatched (cumulative)
    landed        INTEGER NOT NULL DEFAULT 0,         -- tasks landed (cumulative)
    gated         INTEGER NOT NULL DEFAULT 0,         -- verdicts recorded by the gate pass (cumulative)
    recovered     INTEGER NOT NULL DEFAULT 0,         -- dead workers reclaimed for retry (cumulative)
    deferred      INTEGER NOT NULL DEFAULT 0,         -- dispatches deferred by load backpressure (cumulative; populated by sibling task H4)
    errors        INTEGER NOT NULL DEFAULT 0,         -- handled/swallowed pass errors (cumulative)
    last_error    TEXT    NOT NULL DEFAULT '',        -- most recent swallowed error text ('' = none yet)
    last_error_at TEXT    NOT NULL DEFAULT '',        -- when last_error was recorded ('' = none yet)
    updated_at    TEXT    NOT NULL
);
