-- ================= migration 0013 (down): the escalations a parent has raised ================
-- Schema-reversible only: drops the ledger and the two episode flags, with their data. DROP
-- COLUMN suffices (SQLite >= 3.35).
PRAGMA foreign_keys = ON;
DROP TABLE IF EXISTS peer_open_escalations;
ALTER TABLE peers DROP COLUMN bad_ids_open;
ALTER TABLE peers DROP COLUMN cursor_reset_open;
