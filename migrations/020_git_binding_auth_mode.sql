-- Make Git transport authority explicit. GitHub App bindings use short-lived,
-- repository-scoped tokens and therefore must not name a Kubernetes credential
-- Secret. Existing operator-created bindings retain their legacy Secret mode.
ALTER TABLE git_repository_bindings
    ADD COLUMN IF NOT EXISTS credential_mode text NOT NULL DEFAULT 'legacy-secret';

ALTER TABLE git_repository_bindings
    DROP CONSTRAINT IF EXISTS git_repository_bindings_credential_secret_name_check;
ALTER TABLE git_repository_bindings
    DROP CONSTRAINT IF EXISTS git_repository_bindings_credential_mode_check;
ALTER TABLE git_repository_bindings
    ADD CONSTRAINT git_repository_bindings_credential_mode_check CHECK (
        (credential_mode='legacy-secret'
            AND credential_secret_name ~ '^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$')
        OR (credential_mode='github-app' AND credential_secret_name='')
    );

-- One authoritative desired-state binding per scope. Multiple repositories for
-- one environment would make reads and Argo desired revisions ambiguous.
CREATE UNIQUE INDEX IF NOT EXISTS git_repository_bindings_environment_authority
    ON git_repository_bindings(environment_id) WHERE kind='environment';
CREATE UNIQUE INDEX IF NOT EXISTS git_repository_bindings_platform_authority
    ON git_repository_bindings(cluster_id) WHERE kind='platform';

CREATE OR REPLACE FUNCTION protect_git_binding_identity()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.kind,NEW.scope_id,NEW.project_id,NEW.environment_id,NEW.cluster_id,
           NEW.provider,NEW.installation_id,NEW.repository_id,NEW.repository_owner,
           NEW.repository_name,NEW.target_ref,NEW.path_prefix,NEW.credential_mode,
           NEW.credential_secret_name)
       IS DISTINCT FROM
       ROW(OLD.kind,OLD.scope_id,OLD.project_id,OLD.environment_id,OLD.cluster_id,
           OLD.provider,OLD.installation_id,OLD.repository_id,OLD.repository_owner,
           OLD.repository_name,OLD.target_ref,OLD.path_prefix,OLD.credential_mode,
           OLD.credential_secret_name) THEN
        RAISE EXCEPTION 'Git binding identity is immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
