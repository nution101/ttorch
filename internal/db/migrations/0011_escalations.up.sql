-- ===================== migration 0011 (up): coordinator, escalations ======================
-- The three tables a coordinator needs to hold decisions for a lead durably and, later, to
-- answer a parent coordinator. Every coordinator gets them, root or peer.
--
-- coordinator is this database's identity: one row, minted here with a random id. A root
-- keeps role 'root' and no parent; provisioning a peer rewrites role, name and parent_id.
--
-- escalations is what waits on the lead. A row is opened by `ttorch escalate`, or by the
-- approval sync for a done task whose approval_required event has had no human approval
-- since (source_event_id names that event). It is a table rather than a pattern over events
-- because "what is open now" has to survive a restart, and a status column is less to get
-- wrong than pairing append-only events.
--
-- The gate appends approval_required once per task, but a task can leave done and come back,
-- so the sync opens at most one approval escalation per done episode. episode_event_id is the
-- status event that put the task in done (0 when it got there without one), and the partial
-- UNIQUE index below makes (task, kind, episode) exactly-once. Hand-raised rows have no
-- episode and are not limited. answered_by records who wrote the answer down; the CLI records
-- 'manager', which relays the lead's words and cannot vouch for where they came from.
-- answer_request_id is UNIQUE: one request id answers at most one escalation.
--
-- peer_requests stores the result of each mutating request by its request id, so a retry
-- after a timeout replays the stored result instead of acting twice.
--
-- Body and answer text are capped at 2 KiB by the store before they get here.
PRAGMA foreign_keys = ON;

CREATE TABLE coordinator (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    coord_id   TEXT NOT NULL,             -- random id minted below
    role       TEXT NOT NULL CHECK (role IN ('root','peer')),
    name       TEXT NOT NULL DEFAULT '',
    parent_id  TEXT NOT NULL DEFAULT '',  -- coord_id of the parent that provisioned it
    created_at TEXT NOT NULL
);

INSERT INTO coordinator (id, coord_id, role, created_at)
VALUES (1, lower(hex(randomblob(16))), 'root', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

CREATE TABLE escalations (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id           TEXT NULL REFERENCES tasks(id) ON DELETE SET NULL,
    kind              TEXT NOT NULL CHECK (kind IN ('approval','question','gate_change')),
    body              TEXT NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('open','answered','resolved','withdrawn')),
    source_event_id   INTEGER NULL,         -- the approval_required event behind it, if any
    episode_event_id  INTEGER NULL,         -- the done episode an approval was opened for
    answer            TEXT NOT NULL DEFAULT '',
    answered_by       TEXT NOT NULL DEFAULT '',
    answer_request_id TEXT NULL UNIQUE,
    created_at        TEXT NOT NULL,
    resolved_at       TEXT NULL
);

CREATE INDEX idx_escalations_status ON escalations(status, id);
CREATE UNIQUE INDEX idx_escalations_episode ON escalations(task_id, kind, episode_event_id)
    WHERE episode_event_id IS NOT NULL;

CREATE TABLE peer_requests (
    request_id TEXT PRIMARY KEY,
    verb       TEXT NOT NULL,
    result     TEXT NOT NULL,
    created_at TEXT NOT NULL
);
