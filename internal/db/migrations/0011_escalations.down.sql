-- ===================== migration 0011 (down): coordinator, escalations ====================
-- Schema-reversible only: drops the three tables 0011 created, with their data. Nothing else
-- references them.
PRAGMA foreign_keys = ON;
DROP TABLE IF EXISTS peer_requests;
DROP TABLE IF EXISTS escalations;
DROP TABLE IF EXISTS coordinator;
