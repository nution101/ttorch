-- ======================== migration 0012 (down): the peer registry =========================
-- Schema-reversible only: drops the three tables 0012 created, with their data, children first.
PRAGMA foreign_keys = ON;
DROP TABLE IF EXISTS peer_delegations;
DROP TABLE IF EXISTS peer_repos;
DROP TABLE IF EXISTS peers;
