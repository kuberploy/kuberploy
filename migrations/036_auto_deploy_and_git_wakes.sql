-- Immutable image-only auto-deploy policies and replay-safe Git projection
-- webhook wakes. Build workers never receive deployment or Git credentials;
-- the control-plane service account is freshly authorized for every run.

CREATE TABLE auto_deploy_policies (
    id uuid PRIMARY KEY,
    build_definition_id uuid NOT NULL REFERENCES build_definitions(id) ON DELETE RESTRICT,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    application_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    current_revision bigint NOT NULL CHECK (current_revision>0),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    UNIQUE (build_definition_id,environment_id),
    FOREIGN KEY (application_id,project_id) REFERENCES applications(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE RESTRICT
);

CREATE TABLE auto_deploy_policy_revisions (
    policy_id uuid NOT NULL REFERENCES auto_deploy_policies(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision>0),
    enabled boolean NOT NULL,
    source_deployment_id uuid NOT NULL REFERENCES deployments(id) ON DELETE RESTRICT,
    source_deployment_generation bigint NOT NULL CHECK (source_deployment_generation>0),
    source_config_etag text NOT NULL CHECK (source_config_etag ~ '^"(?:sha256:|cfg-sha256-)[0-9a-f]{64}"$'),
    config_intent bytea NOT NULL CHECK (octet_length(config_intent) BETWEEN 2 AND 262144),
    template_digest text NOT NULL CHECK (template_digest ~ '^sha256:[0-9a-f]{64}$'),
    service_actor_id uuid NOT NULL REFERENCES service_accounts(id) ON DELETE RESTRICT,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (policy_id,revision),
    CHECK (jsonb_typeof(convert_from(config_intent,'UTF8')::jsonb)='object')
);
ALTER TABLE auto_deploy_policies ADD CONSTRAINT auto_deploy_policy_current_revision_fk
    FOREIGN KEY (id,current_revision) REFERENCES auto_deploy_policy_revisions(policy_id,revision)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE auto_deploy_policy_commands (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    action text NOT NULL CHECK (action IN ('create','revise')),
    request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    policy_id uuid NOT NULL REFERENCES auto_deploy_policies(id) ON DELETE RESTRICT,
    result_revision bigint NOT NULL CHECK (result_revision>0),
    request_id text NOT NULL CHECK (request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (actor_id,idempotency_key),
    FOREIGN KEY (policy_id,result_revision) REFERENCES auto_deploy_policy_revisions(policy_id,revision) ON DELETE RESTRICT
);

CREATE TABLE auto_deploy_policy_audit (
    id uuid PRIMARY KEY,
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    action text NOT NULL CHECK (action IN ('create','revise','enable','disable')),
    policy_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision>0),
    service_actor_id uuid NOT NULL REFERENCES service_accounts(id) ON DELETE RESTRICT,
    template_digest text NOT NULL CHECK (template_digest ~ '^sha256:[0-9a-f]{64}$'),
    request_id text NOT NULL CHECK (request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    created_at timestamptz NOT NULL,
    FOREIGN KEY (policy_id,revision) REFERENCES auto_deploy_policy_revisions(policy_id,revision) ON DELETE RESTRICT
);

CREATE OR REPLACE FUNCTION validate_auto_deploy_policy_revision()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE policy_row auto_deploy_policies%ROWTYPE;
BEGIN
    SELECT * INTO STRICT policy_row FROM auto_deploy_policies WHERE id=NEW.policy_id;
    IF NOT EXISTS (
        SELECT 1 FROM build_definitions b
        JOIN applications a ON a.id=b.service_id AND a.project_id=b.project_id
        JOIN environments e ON e.id=policy_row.environment_id AND e.project_id=b.project_id
        JOIN deployments d ON d.id=NEW.source_deployment_id
             AND d.application_id=a.id AND d.environment_id=e.id AND d.generation=NEW.source_deployment_generation
        JOIN service_accounts sa ON sa.id=NEW.service_actor_id AND sa.project_id=b.project_id AND sa.disabled_at IS NULL
        WHERE b.id=policy_row.build_definition_id AND b.project_id=policy_row.project_id
          AND b.service_id=policy_row.application_id
    ) THEN
        RAISE EXCEPTION 'auto-deploy policy resource binding mismatch' USING ERRCODE='23503';
    END IF;
    IF NEW.created_at<policy_row.created_at THEN
        RAISE EXCEPTION 'auto-deploy revision predates policy' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;
CREATE CONSTRAINT TRIGGER auto_deploy_policy_revision_validate
    AFTER INSERT ON auto_deploy_policy_revisions DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION validate_auto_deploy_policy_revision();

CREATE OR REPLACE FUNCTION protect_auto_deploy_policy()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.id<>OLD.id OR NEW.build_definition_id<>OLD.build_definition_id OR NEW.project_id<>OLD.project_id OR
       NEW.application_id<>OLD.application_id OR NEW.environment_id<>OLD.environment_id OR NEW.created_by<>OLD.created_by OR
       NEW.created_at<>OLD.created_at OR NEW.current_revision<>OLD.current_revision+1 THEN
        RAISE EXCEPTION 'invalid auto-deploy policy transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER auto_deploy_policies_protect BEFORE UPDATE ON auto_deploy_policies
    FOR EACH ROW EXECUTE FUNCTION protect_auto_deploy_policy();

CREATE OR REPLACE FUNCTION reject_auto_deploy_immutable_change()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'auto-deploy immutable record cannot change' USING ERRCODE='23514';
END; $$;
CREATE TRIGGER auto_deploy_revisions_immutable BEFORE UPDATE OR DELETE ON auto_deploy_policy_revisions FOR EACH ROW EXECUTE FUNCTION reject_auto_deploy_immutable_change();
CREATE TRIGGER auto_deploy_commands_immutable BEFORE UPDATE OR DELETE ON auto_deploy_policy_commands FOR EACH ROW EXECUTE FUNCTION reject_auto_deploy_immutable_change();
CREATE TRIGGER auto_deploy_audit_immutable BEFORE UPDATE OR DELETE ON auto_deploy_policy_audit FOR EACH ROW EXECUTE FUNCTION reject_auto_deploy_immutable_change();

CREATE TABLE auto_deploy_runs (
    attempt_id uuid NOT NULL REFERENCES build_attempts(id) ON DELETE RESTRICT,
    policy_id uuid NOT NULL REFERENCES auto_deploy_policies(id) ON DELETE RESTRICT,
    policy_revision bigint NOT NULL,
    definition_id uuid NOT NULL REFERENCES build_definitions(id) ON DELETE RESTRICT,
    definition_digest text NOT NULL CHECK (definition_digest ~ '^sha256:[0-9a-f]{64}$'),
    release_id uuid NOT NULL REFERENCES registry_releases(id) ON DELETE RESTRICT,
    template_digest text NOT NULL CHECK (template_digest ~ '^sha256:[0-9a-f]{64}$'),
    source_deployment_id uuid NOT NULL REFERENCES deployments(id) ON DELETE RESTRICT,
    source_deployment_generation bigint NOT NULL CHECK (source_deployment_generation>0),
    source_config_etag text NOT NULL CHECK (source_config_etag ~ '^"(?:sha256:|cfg-sha256-)[0-9a-f]{64}"$'),
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^auto-deploy/[0-9a-f-]{36}/[1-9][0-9]*/[0-9a-f-]{36}$'),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','processing','submitted','failed')),
    attempts bigint NOT NULL DEFAULT 0 CHECK (attempts>=0),
    available_at timestamptz NOT NULL,
    lease_owner text,
    lease_until timestamptz,
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    operation_id uuid REFERENCES operations(id) ON DELETE RESTRICT,
    deployment_id uuid REFERENCES deployments(id) ON DELETE RESTRICT,
    failure_code text NOT NULL DEFAULT '' CHECK (failure_code='' OR failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    PRIMARY KEY (attempt_id,policy_id),
    UNIQUE (idempotency_key),
    FOREIGN KEY (policy_id,policy_revision) REFERENCES auto_deploy_policy_revisions(policy_id,revision) ON DELETE RESTRICT,
    CHECK ((lease_owner IS NULL)=(lease_until IS NULL)),
    CHECK ((state='processing')=(lease_owner IS NOT NULL)),
    CHECK ((state IN ('submitted','failed'))=(completed_at IS NOT NULL)),
    CHECK ((state='submitted' AND operation_id IS NOT NULL AND deployment_id IS NOT NULL AND failure_code='') OR
           (state='failed' AND operation_id IS NULL AND deployment_id IS NULL AND failure_code<>'') OR
           (state IN ('pending','processing') AND operation_id IS NULL AND deployment_id IS NULL)),
    CHECK (completed_at IS NULL OR completed_at>=created_at),
    CHECK (updated_at>=created_at),
    CHECK (lease_until IS NULL OR lease_until>updated_at)
);
CREATE INDEX auto_deploy_runs_work_idx ON auto_deploy_runs(available_at,created_at,attempt_id,policy_id)
    WHERE state IN ('pending','processing');

CREATE OR REPLACE FUNCTION protect_auto_deploy_run()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF TG_OP='INSERT' THEN
        IF NEW.state<>'pending' OR NEW.attempts<>0 OR NEW.lease_owner IS NOT NULL OR NEW.lease_epoch<>0 OR
           NEW.operation_id IS NOT NULL OR NEW.deployment_id IS NOT NULL OR NEW.failure_code<>'' OR NEW.completed_at IS NOT NULL OR
           NEW.updated_at<>NEW.created_at OR NEW.available_at<NEW.created_at THEN
            RAISE EXCEPTION 'auto-deploy run must be inserted pristine and pending' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.attempt_id<>OLD.attempt_id OR NEW.policy_id<>OLD.policy_id OR NEW.policy_revision<>OLD.policy_revision OR
       NEW.definition_id<>OLD.definition_id OR NEW.definition_digest<>OLD.definition_digest OR NEW.release_id<>OLD.release_id OR
       NEW.template_digest<>OLD.template_digest OR NEW.source_deployment_id<>OLD.source_deployment_id OR
       NEW.source_deployment_generation<>OLD.source_deployment_generation OR NEW.source_config_etag<>OLD.source_config_etag OR
       NEW.idempotency_key<>OLD.idempotency_key OR NEW.created_at<>OLD.created_at OR NEW.updated_at<OLD.updated_at OR
       NEW.lease_epoch<OLD.lease_epoch THEN
        RAISE EXCEPTION 'auto-deploy run immutable identity changed' USING ERRCODE='23514';
    END IF;
    IF OLD.state='pending' AND NEW.state='processing' THEN
        IF NEW.attempts<>OLD.attempts+1 OR NEW.lease_epoch<>OLD.lease_epoch+1 OR NEW.lease_owner IS NULL OR
           NEW.available_at<>OLD.available_at OR NEW.failure_code<>'' OR NEW.completed_at IS NOT NULL THEN
            RAISE EXCEPTION 'invalid auto-deploy run acquisition' USING ERRCODE='23514'; END IF;
    ELSIF OLD.state='processing' AND NEW.state='processing' THEN
		IF OLD.lease_until>NEW.updated_at AND NEW.attempts=OLD.attempts AND NEW.lease_epoch=OLD.lease_epoch AND NEW.lease_owner=OLD.lease_owner AND NEW.lease_until>OLD.lease_until AND
		   NEW.available_at=OLD.available_at AND NEW.failure_code=OLD.failure_code AND NEW.completed_at IS NOT DISTINCT FROM OLD.completed_at AND
		   NEW.operation_id IS NOT DISTINCT FROM OLD.operation_id AND NEW.deployment_id IS NOT DISTINCT FROM OLD.deployment_id THEN
            NULL; -- heartbeat
		ELSIF OLD.lease_until<=NEW.updated_at AND NEW.attempts=OLD.attempts+1 AND NEW.lease_epoch=OLD.lease_epoch+1 AND NEW.lease_owner IS NOT NULL AND
		   NEW.available_at=OLD.available_at AND NEW.failure_code='' AND NEW.completed_at IS NULL AND NEW.operation_id IS NULL AND NEW.deployment_id IS NULL THEN
            NULL; -- expired-lease recovery
        ELSE RAISE EXCEPTION 'invalid auto-deploy processing transition' USING ERRCODE='23514'; END IF;
    ELSIF OLD.state='processing' AND NEW.state='pending' THEN
		IF OLD.lease_until<=NEW.updated_at OR NEW.attempts<>OLD.attempts OR NEW.lease_epoch<>OLD.lease_epoch OR NEW.lease_owner IS NOT NULL OR
           NEW.available_at<NEW.updated_at OR NEW.failure_code='' OR NEW.completed_at IS NOT NULL THEN
            RAISE EXCEPTION 'invalid auto-deploy retry transition' USING ERRCODE='23514'; END IF;
    ELSIF OLD.state='processing' AND NEW.state IN ('submitted','failed') THEN
		IF OLD.lease_until<=NEW.updated_at OR NEW.attempts<>OLD.attempts OR NEW.lease_epoch<>OLD.lease_epoch OR NEW.lease_owner IS NOT NULL OR NEW.completed_at<>NEW.updated_at THEN
            RAISE EXCEPTION 'invalid auto-deploy completion transition' USING ERRCODE='23514'; END IF;
    ELSE
        RAISE EXCEPTION 'invalid auto-deploy run state transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER auto_deploy_runs_protect BEFORE INSERT OR UPDATE OR DELETE ON auto_deploy_runs
    FOR EACH ROW EXECUTE FUNCTION protect_auto_deploy_run();

CREATE OR REPLACE FUNCTION enqueue_auto_deploy_runs()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state<>'succeeded' OR NEW.release_id IS NULL OR
       (TG_OP='UPDATE' AND OLD.state='succeeded') THEN RETURN NEW; END IF;
    INSERT INTO auto_deploy_runs(attempt_id,policy_id,policy_revision,definition_id,definition_digest,release_id,
        template_digest,source_deployment_id,source_deployment_generation,source_config_etag,idempotency_key,available_at,created_at,updated_at)
    SELECT a.id,p.id,p.current_revision,a.definition_id,a.definition_digest,NEW.release_id,r.template_digest,
        r.source_deployment_id,r.source_deployment_generation,r.source_config_etag,
        'auto-deploy/'||p.id::text||'/'||p.current_revision::text||'/'||a.id::text,
        NEW.completed_at,NEW.completed_at,NEW.completed_at
    FROM build_attempts a
    JOIN auto_deploy_policies p ON p.build_definition_id=a.definition_id
    JOIN auto_deploy_policy_revisions r ON r.policy_id=p.id AND r.revision=p.current_revision AND r.enabled
    WHERE a.id=NEW.attempt_id
    ON CONFLICT(attempt_id,policy_id) DO NOTHING;
    RETURN NEW;
END; $$;
CREATE TRIGGER build_release_enqueue_auto_deploy
    AFTER INSERT OR UPDATE OF state ON build_release_projections
    FOR EACH ROW EXECUTE FUNCTION enqueue_auto_deploy_runs();

-- A wake receipt is an HMAC-authenticated hint, never a verified head. Exact
-- binding targets record which monotonic wake generation each receipt caused.
ALTER TABLE git_safety_poll_cursors
    ADD COLUMN wake_generation bigint NOT NULL DEFAULT 0 CHECK (wake_generation>=0),
    ADD COLUMN reconciled_wake_generation bigint NOT NULL DEFAULT 0 CHECK (reconciled_wake_generation>=0),
    ADD CONSTRAINT git_safety_poll_wake_order CHECK (reconciled_wake_generation<=wake_generation);

CREATE TABLE git_projection_push_wakes (
    delivery_hash text PRIMARY KEY CHECK (delivery_hash ~ '^sha256:[0-9a-f]{64}$'),
    github_app_id bigint NOT NULL CHECK (github_app_id>0),
    installation_id bigint NOT NULL CHECK (installation_id>0),
    repository_id bigint NOT NULL CHECK (repository_id>0),
    target_ref text NOT NULL CHECK (target_ref ~ '^refs/heads/'),
    after_commit text NOT NULL CHECK (after_commit ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    received_at timestamptz NOT NULL,
    UNIQUE (github_app_id,installation_id,repository_id,target_ref,after_commit)
);
CREATE TABLE git_projection_push_wake_targets (
    delivery_hash text NOT NULL REFERENCES git_projection_push_wakes(delivery_hash) ON DELETE RESTRICT,
    binding_id uuid NOT NULL REFERENCES git_repository_bindings(id) ON DELETE RESTRICT,
    wake_generation bigint NOT NULL CHECK (wake_generation>0),
    PRIMARY KEY (delivery_hash,binding_id),
    UNIQUE (binding_id,wake_generation)
);
CREATE OR REPLACE FUNCTION reject_git_push_wake_change()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'Git push wake receipts are immutable' USING ERRCODE='23514';
END; $$;
CREATE TRIGGER git_push_wakes_immutable BEFORE UPDATE OR DELETE ON git_projection_push_wakes FOR EACH ROW EXECUTE FUNCTION reject_git_push_wake_change();
CREATE TRIGGER git_push_wake_targets_immutable BEFORE UPDATE OR DELETE ON git_projection_push_wake_targets FOR EACH ROW EXECUTE FUNCTION reject_git_push_wake_change();
