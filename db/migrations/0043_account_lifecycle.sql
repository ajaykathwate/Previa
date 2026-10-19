-- +goose Up
ALTER TABLE users ADD COLUMN session_epoch bigint NOT NULL DEFAULT 0 CHECK (session_epoch >= 0);
ALTER TABLE users ADD COLUMN deleted_at timestamptz;
ALTER TABLE repos ADD COLUMN disconnected_at timestamptz;
ALTER TABLE installations ADD COLUMN disconnected_at timestamptz;

-- Product rows are owned by their repository/installation. External resource
-- cleanup is verified before the lifecycle service removes either parent.
-- +goose StatementBegin
DO $$ DECLARE c record; BEGIN
    FOR c IN SELECT conrelid::regclass AS relation,conname,pg_get_constraintdef(oid) AS definition
        FROM pg_constraint WHERE connamespace=current_schema()::regnamespace AND contype='f' AND conname=ANY(ARRAY[
        'repos_installation_id_fkey','pull_requests_repo_id_fkey','environments_pull_request_id_fkey',
        'build_logs_environment_id_fkey','env_secrets_repo_id_fkey','environment_operations_pull_request_id_fkey',
        'pull_request_reports_pull_request_id_fkey','pr_syncs_repo_id_fkey','pr_reconciliations_pull_request_id_fkey',
        'environment_routes_environment_id_fkey','environment_routes_pull_request_id_fkey','installation_repository_events_installation_id_fkey'])
    LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I',c.relation,c.conname);
        EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I %s ON DELETE CASCADE',c.relation,c.conname,c.definition);
    END LOOP;
END $$;
-- +goose StatementEnd

CREATE TABLE lifecycle_operations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind text NOT NULL CHECK (kind IN ('repository','installation','workspace','account')),
    target_id bigint NOT NULL,
    target_public_id uuid NOT NULL,
    workspace_id bigint,
    actor_id bigint NOT NULL REFERENCES users(id),
    parent_id uuid REFERENCES lifecycle_operations(id),
    stage text NOT NULL DEFAULT 'journal' CHECK (stage IN ('journal','cleanup','erase','complete')),
    last_error text NOT NULL DEFAULT '',
    available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    journal_ack_at timestamptz,
    receipt_hash bytea UNIQUE,
    receipt_expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz,
    UNIQUE(kind,target_public_id),
    CHECK ((stage='complete')=(completed_at IS NOT NULL)),
    CHECK (stage<>'complete' OR journal_ack_at IS NOT NULL)
);
CREATE INDEX lifecycle_due ON lifecycle_operations(available_at) WHERE stage<>'complete';

CREATE TABLE deletion_journal_metadata (
    singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
    instance uuid NOT NULL DEFAULT gen_random_uuid(),
    snapshot_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO deletion_journal_metadata DEFAULT VALUES;

CREATE TABLE github_disconnections (
    github_installation_id bigint NOT NULL,
    github_repo_id bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(github_installation_id,github_repo_id)
);

-- This journal is exported encrypted through the independent backup channel.
-- It contains operational identifiers, never deleted content or credentials.
CREATE TABLE deletion_journal (
    sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    action text NOT NULL CHECK (action IN ('repository','installation','workspace','account','membership','ownership')),
    target uuid NOT NULL,
    workspace uuid,
    github_installation_id bigint,
    github_repo_id bigint,
    github_user_id bigint,
    successor uuid,
    successor_github_id bigint,
    operation_id uuid REFERENCES lifecycle_operations(id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    acknowledged_at timestamptz
);

CREATE TABLE disconnect_comments (
    pull_request_id bigint PRIMARY KEY REFERENCES pull_requests(id) ON DELETE CASCADE,
    operation_id uuid NOT NULL REFERENCES lifecycle_operations(id),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 3),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','sent','unavailable')),
    available_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX deletion_journal_operation ON deletion_journal(operation_id);
CREATE INDEX deletion_journal_retention ON deletion_journal(created_at);

CREATE TABLE support_requests (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id bigint REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES users(id),
    category text NOT NULL CHECK (category IN ('build','access','data','abuse','other')),
    message text NOT NULL CHECK (octet_length(message) BETWEEN 1 AND 65536),
    diagnostic_reference uuid NOT NULL,
    repository_reference uuid,
    pull_request_reference uuid,
    state text NOT NULL DEFAULT 'open' CHECK (state IN ('open','resolved')),
    revision bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    resolved_at timestamptz,
    CHECK ((state='resolved')=(resolved_at IS NOT NULL))
);
CREATE INDEX support_requests_user ON support_requests(user_id,created_at);
CREATE INDEX support_requests_workspace ON support_requests(workspace_id,created_at);
-- Keep the submission allowance independent of message/workspace erasure.
CREATE TABLE support_daily_usage (
    user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day date NOT NULL,
    requests integer NOT NULL CHECK(requests>0),
    PRIMARY KEY(user_id,day)
);
CREATE TABLE operator_activity (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    github_id bigint NOT NULL,
    action text NOT NULL CHECK (action IN ('support_read','support_resolve','incident_create','incident_update','cleanup_read')),
    reference uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE service_incidents (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    component text NOT NULL CHECK (component IN ('builds','routing','control_plane')),
    summary text NOT NULL CHECK (octet_length(summary) BETWEEN 1 AND 2000),
    state text NOT NULL CHECK (state IN ('investigating','monitoring','resolved')),
    revision bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE service_availability (
    worker_id text NOT NULL,
    component text NOT NULL CHECK(component IN ('builds','routing')),
    state text NOT NULL CHECK(state IN ('available','unavailable')),
    checked_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(worker_id,component)
);

-- Membership revocations and ownership transfers also have to survive restore.
-- +goose StatementBegin
CREATE FUNCTION previa_journal_membership() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF current_setting('previa.journal_replay',true)='on' THEN RETURN NEW; END IF;
    IF OLD.state='active' AND NEW.state='removed' THEN
        -- A baseline requested manually by the departed member has no remaining
        -- authorization. Automatic/team refreshes are unaffected.
        UPDATE baseline_refreshes b SET state='cancelled',finished_at=clock_timestamp(),log_open=false
        FROM repos r,installations i WHERE b.requested_by=NEW.user_id AND b.trigger='manual'
        AND b.repo_id=r.id AND i.id=r.installation_id AND i.workspace_id=NEW.workspace_id
        AND b.state IN ('pending','running');
        INSERT INTO deletion_journal(action,target,workspace,github_user_id)
        SELECT 'membership',u.public_id,w.public_id,u.github_id
        FROM users u,workspaces w WHERE u.id=NEW.user_id AND w.id=NEW.workspace_id;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER previa_journal_membership AFTER UPDATE OF state ON workspace_memberships
FOR EACH ROW EXECUTE FUNCTION previa_journal_membership();

CREATE FUNCTION previa_journal_ownership() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF current_setting('previa.journal_replay',true)='on' THEN RETURN NEW; END IF;
    IF NEW.owner_user_id<>OLD.owner_user_id THEN
        INSERT INTO deletion_journal(action,target,successor,successor_github_id)
        SELECT 'ownership',NEW.public_id,u.public_id,u.github_id FROM users u WHERE u.id=NEW.owner_user_id;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER previa_journal_ownership AFTER UPDATE OF owner_user_id ON workspaces
FOR EACH ROW EXECUTE FUNCTION previa_journal_ownership();

-- A signed late webhook cannot undo a local disconnect, even through a
-- repository upsert or an installation lifecycle transition.
CREATE FUNCTION previa_disconnect_fence() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE installation bigint;
BEGIN
    IF TG_TABLE_NAME='installations' THEN
        IF NEW.disconnected_at IS NOT NULL OR EXISTS(SELECT 1 FROM github_disconnections WHERE github_repo_id=0 AND github_installation_id=NEW.github_installation_id) THEN
            IF TG_OP='INSERT' THEN RETURN NULL; END IF;
            NEW.disconnected_at:=coalesce(NEW.disconnected_at,clock_timestamp());
            NEW.status:='inactive';
        END IF;
    ELSE
        SELECT github_installation_id INTO installation FROM installations WHERE id=NEW.installation_id;
        IF NEW.disconnected_at IS NOT NULL OR EXISTS(SELECT 1 FROM github_disconnections WHERE
            github_installation_id=installation AND github_repo_id IN (0,NEW.github_repo_id)) THEN
            IF TG_OP='INSERT' THEN RETURN NULL; END IF;
            NEW.disconnected_at:=coalesce(NEW.disconnected_at,clock_timestamp());
            NEW.accessible:=false;
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER previa_disconnect_installation BEFORE INSERT OR UPDATE ON installations
FOR EACH ROW EXECUTE FUNCTION previa_disconnect_fence();
CREATE TRIGGER previa_disconnect_repository BEFORE INSERT OR UPDATE ON repos
FOR EACH ROW EXECUTE FUNCTION previa_disconnect_fence();

-- Serialize ownership creation/transfer with account deletion. A stale callback
-- cannot create a new workspace owned by an already deleted identity.
CREATE FUNCTION previa_live_owner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM id FROM users WHERE id=NEW.owner_user_id AND deleted_at IS NULL FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'workspace owner is unavailable' USING ERRCODE='23514'; END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER previa_live_workspace_owner BEFORE INSERT OR UPDATE OF owner_user_id ON workspaces
FOR EACH ROW EXECUTE FUNCTION previa_live_owner();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS(SELECT 1 FROM lifecycle_operations) OR EXISTS(SELECT 1 FROM deletion_journal)
       OR EXISTS(SELECT 1 FROM github_disconnections) OR EXISTS(SELECT 1 FROM users WHERE deleted_at IS NOT NULL) THEN
        RAISE EXCEPTION 'account lifecycle data exists: forward-fix; rollback would discard revocations';
    END IF;
END $$;
DO $$ DECLARE c record; BEGIN
    FOR c IN SELECT conrelid::regclass AS relation,conname,pg_get_constraintdef(oid) AS definition
        FROM pg_constraint WHERE connamespace=current_schema()::regnamespace AND contype='f' AND conname=ANY(ARRAY[
        'repos_installation_id_fkey','pull_requests_repo_id_fkey','environments_pull_request_id_fkey',
        'build_logs_environment_id_fkey','env_secrets_repo_id_fkey','environment_operations_pull_request_id_fkey',
        'pull_request_reports_pull_request_id_fkey','pr_syncs_repo_id_fkey','pr_reconciliations_pull_request_id_fkey',
        'environment_routes_environment_id_fkey','environment_routes_pull_request_id_fkey','installation_repository_events_installation_id_fkey'])
    LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I',c.relation,c.conname);
        EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I %s',c.relation,c.conname,replace(c.definition,' ON DELETE CASCADE',''));
    END LOOP;
END $$;
-- +goose StatementEnd
DROP TRIGGER previa_disconnect_repository ON repos;
DROP TRIGGER previa_live_workspace_owner ON workspaces;
DROP FUNCTION previa_live_owner();
DROP TRIGGER previa_disconnect_installation ON installations;
DROP FUNCTION previa_disconnect_fence();
DROP TRIGGER previa_journal_ownership ON workspaces;
DROP FUNCTION previa_journal_ownership();
DROP TRIGGER previa_journal_membership ON workspace_memberships;
DROP FUNCTION previa_journal_membership();
DROP TABLE service_availability,service_incidents,operator_activity,support_daily_usage,support_requests,disconnect_comments,deletion_journal,deletion_journal_metadata,github_disconnections,lifecycle_operations;
ALTER TABLE installations DROP COLUMN disconnected_at;
ALTER TABLE repos DROP COLUMN disconnected_at;
ALTER TABLE users DROP COLUMN deleted_at,DROP COLUMN session_epoch;
