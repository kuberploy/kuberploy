-- The production GitOps write path persists an immutable, authorized command
-- in the same transaction as its deployment operation. Git/network work is
-- retried outside that transaction; the operation trailer and these exact
-- bytes make a successful push recoverable after a process crash.
CREATE TABLE git_deployment_write_commands (
    operation_id uuid PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
    deployment_id uuid NOT NULL REFERENCES deployments(id) DEFERRABLE INITIALLY DEFERRED,
    actor_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    application_id uuid NOT NULL REFERENCES applications(id) ON DELETE RESTRICT,
    target_ref text NOT NULL,
    path text NOT NULL CHECK (
        path='tenants/'||project_id::text||'/environments/'||environment_id::text||
             '/apps/'||application_id::text||'/app.yaml'
    ),
    base_revision text NOT NULL CHECK (base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    precondition text NOT NULL CHECK (precondition IN ('match-etag','create-if-absent')),
    expected_etag text NOT NULL DEFAULT '' CHECK (
        (precondition='match-etag' AND expected_etag ~ '^"sha256:[0-9a-f]{64}"$') OR
        (precondition='create-if-absent' AND expected_etag='')
    ),
    chart_digest text NOT NULL CHECK (chart_digest ~ '^sha256:[0-9a-f]{64}$'),
    policy_version text NOT NULL CHECK (
        length(policy_version) BETWEEN 1 AND 128 AND policy_version !~ E'[\\x00\\r\\n]'
    ),
    content bytea NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 262144),
    content_sha256 text NOT NULL CHECK (content_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    message text NOT NULL CHECK (
        length(message) BETWEEN 1 AND 512 AND message !~ E'[\\x00\\r]'
    ),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','git-committed','indexed')),
    committed_revision text NOT NULL DEFAULT '' CHECK (
        committed_revision='' OR committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_at timestamptz,
    indexed_generation bigint NOT NULL DEFAULT 0 CHECK (indexed_generation>=0),
    indexed_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (binding_id,target_ref)
        REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (binding_id,project_id,environment_id)
        REFERENCES git_repository_bindings(id,project_id,environment_id) ON DELETE RESTRICT,
    CHECK (updated_at>=created_at),
    CHECK (
        (state='pending' AND committed_revision='' AND committed_at IS NULL
            AND indexed_generation=0 AND indexed_at IS NULL) OR
        (state='git-committed' AND committed_revision<>'' AND committed_at IS NOT NULL
            AND committed_at>=created_at AND indexed_generation=0 AND indexed_at IS NULL) OR
        (state='indexed' AND committed_revision<>'' AND committed_at IS NOT NULL
            AND committed_at>=created_at AND indexed_generation>0 AND indexed_at IS NOT NULL
            AND indexed_at>=committed_at)
    )
);

CREATE INDEX git_deployment_write_commands_binding_state_idx
    ON git_deployment_write_commands(binding_id,state,created_at,operation_id);
CREATE INDEX git_deployment_write_commands_committed_idx
    ON git_deployment_write_commands(binding_id,committed_revision)
    WHERE state IN ('git-committed','indexed');

CREATE OR REPLACE FUNCTION protect_git_deployment_write_command()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.operation_id,NEW.deployment_id,NEW.actor_id,NEW.binding_id,
           NEW.project_id,NEW.environment_id,NEW.application_id,NEW.target_ref,
           NEW.path,NEW.base_revision,NEW.precondition,NEW.expected_etag,
           NEW.chart_digest,NEW.policy_version,NEW.content,NEW.content_sha256,
           NEW.message,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.operation_id,OLD.deployment_id,OLD.actor_id,OLD.binding_id,
           OLD.project_id,OLD.environment_id,OLD.application_id,OLD.target_ref,
           OLD.path,OLD.base_revision,OLD.precondition,OLD.expected_etag,
           OLD.chart_digest,OLD.policy_version,OLD.content,OLD.content_sha256,
           OLD.message,OLD.created_at) THEN
        RAISE EXCEPTION 'Git deployment write command identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF (OLD.state='git-committed' AND NEW.state='pending') OR
       (OLD.state='indexed' AND NEW.state<>'indexed') OR
       NEW.updated_at<OLD.updated_at THEN
        RAISE EXCEPTION 'Git deployment write command state cannot regress'
            USING ERRCODE='23514';
    END IF;
    IF OLD.committed_revision<>'' AND
       (NEW.committed_revision<>OLD.committed_revision OR NEW.committed_at<>OLD.committed_at) THEN
        RAISE EXCEPTION 'Git deployment write result is immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER git_deployment_write_commands_protect
    BEFORE UPDATE ON git_deployment_write_commands
    FOR EACH ROW EXECUTE FUNCTION protect_git_deployment_write_command();

-- A projection-backed preview binds its token to the exact indexed Git
-- snapshot. Legacy previews keep every Git field NULL; the two modes cannot be
-- mixed or partially populated.
ALTER TABLE deployment_config_previews
    ADD COLUMN git_binding_id uuid REFERENCES git_repository_bindings(id) ON DELETE CASCADE,
    ADD COLUMN git_base_revision text,
    ADD COLUMN git_path text,
    ADD COLUMN git_expected_etag text,
    ADD COLUMN git_chart_digest text,
    ADD COLUMN git_policy_version text,
    ADD CONSTRAINT deployment_config_previews_git_shape CHECK (
        (git_binding_id IS NULL AND git_base_revision IS NULL AND git_path IS NULL
            AND git_expected_etag IS NULL AND git_chart_digest IS NULL
            AND git_policy_version IS NULL) OR
        (git_binding_id IS NOT NULL
            AND git_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
            AND git_path IS NOT NULL AND length(git_path) BETWEEN 1 AND 1024
            AND git_path !~ '(^/|/\.\.?(/|$)|//|\\)'
            AND git_expected_etag ~ '^"sha256:[0-9a-f]{64}"$'
            AND git_chart_digest ~ '^sha256:[0-9a-f]{64}$'
            AND length(git_policy_version) BETWEEN 1 AND 128
            AND git_policy_version !~ E'[\\x00\\r\\n]')
    );

-- Public git/gitops capability requires a fresh worker lease matching the
-- complete non-secret runtime digest and this exact writer/indexer contract.
CREATE TABLE git_projection_runtime_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    contract_version text NOT NULL CHECK (
        length(contract_version) BETWEEN 8 AND 64 AND
        contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    config_digest text NOT NULL CHECK (config_digest ~ '^sha256:[0-9a-f]{64}$'),
    github_app_id bigint NOT NULL CHECK (github_app_id>0),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    CHECK (observed_at>=started_at AND lease_until>observed_at)
);

CREATE INDEX git_projection_runtime_readiness_match_idx
    ON git_projection_runtime_readiness(
        contract_version,config_digest,github_app_id,observed_at DESC
    );

CREATE OR REPLACE FUNCTION protect_git_projection_runtime_readiness_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id<>OLD.worker_id OR NEW.worker_epoch<OLD.worker_epoch OR
       NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Git projection readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND (
        NEW.contract_version<>OLD.contract_version OR
        NEW.config_digest<>OLD.config_digest OR
        NEW.github_app_id<>OLD.github_app_id OR
        NEW.started_at<>OLD.started_at OR NEW.observed_at<OLD.observed_at
    ) THEN
        RAISE EXCEPTION 'Git projection readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER git_projection_runtime_readiness_epoch
    BEFORE UPDATE ON git_projection_runtime_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_git_projection_runtime_readiness_epoch();
