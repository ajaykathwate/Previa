-- Live preview activity read from the router (docs/10-preview-runtime.md).

-- name: StartVisitCounter :exec
INSERT INTO preview_visit_counters (environment_id, service, requests)
SELECT e.id, sqlc.arg(service)::text, 0 FROM environments e WHERE e.id = sqlc.arg(environment_id)
ON CONFLICT (environment_id) DO NOTHING;

-- name: LockVisitCounter :one
SELECT service, requests FROM preview_visit_counters WHERE environment_id = $1 FOR UPDATE;

-- name: SaveVisitCounter :exec
UPDATE preview_visit_counters SET service = sqlc.arg(service)::text, requests = sqlc.arg(requests)::double precision, read_at = clock_timestamp()
WHERE environment_id = sqlc.arg(environment_id);

-- name: AddVisits :exec
UPDATE environments SET visits = visits + sqlc.arg(requests)::bigint, last_visited_at = clock_timestamp()
WHERE id = sqlc.arg(environment_id);
