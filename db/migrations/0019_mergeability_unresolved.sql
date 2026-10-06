-- +goose Up
-- When GitHub never reports whether a pull request can merge, the bounded
-- mergeability check gives up (pullrequests.MergeabilityWorker, eight attempts).
-- This records that it gave up, for that exact observation, so the dashboard
-- can say so and offer Sync Pull Requests instead of showing "Checking" forever.
-- Any new observation or new check clears it.
ALTER TABLE pull_requests ADD COLUMN mergeability_unresolved_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE pull_requests DROP COLUMN mergeability_unresolved_at;
