-- +goose Up
-- Fair sharing of one Previa server between accounts (installation owners):
-- at most BUILDS_PER_ACCOUNT running builds each, turns between accounts when
-- builds wait for a slot, and an optional daily budget of build minutes.
--
-- waiting_since: when the current request began waiting (kept while it keeps
-- waiting for the same revision), so the longest-waiting account goes next.
-- waiting_checked_at: the last time the waiting request looked again; a
-- request that stopped looking (superseded, lost) no longer holds a turn.
ALTER TABLE build_intents ADD COLUMN waiting_since TIMESTAMPTZ;
ALTER TABLE build_intents ADD COLUMN waiting_checked_at TIMESTAMPTZ;
ALTER TABLE build_intents DROP CONSTRAINT build_intents_waiting_reason_check;
ALTER TABLE build_intents ADD CONSTRAINT build_intents_waiting_reason_check
    CHECK (waiting_reason = ANY (ARRAY['capacity', 'disk', 'paused', 'account', 'quota']));

-- Build time per account and UTC day, for the daily budget and for operators.
CREATE TABLE account_build_usage (
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day DATE NOT NULL,
    build_seconds BIGINT NOT NULL DEFAULT 0 CHECK (build_seconds >= 0),
    builds INTEGER NOT NULL DEFAULT 0 CHECK (builds >= 0),
    PRIMARY KEY (user_id, day)
);

-- +goose Down
DROP TABLE account_build_usage;
UPDATE build_intents SET waiting_revision = NULL, waiting_reason = NULL WHERE waiting_reason IN ('account', 'quota');
ALTER TABLE build_intents DROP CONSTRAINT build_intents_waiting_reason_check;
ALTER TABLE build_intents ADD CONSTRAINT build_intents_waiting_reason_check
    CHECK (waiting_reason = ANY (ARRAY['capacity', 'disk', 'paused']));
ALTER TABLE build_intents DROP COLUMN waiting_checked_at;
ALTER TABLE build_intents DROP COLUMN waiting_since;
