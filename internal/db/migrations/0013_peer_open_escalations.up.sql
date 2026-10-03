-- ================= migration 0013 (up): the escalations a parent has raised ==================
-- What the root scheduler's peer pass needs to tell a peer whose escalation ids went backwards
-- from one that is simply quiet.
--
-- peer_open_escalations is every escalation open on a peer that this coordinator has raised as a
-- peer_escalation event, keyed by the peer's escalation id. created_at is the peer's own creation
-- stamp for it, as the peer reported it; with the id it names one escalation, so a recreated or
-- restored store that numbers a different escalation with an id already raised is not mistaken
-- for it. event_id is the event that raised it here. A row is dropped once the escalation is no
-- longer open on the peer (escalations never reopen), so the table holds what is open now, not
-- the history; the events table is the history.
--
-- peers.cursor_reset_open is 1 while the peer's ids have gone back and a peer_cursor_reset event
-- has been raised for it; the first poll that finds them consistent clears it, so each episode
-- raises one event. peers.bad_ids_open is the same for escalation ids out of range
-- (peer_protocol_error).
PRAGMA foreign_keys = ON;

CREATE TABLE peer_open_escalations (
    peer          TEXT    NOT NULL REFERENCES peers(name),
    escalation_id INTEGER NOT NULL,
    created_at    TEXT    NOT NULL,
    event_id      INTEGER NOT NULL,
    PRIMARY KEY (peer, escalation_id)
);

ALTER TABLE peers ADD COLUMN cursor_reset_open INTEGER NOT NULL DEFAULT 0;
ALTER TABLE peers ADD COLUMN bad_ids_open INTEGER NOT NULL DEFAULT 0;
