-- +goose Up
-- Fresh execution accounting starts at this migration. Historical elapsed-time
-- counters remain legacy technical data and are never imported or billed.
COMMENT ON TABLE account_build_usage IS 'Legacy technical elapsed-time counter; never billed or imported into workspace execution accounting';
COMMENT ON TABLE workspace_build_usage IS 'Legacy technical elapsed-time counter; never billed or imported into workspace execution accounting';
ALTER TABLE build_intents ADD COLUMN usage_wait_until timestamptz;
ALTER TABLE build_intents DROP CONSTRAINT build_intents_waiting_reason_check;
ALTER TABLE build_intents ADD CONSTRAINT build_intents_waiting_reason_check CHECK (waiting_reason IN
    ('capacity','disk','paused','account','quota','workspace_builds','workspace_minutes','workspace_daily','workspace_previews','workspace_restricted','workspace_accounting'));
ALTER TABLE baseline_refreshes DROP CONSTRAINT baseline_refreshes_waiting_reason_check;
ALTER TABLE baseline_refreshes ADD CONSTRAINT baseline_refreshes_waiting_reason_check CHECK (waiting_reason IN
    ('capacity_wait','quota_blocked','workspace_builds','workspace_minutes','workspace_daily','workspace_previews','workspace_restricted','workspace_accounting'));

CREATE TABLE free_plan_policy (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    period_kind text NOT NULL CHECK (period_kind IN ('day','month')),
    included_seconds bigint NOT NULL CHECK (included_seconds > 0),
    concurrent_builds integer NOT NULL CHECK (concurrent_builds > 0),
    preview_slots integer NOT NULL CHECK (preview_slots > 0),
    operator_daily_seconds bigint NOT NULL DEFAULT 0 CHECK (operator_daily_seconds >= 0),
    operator_concurrent_builds integer NOT NULL DEFAULT 0 CHECK (operator_concurrent_builds >= 0),
    operator_checked_at timestamptz,
    preview_lifetime_seconds bigint CHECK (preview_lifetime_seconds >= 0),
    preview_idle_seconds bigint CHECK (preview_idle_seconds >= 0),
    log_retention_seconds bigint CHECK (log_retention_seconds > 0),
    cutover_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
-- Owner delegated the allowance choice and confirmed no hosted users on
-- 18 October 2026: fresh free allowances, no historical usage import.
INSERT INTO free_plan_policy(period_kind,included_seconds,concurrent_builds,preview_slots)
VALUES ('month',18000,1,2);

CREATE TABLE workspace_entitlements (
    workspace_id bigint PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    plan_key text NOT NULL DEFAULT 'free' CHECK (plan_key = 'free'),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    period_kind text NOT NULL CHECK (period_kind IN ('day','month')),
    included_seconds bigint NOT NULL CHECK (included_seconds > 0),
    concurrent_builds integer NOT NULL CHECK (concurrent_builds > 0),
    preview_slots integer NOT NULL CHECK (preview_slots > 0),
    compute_restricted boolean NOT NULL DEFAULT false,
    source text NOT NULL DEFAULT 'free_early_access' CHECK (source = 'free_early_access'),
    verified_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- +goose StatementBegin
CREATE FUNCTION previa_free_entitlement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO workspace_entitlements(workspace_id,period_kind,included_seconds,concurrent_builds,preview_slots)
    SELECT NEW.id,period_kind,included_seconds,concurrent_builds,preview_slots FROM free_plan_policy;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER previa_free_entitlement AFTER INSERT ON workspaces FOR EACH ROW EXECUTE FUNCTION previa_free_entitlement();
INSERT INTO workspace_entitlements(workspace_id,period_kind,included_seconds,concurrent_builds,preview_slots)
SELECT w.id,p.period_kind,p.included_seconds,p.concurrent_builds,p.preview_slots
FROM workspaces w CROSS JOIN free_plan_policy p WHERE w.state='active';

CREATE TABLE workspace_usage_periods (
    workspace_id bigint NOT NULL REFERENCES workspace_entitlements(workspace_id) ON DELETE CASCADE,
    starts_at timestamptz NOT NULL,
    ends_at timestamptz NOT NULL CHECK (ends_at > starts_at),
    consumed_us bigint NOT NULL DEFAULT 0 CHECK (consumed_us >= 0),
    measured_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(workspace_id,starts_at)
);

CREATE TABLE workspace_usage_attempts (
    attempt_key uuid PRIMARY KEY,
    workspace_id bigint NOT NULL REFERENCES workspace_entitlements(workspace_id) ON DELETE CASCADE,
    repo_id bigint REFERENCES repos(id) ON DELETE SET NULL,
    -- Captured target identities survive PR/refresh history pruning. The
    -- workspace owns this ledger; deleting a target must not erase usage.
    pull_request_id bigint,
    baseline_refresh_id bigint,
    generation bigint NOT NULL DEFAULT 0,
    revision bigint NOT NULL DEFAULT 0,
    state text NOT NULL DEFAULT 'reserved' CHECK (state IN ('reserved','running','finished','reconciled')),
    outcome text NOT NULL DEFAULT '' CHECK (outcome IN ('','succeeded','failed','interrupted','limit','crash')),
    provisional boolean NOT NULL DEFAULT false,
    admitted_at timestamptz NOT NULL,
    started_at timestamptz,
    finished_at timestamptz,
    CHECK ((pull_request_id IS NOT NULL) <> (baseline_refresh_id IS NOT NULL))
);
CREATE INDEX workspace_usage_attempts_workspace ON workspace_usage_attempts(workspace_id,admitted_at);
CREATE INDEX workspace_usage_attempts_pr ON workspace_usage_attempts(pull_request_id,revision);

CREATE TABLE workspace_usage_segments (
    attempt_key uuid NOT NULL REFERENCES workspace_usage_attempts(attempt_key) ON DELETE CASCADE,
    sequence integer NOT NULL CHECK (sequence > 0),
    workspace_id bigint NOT NULL,
    period_start timestamptz NOT NULL,
    day date NOT NULL,
    starts_at timestamptz NOT NULL,
    reserved_until timestamptz NOT NULL,
    ended_at timestamptz,
    consumed_us bigint NOT NULL DEFAULT 0 CHECK (consumed_us >= 0),
    PRIMARY KEY(attempt_key,sequence),
    FOREIGN KEY(workspace_id,period_start) REFERENCES workspace_usage_periods(workspace_id,starts_at) ON DELETE CASCADE,
    CHECK (reserved_until >= starts_at AND reserved_until <= starts_at + interval '60 seconds'),
    CHECK (ended_at IS NULL OR (ended_at >= starts_at AND ended_at <= reserved_until)),
    CHECK (consumed_us <= extract(epoch FROM reserved_until-starts_at)*1000000)
);
CREATE INDEX workspace_usage_open_segments ON workspace_usage_segments(reserved_until) WHERE ended_at IS NULL;
CREATE INDEX workspace_usage_day ON workspace_usage_segments(workspace_id,day);

-- +goose Down
UPDATE build_intents SET waiting_revision=NULL,waiting_reason=NULL,waiting_since=NULL,waiting_checked_at=NULL WHERE waiting_reason LIKE 'workspace_%';
UPDATE baseline_refreshes SET waiting_reason=NULL WHERE waiting_reason LIKE 'workspace_%';
ALTER TABLE build_intents DROP COLUMN usage_wait_until;
ALTER TABLE build_intents DROP CONSTRAINT build_intents_waiting_reason_check;
ALTER TABLE build_intents ADD CONSTRAINT build_intents_waiting_reason_check CHECK (waiting_reason IN ('capacity','disk','paused','account','quota'));
ALTER TABLE baseline_refreshes DROP CONSTRAINT baseline_refreshes_waiting_reason_check;
ALTER TABLE baseline_refreshes ADD CONSTRAINT baseline_refreshes_waiting_reason_check CHECK (waiting_reason IN ('capacity_wait','quota_blocked'));
DROP TABLE workspace_usage_segments,workspace_usage_attempts,workspace_usage_periods;
DROP TRIGGER previa_free_entitlement ON workspaces;
DROP FUNCTION previa_free_entitlement();
DROP TABLE workspace_entitlements,free_plan_policy;
COMMENT ON TABLE account_build_usage IS NULL;
COMMENT ON TABLE workspace_build_usage IS NULL;
