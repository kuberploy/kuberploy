-- Protected Argo desired state is written only to the operator-owned platform
-- Git binding. These rows are immutable commands, not a second desired-state
-- authority: the exact bytes become authoritative only after the hardened Git
-- writer has pushed and provider-verified the protected ref.
ALTER TABLE git_repository_bindings
    ADD CONSTRAINT git_repository_bindings_id_cluster_unique UNIQUE (id,cluster_id);

CREATE TABLE argo_desired_state_commands (
    id uuid PRIMARY KEY,
    generation bigint NOT NULL CHECK (generation>0),
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    platform_binding_id uuid NOT NULL,
    environment_binding_id uuid NOT NULL,
    cluster_id uuid NOT NULL,
    platform_target_ref text NOT NULL,
    environment_target_ref text NOT NULL,
    environment_revision text NOT NULL CHECK (
        environment_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    environment_generation bigint NOT NULL CHECK (environment_generation>0),
    path text NOT NULL CHECK (
        path='clusters/'||cluster_id::text||'/argocd/environments/'||
             environment_id::text||'.yaml'
    ),
    argo_namespace text NOT NULL CHECK (
        argo_namespace ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'
    ),
    destination_namespace text NOT NULL CHECK (
        destination_namespace ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'
    ),
    argo_project text NOT NULL CHECK (
        argo_project ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'
    ),
    base_revision text NOT NULL CHECK (
        base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    write_base_revision text NOT NULL DEFAULT '' CHECK (
        write_base_revision='' OR
        write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    write_base_observed_at timestamptz,
    precondition text NOT NULL CHECK (
        precondition IN ('match-etag','create-if-absent')
    ),
    expected_etag text NOT NULL DEFAULT '' CHECK (
        (precondition='match-etag' AND
         expected_etag ~ '^"sha256:[0-9a-f]{64}"$') OR
        (precondition='create-if-absent' AND expected_etag='')
    ),
    catalog_digest text NOT NULL CHECK (
        catalog_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    chart_repository text NOT NULL CHECK (
        length(chart_repository) BETWEEN 7 AND 512 AND
        chart_repository ~ '^oci://[^/?#@[:space:]]+/[^?#@[:space:]]+$'
    ),
    chart_name text NOT NULL CHECK (chart_name='kuberploy-runtime'),
    chart_version text NOT NULL CHECK (
        length(chart_version) BETWEEN 5 AND 64 AND
        chart_version ~ '^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$'
    ),
    chart_digest text NOT NULL CHECK (
        chart_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    renderer_image text NOT NULL CHECK (
        length(renderer_image) BETWEEN 10 AND 512 AND
        renderer_image ~ '^[^[:space:]@]+@sha256:[0-9a-f]{64}$'
    ),
    chart_digest_enforcement text NOT NULL CHECK (
        chart_digest_enforcement IN ('unavailable','native-oci-digest-v1')
    ),
    content bytea NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 262144),
    content_sha256 text NOT NULL CHECK (
        content_sha256 ~ '^sha256:[0-9a-f]{64}$'
    ),
    message text NOT NULL CHECK (
        length(message) BETWEEN 1 AND 512 AND message !~ E'[\\x00\\r]'
    ),
    state text NOT NULL DEFAULT 'pending' CHECK (
        state IN ('pending','claimed','git-committed','verified',
                  'blocked-prerequisite','failed','superseded')
    ),
    committed_revision text NOT NULL DEFAULT '' CHECK (
        committed_revision='' OR
        committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_at timestamptz,
    verified_at timestamptz,
    next_attempt_at timestamptz NOT NULL,
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (
        consecutive_failures BETWEEN 0 AND 30
    ),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR
        last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND
         lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    lease_until timestamptz,
    worker_contract text CHECK (
        worker_contract IS NULL OR
        (length(worker_contract) BETWEEN 8 AND 64 AND
         worker_contract ~ '^[a-z][a-z0-9.-]{7,63}$')
    ),
    worker_config_digest text CHECK (
        worker_config_digest IS NULL OR
        worker_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    UNIQUE (environment_id,generation),
    FOREIGN KEY (environment_id,project_id)
        REFERENCES environments(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id,platform_target_ref)
        REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id,cluster_id)
        REFERENCES git_repository_bindings(id,cluster_id) ON DELETE RESTRICT,
    FOREIGN KEY (environment_binding_id,environment_target_ref)
        REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (environment_binding_id,project_id,environment_id)
        REFERENCES git_repository_bindings(id,project_id,environment_id)
        ON DELETE RESTRICT,
    CHECK (updated_at>=created_at AND next_attempt_at>=created_at),
    CHECK (
        (write_base_revision='' AND write_base_observed_at IS NULL) OR
        (write_base_revision<>'' AND write_base_observed_at IS NOT NULL AND
         write_base_observed_at>=created_at AND
         write_base_observed_at<=updated_at)
    ),
    CHECK ((last_failure_code='')=(consecutive_failures=0)),
    CHECK (
        (lease_owner IS NULL AND lease_until IS NULL AND
         worker_contract IS NULL AND worker_config_digest IS NULL) OR
        (lease_owner IS NOT NULL AND lease_until IS NOT NULL AND
         worker_contract IS NOT NULL AND worker_config_digest IS NOT NULL AND
         lease_epoch>0 AND lease_until>updated_at)
    ),
    CHECK (
        (state='pending' AND committed_revision='' AND committed_at IS NULL AND
         verified_at IS NULL AND completed_at IS NULL AND lease_owner IS NULL) OR
        (state='claimed' AND committed_revision='' AND committed_at IS NULL AND
         verified_at IS NULL AND completed_at IS NULL AND lease_owner IS NOT NULL) OR
        (state='git-committed' AND write_base_revision<>'' AND
         committed_revision<>'' AND
         committed_at IS NOT NULL AND committed_at>=created_at AND
         verified_at IS NULL AND completed_at IS NULL) OR
        (state='verified' AND write_base_revision<>'' AND
         committed_revision<>'' AND
         committed_at IS NOT NULL AND verified_at IS NOT NULL AND
         verified_at>=committed_at AND completed_at=verified_at AND
         lease_owner IS NULL) OR
        (state IN ('blocked-prerequisite','superseded') AND
         write_base_revision='' AND committed_revision='' AND
         committed_at IS NULL AND verified_at IS NULL AND
         completed_at IS NOT NULL AND completed_at>=created_at AND
         lease_owner IS NULL) OR
        (state='failed' AND
         committed_revision='' AND committed_at IS NULL AND
         verified_at IS NULL AND completed_at IS NOT NULL AND
         completed_at>=created_at AND lease_owner IS NULL)
    ),
    CHECK (
        (chart_digest_enforcement='unavailable' AND
         state='blocked-prerequisite') OR
        (chart_digest_enforcement='native-oci-digest-v1' AND
         state<>'blocked-prerequisite')
    )
);

CREATE UNIQUE INDEX argo_desired_state_commands_environment_live_idx
    ON argo_desired_state_commands(environment_id)
    WHERE state IN ('pending','claimed','git-committed');
CREATE UNIQUE INDEX argo_desired_state_commands_binding_claim_idx
    ON argo_desired_state_commands(platform_binding_id)
    WHERE lease_owner IS NOT NULL;
CREATE INDEX argo_desired_state_commands_due_idx
    ON argo_desired_state_commands(next_attempt_at,created_at,id)
    WHERE state IN ('pending','claimed','git-committed');
CREATE INDEX argo_desired_state_commands_status_idx
    ON argo_desired_state_commands(environment_id,generation DESC);

CREATE OR REPLACE FUNCTION validate_argo_desired_state_command()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    platform_kind text;
    platform_mode text;
    platform_ref text;
    platform_cluster uuid;
    platform_state text;
    platform_head text;
    environment_kind text;
    environment_ref text;
    environment_project uuid;
    bound_environment uuid;
    environment_state text;
    environment_head text;
    environment_indexed text;
    environment_projection_generation bigint;
BEGIN
    SELECT kind,credential_mode,target_ref,cluster_id,state,target_head_revision
      INTO platform_kind,platform_mode,platform_ref,platform_cluster,
           platform_state,platform_head
      FROM git_repository_bindings
     WHERE id=NEW.platform_binding_id;
    IF platform_kind IS DISTINCT FROM 'platform' OR
       platform_mode IS DISTINCT FROM 'github-app' OR
       platform_ref IS DISTINCT FROM NEW.platform_target_ref OR
       platform_cluster IS DISTINCT FROM NEW.cluster_id THEN
        RAISE EXCEPTION 'Argo desired state requires the exact protected GitHub App platform binding'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (
       platform_state NOT IN ('ready','indexing') OR
       platform_head IS DISTINCT FROM NEW.base_revision
    ) THEN
        RAISE EXCEPTION 'Argo desired state requires the exact provider-verified planned platform head'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (
       NEW.write_base_revision<>'' OR NEW.write_base_observed_at IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'Argo desired-state write base may only be set by a fenced claim'
            USING ERRCODE='23514';
    END IF;

    SELECT kind,target_ref,project_id,environment_id,state,target_head_revision,
           indexed_revision,projection_generation
      INTO environment_kind,environment_ref,environment_project,bound_environment,
           environment_state,environment_head,environment_indexed,
           environment_projection_generation
      FROM git_repository_bindings
     WHERE id=NEW.environment_binding_id;
    IF environment_kind IS DISTINCT FROM 'environment' OR
       environment_ref IS DISTINCT FROM NEW.environment_target_ref OR
       environment_project IS DISTINCT FROM NEW.project_id OR
       bound_environment IS DISTINCT FROM NEW.environment_id THEN
        RAISE EXCEPTION 'Argo desired state environment binding identity does not match'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (
       environment_state IS DISTINCT FROM 'ready' OR
       environment_head IS DISTINCT FROM environment_indexed OR
       environment_indexed IS DISTINCT FROM NEW.environment_revision OR
       environment_projection_generation IS DISTINCT FROM NEW.environment_generation
    ) THEN
        RAISE EXCEPTION 'Argo desired state requires the exact active indexed environment generation'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND NOT EXISTS (
        SELECT 1 FROM git_projection_generations generation
         WHERE generation.binding_id=NEW.environment_binding_id
           AND generation.generation=NEW.environment_generation
           AND generation.head_revision=NEW.environment_revision
           AND generation.state='active'
    ) THEN
        RAISE EXCEPTION 'Argo desired state requires an activated projection receipt'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND EXISTS (
        SELECT 1 FROM git_projected_documents document
         WHERE document.binding_id=NEW.environment_binding_id
           AND document.generation=NEW.environment_generation
           AND NOT document.valid
    ) THEN
        RAISE EXCEPTION 'Argo desired state refuses an invalid projected document'
            USING ERRCODE='23514';
    END IF;

    IF NEW.destination_namespace IS DISTINCT FROM (
        SELECT namespace FROM environments WHERE id=NEW.environment_id
    ) OR NEW.argo_project IS DISTINCT FROM (
        SELECT argo_project FROM environments WHERE id=NEW.environment_id
    ) THEN
        RAISE EXCEPTION 'Argo destination identity must be server-derived'
            USING ERRCODE='23514';
    END IF;

    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.id,NEW.generation,NEW.project_id,NEW.environment_id,
               NEW.platform_binding_id,NEW.environment_binding_id,NEW.cluster_id,
               NEW.platform_target_ref,NEW.environment_target_ref,
               NEW.environment_revision,NEW.environment_generation,NEW.path,
               NEW.argo_namespace,NEW.destination_namespace,NEW.argo_project,
               NEW.base_revision,NEW.precondition,NEW.expected_etag,
               NEW.catalog_digest,NEW.chart_repository,NEW.chart_name,
               NEW.chart_version,NEW.chart_digest,NEW.renderer_image,
               NEW.chart_digest_enforcement,NEW.content,NEW.content_sha256,
               NEW.message,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.id,OLD.generation,OLD.project_id,OLD.environment_id,
               OLD.platform_binding_id,OLD.environment_binding_id,OLD.cluster_id,
               OLD.platform_target_ref,OLD.environment_target_ref,
               OLD.environment_revision,OLD.environment_generation,OLD.path,
               OLD.argo_namespace,OLD.destination_namespace,OLD.argo_project,
               OLD.base_revision,OLD.precondition,OLD.expected_etag,
               OLD.catalog_digest,OLD.chart_repository,OLD.chart_name,
               OLD.chart_version,OLD.chart_digest,OLD.renderer_image,
               OLD.chart_digest_enforcement,OLD.content,OLD.content_sha256,
               OLD.message,OLD.created_at) THEN
            RAISE EXCEPTION 'Argo desired-state command identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR
           NEW.lease_epoch>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'Argo desired-state command epoch is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           NEW.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest) THEN
            RAISE EXCEPTION 'Argo desired-state lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.write_base_revision<>'' AND
           ROW(NEW.write_base_revision,NEW.write_base_observed_at)
           IS DISTINCT FROM
           ROW(OLD.write_base_revision,OLD.write_base_observed_at) THEN
            RAISE EXCEPTION 'Argo desired-state write-base receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.write_base_revision='' AND NEW.write_base_revision<>'' AND (
           OLD.state<>'claimed' OR NEW.state<>'claimed' OR
           OLD.lease_owner IS NULL OR NEW.lease_owner IS NULL OR
           NEW.lease_epoch<>OLD.lease_epoch OR
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest)
        ) THEN
            RAISE EXCEPTION 'Argo desired-state write-base receipt requires the exact fenced claim'
                USING ERRCODE='23514';
        END IF;
        IF OLD.state IN ('verified','blocked-prerequisite','failed','superseded') AND
           NEW.state<>OLD.state THEN
            RAISE EXCEPTION 'Argo desired-state terminal state is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.state IN ('verified','blocked-prerequisite','failed','superseded') AND
           ROW(NEW.state,NEW.committed_revision,NEW.committed_at,NEW.verified_at,
               NEW.write_base_revision,NEW.write_base_observed_at,
               NEW.next_attempt_at,NEW.consecutive_failures,NEW.last_failure_code,
               NEW.lease_owner,NEW.lease_epoch,NEW.lease_until,
               NEW.worker_contract,NEW.worker_config_digest,NEW.updated_at,
               NEW.completed_at)
           IS DISTINCT FROM
           ROW(OLD.state,OLD.committed_revision,OLD.committed_at,OLD.verified_at,
               OLD.write_base_revision,OLD.write_base_observed_at,
               OLD.next_attempt_at,OLD.consecutive_failures,OLD.last_failure_code,
               OLD.lease_owner,OLD.lease_epoch,OLD.lease_until,
               OLD.worker_contract,OLD.worker_config_digest,OLD.updated_at,
               OLD.completed_at) THEN
            RAISE EXCEPTION 'Argo desired-state terminal result is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.committed_revision<>'' AND
           ROW(NEW.committed_revision,NEW.committed_at)
           IS DISTINCT FROM ROW(OLD.committed_revision,OLD.committed_at) THEN
            RAISE EXCEPTION 'Argo desired-state Git receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.updated_at<OLD.updated_at OR
           (OLD.completed_at IS NOT NULL AND NEW.completed_at<>OLD.completed_at) THEN
            RAISE EXCEPTION 'Argo desired-state command time cannot regress'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER argo_desired_state_commands_validate
    BEFORE INSERT OR UPDATE ON argo_desired_state_commands
    FOR EACH ROW EXECUTE FUNCTION validate_argo_desired_state_command();

-- A public Argo capability requires a fresh worker with the exact protected
-- binding, non-secret runtime config, pinned chart identity, and a real digest
-- enforcement mechanism. Annotation-only Helm chart/version sources are
-- deliberately not an accepted enforcement mode here: Argo's generic OCI
-- source must use the exact digest as targetRevision.
CREATE TABLE argo_desired_state_runtime_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    contract_version text NOT NULL CHECK (
        length(contract_version) BETWEEN 8 AND 64 AND
        contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    config_digest text NOT NULL CHECK (
        config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    github_app_id bigint NOT NULL CHECK (github_app_id>0),
    platform_binding_id uuid NOT NULL,
    cluster_id uuid NOT NULL,
    argo_namespace text NOT NULL CHECK (
        argo_namespace ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'
    ),
    root_application_name text NOT NULL CHECK (
        root_application_name ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'
    ),
    repository_secret_name text NOT NULL CHECK (
        repository_secret_name ~ '^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$'
    ),
    chart_repository text NOT NULL CHECK (
        length(chart_repository) BETWEEN 7 AND 512 AND
        chart_repository ~ '^oci://[^/?#@[:space:]]+/[^?#@[:space:]]+$'
    ),
    chart_name text NOT NULL CHECK (chart_name='kuberploy-runtime'),
    chart_version text NOT NULL CHECK (
        length(chart_version) BETWEEN 5 AND 64 AND
        chart_version ~ '^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$'
    ),
    chart_digest text NOT NULL CHECK (
        chart_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    renderer_image text NOT NULL CHECK (
        length(renderer_image) BETWEEN 10 AND 512 AND
        renderer_image ~ '^[^[:space:]@]+@sha256:[0-9a-f]{64}$'
    ),
    chart_digest_enforcement text NOT NULL CHECK (
        chart_digest_enforcement='native-oci-digest-v1'
    ),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    FOREIGN KEY (platform_binding_id,cluster_id)
        REFERENCES git_repository_bindings(id,cluster_id) ON DELETE RESTRICT,
    CHECK (observed_at>=started_at AND lease_until>observed_at)
);

CREATE INDEX argo_desired_state_runtime_readiness_match_idx
    ON argo_desired_state_runtime_readiness(
        contract_version,config_digest,github_app_id,platform_binding_id,
        cluster_id,chart_digest,renderer_image,observed_at DESC
    );

CREATE OR REPLACE FUNCTION protect_argo_desired_state_runtime_readiness()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    binding_kind text;
    binding_mode text;
BEGIN
    SELECT kind,credential_mode INTO binding_kind,binding_mode
      FROM git_repository_bindings WHERE id=NEW.platform_binding_id;
    IF binding_kind IS DISTINCT FROM 'platform' OR
       binding_mode IS DISTINCT FROM 'github-app' THEN
        RAISE EXCEPTION 'Argo readiness requires a protected GitHub App platform binding'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF NEW.worker_id<>OLD.worker_id OR NEW.worker_epoch<OLD.worker_epoch OR
           NEW.worker_epoch>OLD.worker_epoch+1 THEN
            RAISE EXCEPTION 'Argo desired-state readiness epoch is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.worker_epoch=OLD.worker_epoch AND (
            NEW.contract_version<>OLD.contract_version OR
            NEW.config_digest<>OLD.config_digest OR
            NEW.github_app_id<>OLD.github_app_id OR
            NEW.platform_binding_id<>OLD.platform_binding_id OR
            NEW.cluster_id<>OLD.cluster_id OR
            NEW.argo_namespace<>OLD.argo_namespace OR
            NEW.root_application_name<>OLD.root_application_name OR
            NEW.repository_secret_name<>OLD.repository_secret_name OR
            NEW.chart_repository<>OLD.chart_repository OR
            NEW.chart_name<>OLD.chart_name OR
            NEW.chart_version<>OLD.chart_version OR
            NEW.chart_digest<>OLD.chart_digest OR
            NEW.renderer_image<>OLD.renderer_image OR
            NEW.chart_digest_enforcement<>OLD.chart_digest_enforcement OR
            NEW.started_at<>OLD.started_at OR NEW.observed_at<OLD.observed_at
        ) THEN
            RAISE EXCEPTION 'Argo desired-state readiness identity or time regressed'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER argo_desired_state_runtime_readiness_validate
    BEFORE INSERT OR UPDATE ON argo_desired_state_runtime_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_argo_desired_state_runtime_readiness();
