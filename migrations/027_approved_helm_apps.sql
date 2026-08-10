-- Approved external Helm applications are an immutable OCI-chart projection.
-- PostgreSQL stores no OCI username, token, credential reference, kubeconfig,
-- Helm flag bag, post-renderer, or dependency URL. The renderer consumes one
-- server-derived descriptor and one bounded values.yaml document.
CREATE TABLE helm_chart_approvals (
    approval_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision>0),
    oci_repository text NOT NULL CHECK (
        length(oci_repository) BETWEEN 12 AND 512 AND
        oci_repository=lower(oci_repository) AND
        oci_repository ~ '^oci://[a-z0-9][a-z0-9.-]*(?::[1-9][0-9]{0,4})?/[a-z0-9][a-z0-9._/-]*[a-z0-9]$' AND
        oci_repository !~ '[@?#[:space:]]' AND
        oci_repository !~ '(^|/)(\.|\.\.)($|/)'
    ),
    chart_version text NOT NULL CHECK (
        length(chart_version) BETWEEN 5 AND 128 AND
        chart_version ~ '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$'
    ),
    manifest_digest text NOT NULL CHECK (manifest_digest ~ '^sha256:[0-9a-f]{64}$'),
    package_digest text NOT NULL CHECK (package_digest ~ '^sha256:[0-9a-f]{64}$'),
    values_schema_digest text NOT NULL CHECK (values_schema_digest ~ '^sha256:[0-9a-f]{64}$'),
    renderer_image text NOT NULL CHECK (
        renderer_image='docker.io/alpine/helm:4.2.3'
    ),
    renderer_version text NOT NULL CHECK (renderer_version='4.2.3'),
    policy_version text NOT NULL CHECK (policy_version='external-helm-p0.v1'),
    identity_digest text NOT NULL CHECK (identity_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (
        length(idempotency_key) BETWEEN 16 AND 128 AND
        idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (approval_id,revision),
    UNIQUE (created_by,idempotency_key),
    UNIQUE (oci_repository,chart_version,manifest_digest,package_digest,
            values_schema_digest,renderer_image,policy_version)
);

CREATE OR REPLACE FUNCTION protect_helm_chart_approval()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Approved Helm chart revisions are immutable'
        USING ERRCODE='23514';
END;
$$;

CREATE TRIGGER helm_chart_approvals_immutable
    BEFORE UPDATE ON helm_chart_approvals
    FOR EACH ROW EXECUTE FUNCTION protect_helm_chart_approval();

CREATE TABLE helm_render_commands (
    id uuid PRIMARY KEY,
    idempotency_scope uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (
        length(idempotency_key) BETWEEN 16 AND 128 AND
        idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    approval_id uuid NOT NULL,
    approval_revision bigint NOT NULL,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    namespace text NOT NULL CHECK (
        length(namespace) BETWEEN 1 AND 63 AND
        namespace ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    release_name text NOT NULL CHECK (
        length(release_name) BETWEEN 1 AND 63 AND
        release_name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    descriptor_yaml bytea NOT NULL CHECK (octet_length(descriptor_yaml) BETWEEN 1 AND 32768),
    values_yaml bytea NOT NULL CHECK (octet_length(values_yaml) BETWEEN 1 AND 262144),
    descriptor_digest text NOT NULL CHECK (descriptor_digest ~ '^sha256:[0-9a-f]{64}$'),
    values_digest text NOT NULL CHECK (values_digest ~ '^sha256:[0-9a-f]{64}$'),
    input_digest text NOT NULL CHECK (input_digest ~ '^sha256:[0-9a-f]{64}$'),
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','processing','succeeded','failed')),
    available_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 10),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 10),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND
         lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    lease_until timestamptz,
    worker_contract text CHECK (worker_contract IS NULL OR worker_contract='external-helm-renderer.v1'),
    worker_renderer_image text CHECK (
        worker_renderer_image IS NULL OR
        worker_renderer_image='docker.io/alpine/helm:4.2.3'
    ),
    worker_renderer_version text CHECK (worker_renderer_version IS NULL OR worker_renderer_version='4.2.3'),
    worker_policy_version text CHECK (worker_policy_version IS NULL OR worker_policy_version='external-helm-p0.v1'),
    worker_limits_digest text CHECK (
        worker_limits_digest IS NULL OR worker_limits_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    UNIQUE (idempotency_scope,idempotency_key),
    FOREIGN KEY (approval_id,approval_revision)
        REFERENCES helm_chart_approvals(approval_id,revision) ON DELETE RESTRICT,
    FOREIGN KEY (environment_id,project_id)
        REFERENCES environments(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (application_id,project_id)
        REFERENCES applications(id,project_id) ON DELETE RESTRICT,
    CHECK (updated_at>=created_at AND available_at>=created_at),
    CHECK ((last_failure_code='')=(consecutive_failures=0)),
    CHECK (
        (lease_owner IS NULL AND lease_until IS NULL AND worker_contract IS NULL AND
         worker_renderer_image IS NULL AND worker_renderer_version IS NULL AND
         worker_policy_version IS NULL AND worker_limits_digest IS NULL) OR
        (lease_owner IS NOT NULL AND lease_until IS NOT NULL AND
         worker_contract='external-helm-renderer.v1' AND
         worker_renderer_image='docker.io/alpine/helm:4.2.3' AND
         worker_renderer_version='4.2.3' AND worker_policy_version='external-helm-p0.v1' AND
         worker_limits_digest IS NOT NULL AND lease_epoch>0 AND lease_until>updated_at)
    ),
    CHECK (
        (state='queued' AND lease_owner IS NULL AND completed_at IS NULL) OR
        (state='processing' AND lease_owner IS NOT NULL AND completed_at IS NULL AND attempts>0) OR
        (state IN ('succeeded','failed') AND lease_owner IS NULL AND
         completed_at IS NOT NULL AND completed_at>=created_at)
    )
);

CREATE INDEX helm_render_commands_due_idx
    ON helm_render_commands(available_at,created_at,id)
    WHERE state IN ('queued','processing');
CREATE INDEX helm_render_commands_approval_idx
    ON helm_render_commands(approval_id,approval_revision,created_at DESC);
CREATE INDEX helm_render_commands_application_idx
    ON helm_render_commands(environment_id,application_id,created_at DESC);

CREATE OR REPLACE FUNCTION validate_helm_render_command()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_namespace text;
    durable_release_name text;
BEGIN
    SELECT namespace INTO durable_namespace
      FROM environments WHERE id=NEW.environment_id FOR KEY SHARE;
    IF durable_namespace IS DISTINCT FROM NEW.namespace THEN
        RAISE EXCEPTION 'Helm render destination does not match its durable environment'
            USING ERRCODE='23514';
    END IF;
    SELECT slug INTO durable_release_name
      FROM applications
      WHERE id=NEW.application_id AND project_id=NEW.project_id
      FOR KEY SHARE;
    IF durable_release_name IS DISTINCT FROM NEW.release_name THEN
        RAISE EXCEPTION 'Helm render release does not match its durable application'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (
        NEW.state<>'queued' OR NEW.attempts<>0 OR
        NEW.consecutive_failures<>0 OR NEW.last_failure_code<>'' OR
        NEW.lease_owner IS NOT NULL OR NEW.lease_epoch<>0 OR
        NEW.lease_until IS NOT NULL OR NEW.worker_contract IS NOT NULL OR
        NEW.worker_renderer_image IS NOT NULL OR
        NEW.worker_renderer_version IS NOT NULL OR
        NEW.worker_policy_version IS NOT NULL OR
        NEW.worker_limits_digest IS NOT NULL OR NEW.completed_at IS NOT NULL OR
        NEW.available_at<>NEW.created_at OR NEW.updated_at<>NEW.created_at
    ) THEN
        RAISE EXCEPTION 'Helm render commands must be inserted as pristine queued work'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF OLD.state IN ('succeeded','failed') THEN
            RAISE EXCEPTION 'Terminal Helm render commands are immutable'
                USING ERRCODE='23514';
        END IF;
        IF ROW(NEW.id,NEW.idempotency_scope,NEW.idempotency_key,
               NEW.approval_id,NEW.approval_revision,NEW.project_id,
               NEW.environment_id,NEW.application_id,NEW.namespace,
               NEW.release_name,NEW.descriptor_yaml,NEW.values_yaml,
               NEW.descriptor_digest,NEW.values_digest,NEW.input_digest,
               NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.id,OLD.idempotency_scope,OLD.idempotency_key,
               OLD.approval_id,OLD.approval_revision,OLD.project_id,
               OLD.environment_id,OLD.application_id,OLD.namespace,
               OLD.release_name,OLD.descriptor_yaml,OLD.values_yaml,
               OLD.descriptor_digest,OLD.values_digest,OLD.input_digest,
               OLD.created_at) THEN
            RAISE EXCEPTION 'Helm render command identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 OR
           NEW.attempts<OLD.attempts OR NEW.attempts>OLD.attempts+1 OR
           NEW.consecutive_failures<OLD.consecutive_failures OR
           NEW.updated_at<OLD.updated_at THEN
            RAISE EXCEPTION 'Helm render command fencing or time regressed'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           NEW.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_renderer_image,
               NEW.worker_renderer_version,NEW.worker_policy_version,
               NEW.worker_limits_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_renderer_image,
               OLD.worker_renderer_version,OLD.worker_policy_version,
               OLD.worker_limits_digest) THEN
            RAISE EXCEPTION 'Helm render lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_render_commands_validate
    BEFORE INSERT OR UPDATE ON helm_render_commands
    FOR EACH ROW EXECUTE FUNCTION validate_helm_render_command();

CREATE TABLE helm_render_results (
    command_id uuid PRIMARY KEY REFERENCES helm_render_commands(id) ON DELETE RESTRICT,
    input_digest text NOT NULL CHECK (input_digest ~ '^sha256:[0-9a-f]{64}$'),
    manifest_digest text NOT NULL CHECK (manifest_digest ~ '^sha256:[0-9a-f]{64}$'),
    inventory_digest text NOT NULL CHECK (inventory_digest ~ '^sha256:[0-9a-f]{64}$'),
    rendered_manifests bytea NOT NULL CHECK (octet_length(rendered_manifests) BETWEEN 1 AND 2097152),
    resource_count integer NOT NULL CHECK (resource_count BETWEEN 1 AND 128),
    output_bytes integer NOT NULL CHECK (
        output_bytes BETWEEN 1 AND 2097152 AND
        output_bytes=octet_length(rendered_manifests)
    ),
    renderer_image text NOT NULL CHECK (
        renderer_image='docker.io/alpine/helm:4.2.3'
    ),
    renderer_version text NOT NULL CHECK (renderer_version='4.2.3'),
    policy_version text NOT NULL CHECK (policy_version='external-helm-p0.v1'),
    limits_digest text NOT NULL CHECK (limits_digest ~ '^sha256:[0-9a-f]{64}$'),
    completed_at timestamptz NOT NULL
);

CREATE OR REPLACE FUNCTION validate_helm_render_result()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_input text;
    durable_state text;
BEGIN
    SELECT input_digest,state INTO durable_input,durable_state
      FROM helm_render_commands WHERE id=NEW.command_id FOR KEY SHARE;
    IF durable_state IS DISTINCT FROM 'processing' OR
       durable_input IS DISTINCT FROM NEW.input_digest THEN
        RAISE EXCEPTION 'Helm render result does not match one processing command'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        RAISE EXCEPTION 'Helm render results are immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_render_results_validate
    BEFORE INSERT OR UPDATE ON helm_render_results
    FOR EACH ROW EXECUTE FUNCTION validate_helm_render_result();

-- No capability is wired by this migration. A later API seam may consume only
-- a fresh exact row matching every pinned renderer and policy identity field.
CREATE TABLE helm_renderer_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    contract_version text NOT NULL CHECK (contract_version='external-helm-renderer.v1'),
    renderer_image text NOT NULL CHECK (
        renderer_image='docker.io/alpine/helm:4.2.3'
    ),
    renderer_version text NOT NULL CHECK (renderer_version='4.2.3'),
    policy_version text NOT NULL CHECK (policy_version='external-helm-p0.v1'),
    limits_digest text NOT NULL CHECK (limits_digest ~ '^sha256:[0-9a-f]{64}$'),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    CHECK (observed_at>=started_at AND lease_until>observed_at)
);

CREATE INDEX helm_renderer_readiness_match_idx
    ON helm_renderer_readiness(contract_version,renderer_image,renderer_version,
                               policy_version,limits_digest,observed_at DESC);

CREATE OR REPLACE FUNCTION protect_helm_renderer_readiness_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id<>OLD.worker_id OR NEW.worker_epoch<OLD.worker_epoch OR
       NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Helm renderer readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND (
        NEW.contract_version<>OLD.contract_version OR
        NEW.renderer_image<>OLD.renderer_image OR
        NEW.renderer_version<>OLD.renderer_version OR
        NEW.policy_version<>OLD.policy_version OR
        NEW.limits_digest<>OLD.limits_digest OR
        NEW.started_at<>OLD.started_at OR NEW.observed_at<OLD.observed_at
    ) THEN
        RAISE EXCEPTION 'Helm renderer readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_renderer_readiness_epoch
    BEFORE UPDATE ON helm_renderer_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_helm_renderer_readiness_epoch();
