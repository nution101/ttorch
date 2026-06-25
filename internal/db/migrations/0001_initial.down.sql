-- =========================== migration 0001 (down) ===========================
-- Schema-reversible only: this DROPs every table and destroys data (§1.5). The
-- data-rollback path is the preserved state.migrated/ directory (§2.5), not this.
--
-- Drops are child-first so foreign_keys=ON does not reject a parent still
-- referenced by a child (verified error 1811 otherwise, §1.5).
DROP TABLE IF EXISTS notes;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS phases;
DROP TABLE IF EXISTS epics;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS manager;
DROP TABLE IF EXISTS schema_migrations;
