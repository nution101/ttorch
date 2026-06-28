-- ============================ migration 0002 (up) ============================
-- Durable adversarial-review verdicts (roadmap item 1). Promotes the trust gate's
-- authoritative verdict + approval token out of short-lived TTL'd files and into
-- SQLite, so the merge gate validates freshness from the DB row — content-pinned
-- (commit + diff), never time-pinned. A re-gate is then forced ONLY by a genuine
-- content change, never by file-TTL expiry.
PRAGMA foreign_keys = ON;

-- One row per task, replaced in place by `trust record` / carry-forward and removed
-- when a gated merge consumes it. It is keyed by the task it gates (CASCADE so a
-- task teardown drop never strands a verdict). Everything the trust gate needs to
-- validate freshness lives here: the pass/block outcome, the per-dimension findings,
-- the commit pin (reviewed_sha) and the content pin (diff_id), plus the approval
-- token's durable provenance (approved_by) and the commit it authorizes (approval_sha).
CREATE TABLE verdicts (
    task_id      TEXT    PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    overall      TEXT    NOT NULL                              -- pass | block
                    CHECK (overall IN ('pass','block')),
    reviewed_sha TEXT    NOT NULL,                             -- commit pin (the reviewed HEAD)
    diff_id      TEXT    NOT NULL DEFAULT '',                  -- content pin (review.DiffID); '' = pre-content-identity
    findings     TEXT    NOT NULL DEFAULT '[]',                -- JSON array of review.Finding
    approved_by  TEXT    NOT NULL DEFAULT ''                   -- '' | human | auto
                    CHECK (approved_by IN ('','human','auto')),
    approval_sha TEXT    NOT NULL DEFAULT '',                  -- commit the approval token authorizes
    created_at   TEXT    NOT NULL,
    updated_at   TEXT    NOT NULL
);
