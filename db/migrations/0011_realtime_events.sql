-- +goose Up
-- Committed changes to repository-scoped state wake live dashboard pages.
-- NOTIFY is transactional: listeners hear nothing for rolled-back work, and
-- identical payloads raised within one transaction are delivered once. Payloads
-- carry only a topic and source table; browsers refetch through ordinary,
-- ownership-checked GET handlers, so no repository data travels in the event.

-- +goose StatementBegin
CREATE FUNCTION previa_notify_repository(target_repo_id bigint, source text) RETURNS void
LANGUAGE sql AS $$
    SELECT pg_notify('previa_events',
        json_build_object('topic', 'repository:' || target_repo_id, 'source', source)::text)
    WHERE target_repo_id IS NOT NULL;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION previa_repository_changed() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    changed record;
    repo bigint;
BEGIN
    IF TG_OP = 'UPDATE' AND OLD IS NOT DISTINCT FROM NEW THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'DELETE' THEN
        changed := OLD;
    ELSE
        changed := NEW;
    END IF;
    CASE TG_TABLE_NAME
        WHEN 'repos' THEN
            repo := changed.id;
        WHEN 'pull_requests', 'pr_syncs' THEN
            repo := changed.repo_id;
        WHEN 'build_intents', 'environments' THEN
            SELECT p.repo_id INTO repo FROM pull_requests p WHERE p.id = changed.pull_request_id;
    END CASE;
    PERFORM previa_notify_repository(repo, TG_TABLE_NAME);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION previa_installation_changed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM previa_notify_repository(r.id, TG_TABLE_NAME)
    FROM repos r WHERE r.installation_id = NEW.id;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER repos_realtime AFTER INSERT OR UPDATE OR DELETE ON repos
    FOR EACH ROW EXECUTE FUNCTION previa_repository_changed();
CREATE TRIGGER pull_requests_realtime AFTER INSERT OR UPDATE OR DELETE ON pull_requests
    FOR EACH ROW EXECUTE FUNCTION previa_repository_changed();
CREATE TRIGGER pr_syncs_realtime AFTER INSERT OR UPDATE OR DELETE ON pr_syncs
    FOR EACH ROW EXECUTE FUNCTION previa_repository_changed();
CREATE TRIGGER build_intents_realtime AFTER INSERT OR UPDATE OR DELETE ON build_intents
    FOR EACH ROW EXECUTE FUNCTION previa_repository_changed();
CREATE TRIGGER environments_realtime AFTER INSERT OR UPDATE OR DELETE ON environments
    FOR EACH ROW EXECUTE FUNCTION previa_repository_changed();
-- Installation status decides whether every repository in it is available.
CREATE TRIGGER installations_realtime AFTER UPDATE ON installations
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION previa_installation_changed();

-- +goose Down
DROP TRIGGER installations_realtime ON installations;
DROP TRIGGER environments_realtime ON environments;
DROP TRIGGER build_intents_realtime ON build_intents;
DROP TRIGGER pr_syncs_realtime ON pr_syncs;
DROP TRIGGER pull_requests_realtime ON pull_requests;
DROP TRIGGER repos_realtime ON repos;
DROP FUNCTION previa_installation_changed();
DROP FUNCTION previa_repository_changed();
DROP FUNCTION previa_notify_repository(bigint, text);
