-- ========================= migration 0012 (up): the peer registry ==========================
-- What a parent coordinator keeps about the peers it provisioned. Every coordinator gets the
-- tables; only one that ran `ttorch peer add` has rows in them.
--
-- peers is one row per peer, keyed by the name the lead gave it. control_dest is the ssh
-- destination the parent reaches the peer's control channel at, with the key at control_key (a
-- private key under the parent's ttorch home, peers/<name>/). approve_dest is the destination
-- the lead uses for an interactive session to the same machine; provisioning ran there. A row
-- starts 'provisioning' and becomes 'live' once the control key has answered `version`.
-- 'unreachable' is for the root scheduler's poll; 'retired' keeps the row, and the delegations
-- that name it, after the peer is no longer used. protocol and version are what the peer last
-- reported. escalation_cursor, consecutive_failures and down_attempts belong to the scheduler's
-- poll and start at zero. summary is the last summary the parent read, for display only;
-- nothing decides on it. last_error is the last call's failure, escaped and capped.
--
-- peer_repos is what each peer owns: the repository's path on the peer and its origin URL, so
-- the parent can refuse to give one repository to two coordinators (design 3.5). It only sees
-- what this database knows.
--
-- peer_delegations is every task-add and goal the parent sent, under the request id it minted,
-- so after a restart it knows what it asked for without asking the peer. brief_sha256 is the
-- hex SHA-256 of the brief or the goal's text; remote_task_id is '' for a goal.
--
-- A peer row is never deleted (retiring keeps it), so the references need no ON DELETE. The
-- record of a delegation the peer refused outright is dropped, so the table lists only what may
-- have been delegated.
PRAGMA foreign_keys = ON;

CREATE TABLE peers (
    name                 TEXT PRIMARY KEY,
    control_dest         TEXT NOT NULL,
    approve_dest         TEXT NOT NULL,
    control_key          TEXT NOT NULL,
    status               TEXT NOT NULL
                            CHECK (status IN ('provisioning','live','unreachable','retired')),
    protocol             INTEGER NOT NULL DEFAULT 0,
    version              TEXT NOT NULL DEFAULT '',
    escalation_cursor    INTEGER NOT NULL DEFAULT 0,
    summary              TEXT NOT NULL DEFAULT '{}',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    down_attempts        INTEGER NOT NULL DEFAULT 0,
    last_ok_at           TEXT NULL,
    last_error           TEXT NOT NULL DEFAULT '',
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL
);

CREATE TABLE peer_repos (
    peer        TEXT NOT NULL REFERENCES peers(name),
    remote_path TEXT NOT NULL,
    origin_url  TEXT NOT NULL,
    PRIMARY KEY (peer, remote_path)
);

CREATE TABLE peer_delegations (
    request_id     TEXT PRIMARY KEY,
    peer           TEXT NOT NULL REFERENCES peers(name),
    kind           TEXT NOT NULL CHECK (kind IN ('task','goal')),
    remote_task_id TEXT NOT NULL DEFAULT '',
    brief_sha256   TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL
);

CREATE INDEX idx_peer_delegations_peer ON peer_delegations(peer, created_at);
