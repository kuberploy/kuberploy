-- Deployment AppConfig and VariableSet previews share one bounded, one-time
-- token authority. Kind-specific CHECK constraints keep their identities
-- disjoint while retaining exact Git snapshot and candidate bindings.

CREATE TABLE preview_authorities (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash)=32),
    preview_kind text NOT NULL CHECK (preview_kind IN ('deployment-config','variable-set')),
    actor_id uuid NOT NULL,
    deployment_id uuid REFERENCES deployments(id) ON DELETE CASCADE,
    binding_id uuid REFERENCES git_repository_bindings(id) ON DELETE CASCADE,
    project_id uuid REFERENCES projects(id) ON DELETE CASCADE,
    environment_id uuid REFERENCES environments(id) ON DELETE CASCADE,
    variable_scope text,
    path text,
    base_revision text,
    base_etag text NOT NULL,
    expected_etag text,
    policy_version text,
    chart_identity text,
    candidate_hash bytea NOT NULL CHECK (octet_length(candidate_hash)=32),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (binding_id,project_id,environment_id)
        REFERENCES git_repository_bindings(id,project_id,environment_id) ON DELETE CASCADE,
    CHECK (expires_at>created_at OR preview_kind='deployment-config'),
    CHECK (consumed_at IS NULL OR consumed_at>=created_at),
    CHECK (
        (preview_kind='deployment-config' AND deployment_id IS NOT NULL
            AND project_id IS NULL AND environment_id IS NULL AND variable_scope IS NULL
            AND ((binding_id IS NULL AND path IS NULL AND base_revision IS NULL
                    AND expected_etag IS NULL AND policy_version IS NULL AND chart_identity IS NULL) OR
                (binding_id IS NOT NULL
                    AND base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
                    AND path IS NOT NULL AND length(path) BETWEEN 1 AND 1024
                    AND path !~ '(^/|/\.\.?(/|$)|//|\\)'
                    AND expected_etag ~ '^"sha256:[0-9a-f]{64}"$'
                    AND chart_identity ~ '^(?:sha256:[0-9a-f]{64}|[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[0-9]+)?)$'
                    AND length(policy_version) BETWEEN 1 AND 128
                    AND policy_version !~ E'[\\x00\\r\\n]'))) OR
        (preview_kind='variable-set' AND deployment_id IS NULL AND binding_id IS NOT NULL
            AND project_id IS NOT NULL AND environment_id IS NOT NULL
            AND variable_scope IN ('project','environment')
            AND base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
            AND (base_etag='' OR base_etag ~ '^"sha256:[0-9a-f]{64}"$')
            AND expected_etag IS NULL AND chart_identity IS NULL
            AND length(policy_version) BETWEEN 1 AND 128
            AND policy_version !~ E'[\\x00\\r\\n]'
            AND ((variable_scope='project' AND path='tenants/'||project_id::text||'/variables.yaml') OR
                 (variable_scope='environment' AND path='tenants/'||project_id::text||'/environments/'||environment_id::text||'/variables.yaml')))
    )
);
CREATE INDEX preview_authorities_expiry_idx ON preview_authorities(expires_at)
    WHERE consumed_at IS NULL;
CREATE INDEX preview_authorities_deployment_lookup_idx
    ON preview_authorities(deployment_id,actor_id,expires_at DESC)
    WHERE preview_kind='deployment-config';

INSERT INTO preview_authorities(token_hash,preview_kind,actor_id,deployment_id,binding_id,path,
    base_revision,base_etag,expected_etag,policy_version,chart_identity,candidate_hash,
    expires_at,consumed_at,created_at)
SELECT token_hash,'deployment-config',actor_id,deployment_id,git_binding_id,git_path,
    git_base_revision,base_etag,git_expected_etag,git_policy_version,git_chart_digest,candidate_hash,
    expires_at,consumed_at,created_at
FROM deployment_config_previews;

INSERT INTO preview_authorities(token_hash,preview_kind,actor_id,binding_id,project_id,environment_id,
    variable_scope,path,base_revision,base_etag,policy_version,candidate_hash,expires_at,consumed_at,created_at)
SELECT token_hash,'variable-set',actor_id,binding_id,project_id,environment_id,scope,path,
    base_revision,base_etag,parser_version,candidate_hash,expires_at,consumed_at,created_at
FROM variable_set_previews;

DROP TABLE deployment_config_previews;
DROP TABLE variable_set_previews;

CREATE OR REPLACE FUNCTION protect_preview_authority()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        IF OLD.preview_kind='variable-set' THEN
            RAISE EXCEPTION 'VariableSet previews are immutable' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.consumed_at IS NOT NULL OR NEW.created_at>now()+interval '1 minute' OR
           (NEW.preview_kind='variable-set' AND NEW.expires_at>NEW.created_at+interval '15 minutes') THEN
            RAISE EXCEPTION 'preview must start pristine and bounded' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.token_hash,NEW.preview_kind,NEW.actor_id,NEW.deployment_id,NEW.binding_id,NEW.project_id,
        NEW.environment_id,NEW.variable_scope,NEW.path,NEW.base_revision,NEW.base_etag,NEW.expected_etag,
        NEW.policy_version,NEW.chart_identity,NEW.candidate_hash,NEW.expires_at,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.token_hash,OLD.preview_kind,OLD.actor_id,OLD.deployment_id,OLD.binding_id,OLD.project_id,
        OLD.environment_id,OLD.variable_scope,OLD.path,OLD.base_revision,OLD.base_etag,OLD.expected_etag,
        OLD.policy_version,OLD.chart_identity,OLD.candidate_hash,OLD.expires_at,OLD.created_at) OR
       OLD.consumed_at IS NOT NULL OR NEW.consumed_at IS NULL OR NEW.consumed_at<OLD.created_at OR
       NEW.consumed_at>now()+interval '1 minute' THEN
        RAISE EXCEPTION 'preview update is not an exact consumption' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER preview_authorities_protect
    BEFORE INSERT OR UPDATE OR DELETE ON preview_authorities
    FOR EACH ROW EXECUTE FUNCTION protect_preview_authority();

DROP FUNCTION protect_variable_set_preview();
