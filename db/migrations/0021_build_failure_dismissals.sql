-- +goose Up
-- A failed build request needs attention on the Overview only while it is
-- recent. An owner can also dismiss one failed attempt ("not relevant") so it
-- stops counting; the row names the request revision it dismissed, so a later
-- failure of the same pull request (a new revision) needs attention again.
-- Builds still lists every failure. Undo deletes the row.
CREATE TABLE build_failure_dismissals (
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    pull_request_id BIGINT NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    revision BIGINT NOT NULL CHECK (revision > 0),
    dismissed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id, pull_request_id)
);

-- +goose Down
DROP TABLE build_failure_dismissals;
