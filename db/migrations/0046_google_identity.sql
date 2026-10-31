-- +goose Up
-- Provider identity is optional; workspace roles remain independent of login.
ALTER TABLE users ALTER COLUMN github_id DROP NOT NULL;
ALTER TABLE users ADD COLUMN google_subject text UNIQUE CHECK (google_subject IS NULL OR length(google_subject) BETWEEN 1 AND 255);
ALTER TABLE users ADD COLUMN google_email text NOT NULL DEFAULT '';
ALTER TABLE workspace_invitations ALTER COLUMN github_id DROP NOT NULL;
ALTER TABLE workspace_invitations ADD COLUMN email text NOT NULL DEFAULT '';
ALTER TABLE workspace_invitations ADD CONSTRAINT invitation_recipient CHECK ((github_id IS NOT NULL) <> (email <> ''));
ALTER TABLE workspace_invitations ADD CONSTRAINT email_invitation_reviewer CHECK (email='' OR role='reviewer');
CREATE UNIQUE INDEX workspace_pending_email_invitation ON workspace_invitations(workspace_id,email) WHERE state='pending' AND email<>'';
ALTER TABLE deletion_journal ADD COLUMN google_subject text;

-- +goose StatementBegin
CREATE FUNCTION workspace_invitation_matches(invitation bigint, actor bigint, github bigint) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM workspace_invitations v WHERE v.id=invitation AND
 ((actor=0 AND github>0 AND v.github_id=github) OR EXISTS(SELECT 1 FROM users u WHERE u.id=actor AND u.deleted_at IS NULL AND
 ((v.github_id IS NOT NULL AND u.github_id=v.github_id) OR (v.email<>'' AND (v.email=u.google_email OR (u.github_id IS NOT NULL AND NOT u.email_sync_error AND v.email=lower(u.email))))))));
$$;
-- +goose StatementEnd
-- Database enforcement also protects jobs and direct service calls.
-- +goose StatementBegin
CREATE FUNCTION workspace_require_github() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE actor bigint;
BEGIN
 IF TG_TABLE_NAME='workspaces' THEN actor:=NEW.owner_user_id;
 ELSE
  IF NEW.state<>'active' OR NEW.role='reviewer' THEN RETURN NEW; END IF;
  actor:=NEW.user_id;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM users WHERE id=actor AND github_id IS NOT NULL AND deleted_at IS NULL) THEN
  RAISE EXCEPTION 'Connect GitHub before creating a workspace or receiving a technical role' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER workspace_github_owner BEFORE INSERT OR UPDATE OF owner_user_id ON workspaces FOR EACH ROW EXECUTE FUNCTION workspace_require_github();
CREATE TRIGGER workspace_github_role BEFORE INSERT OR UPDATE OF role,state ON workspace_memberships FOR EACH ROW EXECUTE FUNCTION workspace_require_github();

-- +goose Down
-- Refuse destructive rollback once provider-independent accounts/invites exist.
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM users WHERE google_subject IS NOT NULL OR github_id IS NULL) OR EXISTS(SELECT 1 FROM workspace_invitations WHERE email<>'') THEN
  RAISE EXCEPTION 'Google identities or email invitations exist; forward-fix instead of discarding identity/access data';
 END IF;
END $$;
-- +goose StatementEnd
DROP TRIGGER workspace_github_role ON workspace_memberships;
DROP TRIGGER workspace_github_owner ON workspaces;
DROP FUNCTION workspace_require_github();
DROP FUNCTION workspace_invitation_matches(bigint,bigint,bigint);
ALTER TABLE deletion_journal DROP COLUMN google_subject;
DROP INDEX workspace_pending_email_invitation;
ALTER TABLE workspace_invitations DROP CONSTRAINT invitation_recipient, DROP CONSTRAINT email_invitation_reviewer, DROP COLUMN email;
ALTER TABLE workspace_invitations ALTER COLUMN github_id SET NOT NULL;
ALTER TABLE users DROP COLUMN google_subject,DROP COLUMN google_email;
ALTER TABLE users ALTER COLUMN github_id SET NOT NULL;
