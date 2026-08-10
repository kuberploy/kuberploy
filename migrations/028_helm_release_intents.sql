-- Durable desired-release and protected-Git intent state for approved external
-- Helm applications. Migration 027 remains the immutable chart/render core.
--
-- Publication is deliberately two phase. First, rendered bytes (or a disabled
-- receipt) are committed at a revision-unique path outside Argo's recursive
-- root and provider-verified. Only then may a second intent publish the stable
-- Argo Application, pinned to the first phase's exact commit. A mutable branch
-- can therefore never replace the protected payload Argo consumes.

CREATE TABLE helm_chart_approval_documents (
    approval_id uuid NOT NULL,
    approval_revision bigint NOT NULL,
    values_schema_json bytea NOT NULL CHECK (
        octet_length(values_schema_json) BETWEEN 2 AND 524288
    ),
    default_values_yaml bytea NOT NULL CHECK (
        octet_length(default_values_yaml) BETWEEN 1 AND 262144
    ),
    values_schema_digest text NOT NULL CHECK (
        values_schema_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    documents_digest text NOT NULL CHECK (
        documents_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (approval_id,approval_revision),
    FOREIGN KEY (approval_id,approval_revision)
        REFERENCES helm_chart_approvals(approval_id,revision) ON DELETE RESTRICT
);

CREATE OR REPLACE FUNCTION protect_helm_chart_approval_documents()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Approved Helm UI documents are immutable'
        USING ERRCODE='23514';
END;
$$;

CREATE TRIGGER helm_chart_approval_documents_immutable
    BEFORE UPDATE OR DELETE ON helm_chart_approval_documents
    FOR EACH ROW EXECUTE FUNCTION protect_helm_chart_approval_documents();

CREATE OR REPLACE FUNCTION validate_helm_chart_approval_document_insert()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE durable_schema_digest text;
BEGIN
    SELECT values_schema_digest INTO durable_schema_digest
      FROM helm_chart_approvals
     WHERE approval_id=NEW.approval_id
       AND revision=NEW.approval_revision
     FOR KEY SHARE;
    IF durable_schema_digest IS DISTINCT FROM NEW.values_schema_digest THEN
        RAISE EXCEPTION 'Approved Helm schema document digest does not match approval'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_chart_approval_documents_validate_insert
    BEFORE INSERT ON helm_chart_approval_documents
    FOR EACH ROW EXECUTE FUNCTION validate_helm_chart_approval_document_insert();

CREATE TABLE helm_release_revisions (
    id uuid PRIMARY KEY,
    generation bigint NOT NULL CHECK (generation>0),
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    release_name text NOT NULL CHECK (
        length(release_name) BETWEEN 1 AND 63 AND
        release_name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    action text NOT NULL CHECK (
        action IN ('initial','update','retry','disable','rollback')
    ),
    desired_enabled boolean NOT NULL,
    parent_revision_id uuid REFERENCES helm_release_revisions(id) ON DELETE RESTRICT,
    rollback_source_revision_id uuid REFERENCES helm_release_revisions(id) ON DELETE RESTRICT,
    -- The latest verified, live stable Application before this desired change.
    -- It is NULL only while that stable Application is known absent.
    base_intent_id uuid,
    approval_id uuid NOT NULL,
    approval_revision bigint NOT NULL,
    render_command_id uuid UNIQUE REFERENCES helm_render_commands(id) ON DELETE RESTRICT,
    values_yaml bytea NOT NULL CHECK (
        octet_length(values_yaml) BETWEEN 1 AND 262144
    ),
    values_digest text NOT NULL CHECK (
        values_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    intent_digest text NOT NULL CHECK (
        intent_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (
        length(idempotency_key) BETWEEN 16 AND 128 AND
        idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    request_id text NOT NULL CHECK (
        length(request_id) BETWEEN 1 AND 128 AND
        request_id !~ '[[:cntrl:]]'
    ),
    created_at timestamptz NOT NULL,
    UNIQUE (environment_id,application_id,generation),
    UNIQUE (actor_id,idempotency_key),
    UNIQUE (id,project_id,environment_id,application_id,generation),
    FOREIGN KEY (environment_id,project_id)
        REFERENCES environments(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (application_id,project_id)
        REFERENCES applications(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (approval_id,approval_revision)
        REFERENCES helm_chart_approvals(approval_id,revision) ON DELETE RESTRICT,
    CHECK (desired_enabled=(action<>'disable')),
    CHECK ((render_command_id IS NOT NULL)=desired_enabled),
    CHECK (
        (action='initial' AND generation=1 AND parent_revision_id IS NULL AND
         rollback_source_revision_id IS NULL AND base_intent_id IS NULL) OR
        (action IN ('update','retry') AND generation>1 AND
         parent_revision_id IS NOT NULL AND rollback_source_revision_id IS NULL) OR
        (action='disable' AND generation>1 AND parent_revision_id IS NOT NULL AND
         rollback_source_revision_id IS NULL AND base_intent_id IS NOT NULL) OR
        (action='rollback' AND generation>1 AND parent_revision_id IS NOT NULL AND
         rollback_source_revision_id IS NOT NULL)
    ),
    CHECK (
        rollback_source_revision_id IS NULL OR
        rollback_source_revision_id<>parent_revision_id
    )
);

CREATE TABLE helm_release_heads (
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    revision_id uuid NOT NULL,
    generation bigint NOT NULL CHECK (generation>0),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (environment_id,application_id),
    FOREIGN KEY (environment_id,project_id)
        REFERENCES environments(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (application_id,project_id)
        REFERENCES applications(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (revision_id,project_id,environment_id,application_id,generation)
        REFERENCES helm_release_revisions(
            id,project_id,environment_id,application_id,generation
        ) ON DELETE RESTRICT
);

-- Phase one: write one immutable payload path. The stable Argo Application
-- cannot reference this payload until this row reaches provider-verified.
CREATE TABLE helm_protected_payload_intents (
    id uuid PRIMARY KEY,
    release_revision_id uuid NOT NULL UNIQUE,
    release_generation bigint NOT NULL CHECK (release_generation>0),
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    action text NOT NULL CHECK (action IN ('publish','disable-receipt')),
    platform_binding_id uuid NOT NULL,
    environment_binding_id uuid NOT NULL,
    cluster_id uuid NOT NULL,
    platform_target_ref text NOT NULL CHECK (
        length(platform_target_ref) BETWEEN 1 AND 255 AND
        platform_target_ref !~ '[[:cntrl:]]'
    ),
    environment_target_ref text NOT NULL CHECK (
        length(environment_target_ref) BETWEEN 1 AND 255 AND
        environment_target_ref !~ '[[:cntrl:]]'
    ),
    environment_revision text NOT NULL CHECK (
        environment_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    environment_generation bigint NOT NULL CHECK (environment_generation>0),
    catalog_digest text NOT NULL CHECK (
        catalog_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    planned_base_revision text NOT NULL CHECK (
        planned_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    path text NOT NULL CHECK (
        length(path) BETWEEN 1 AND 1024 AND
        path !~ '(^/|/\.\.?(/|$)|//|\\|[[:cntrl:]])'
    ),
    precondition text NOT NULL CHECK (precondition='create-if-absent'),
    expected_etag text NOT NULL CHECK (expected_etag=''),
    content bytea NOT NULL CHECK (
        octet_length(content) BETWEEN 1 AND 2097152
    ),
    content_digest text NOT NULL CHECK (
        content_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    manifest_inventory_digest text CHECK (
        manifest_inventory_digest IS NULL OR
        manifest_inventory_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    manifest_resource_count integer CHECK (
        manifest_resource_count IS NULL OR
        manifest_resource_count BETWEEN 1 AND 128
    ),
    intent_digest text NOT NULL CHECK (
        intent_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    commit_trailer text NOT NULL CHECK (
        commit_trailer='Kuberploy-Helm-Payload-Intent: '||id::text
    ),
    publisher_contract text NOT NULL CHECK (
        publisher_contract='helm-protected-publisher.v1'
    ),
    publisher_config_digest text NOT NULL CHECK (
        publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    message text NOT NULL CHECK (
        length(message) BETWEEN 1 AND 512 AND
        message !~ '[[:cntrl:]]'
    ),
    state text NOT NULL CHECK (
        state IN ('pending','claimed','git-committed','verified','failed','superseded')
    ),
    next_attempt_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 30),
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
    write_base_revision text NOT NULL DEFAULT '' CHECK (
        write_base_revision='' OR
        write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    write_base_observed_at timestamptz,
    committed_revision text NOT NULL DEFAULT '' CHECK (
        committed_revision='' OR
        committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_parent_revision text NOT NULL DEFAULT '' CHECK (
        committed_parent_revision='' OR
        committed_parent_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_at timestamptz,
    verified_at timestamptz,
    verified_path_digest text NOT NULL DEFAULT '' CHECK (
        verified_path_digest='' OR
        verified_path_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    provider_request text NOT NULL DEFAULT '' CHECK (
        length(provider_request)<=256 AND
        provider_request !~ '[[:cntrl:]]'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    UNIQUE (environment_id,application_id,release_generation),
    FOREIGN KEY (
        release_revision_id,project_id,environment_id,application_id,release_generation
    ) REFERENCES helm_release_revisions(
        id,project_id,environment_id,application_id,generation
    ) ON DELETE RESTRICT,
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
    CHECK ((last_failure_code='')=(consecutive_failures=0)),
    CHECK (
        (write_base_revision='' AND write_base_observed_at IS NULL) OR
        (write_base_revision<>'' AND write_base_observed_at IS NOT NULL AND
         write_base_observed_at>=created_at AND
         write_base_observed_at<=updated_at)
    ),
    CHECK (
        (action='publish' AND manifest_inventory_digest IS NOT NULL AND
         manifest_resource_count IS NOT NULL) OR
        (action='disable-receipt' AND manifest_inventory_digest IS NULL AND
         manifest_resource_count IS NULL)
    ),
    CHECK (
        (lease_owner IS NULL AND lease_until IS NULL) OR
        (lease_owner IS NOT NULL AND lease_until IS NOT NULL AND
         lease_epoch>0 AND lease_until>updated_at)
    ),
    CHECK (
        (state='pending' AND lease_owner IS NULL AND committed_revision='' AND
         committed_parent_revision='' AND committed_at IS NULL AND
         verified_at IS NULL AND verified_path_digest='' AND
         provider_request='' AND completed_at IS NULL) OR
        (state='claimed' AND lease_owner IS NOT NULL AND attempts>0 AND
         committed_revision='' AND committed_parent_revision='' AND
         committed_at IS NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NULL) OR
        (state='git-committed' AND lease_owner IS NOT NULL AND
         write_base_revision<>'' AND committed_revision<>'' AND
         committed_parent_revision=write_base_revision AND
         committed_at IS NOT NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NULL) OR
        (state='verified' AND lease_owner IS NULL AND
         write_base_revision<>'' AND committed_revision<>'' AND
         committed_parent_revision=write_base_revision AND
         committed_at IS NOT NULL AND verified_at IS NOT NULL AND
         verified_at>=committed_at AND verified_path_digest=content_digest AND
         provider_request<>'' AND completed_at=verified_at) OR
        (state='failed' AND lease_owner IS NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NOT NULL AND completed_at>=created_at AND
         committed_revision='' AND committed_parent_revision='' AND
         committed_at IS NULL) OR
        (state='superseded' AND lease_owner IS NULL AND
         committed_revision='' AND committed_parent_revision='' AND
         committed_at IS NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NOT NULL AND completed_at>=created_at)
    )
);

-- Phase two: after the payload commit is provider-verified, create/update/delete
-- the one stable Argo Application. Published content must be constructed by the
-- typed Go adapter and pins source_revision to the exact phase-one commit.
CREATE TABLE helm_protected_application_intents (
    id uuid PRIMARY KEY,
    release_revision_id uuid NOT NULL UNIQUE,
    payload_intent_id uuid NOT NULL UNIQUE
        REFERENCES helm_protected_payload_intents(id) ON DELETE RESTRICT,
    release_generation bigint NOT NULL CHECK (release_generation>0),
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    action text NOT NULL CHECK (action IN ('publish','delete')),
    platform_binding_id uuid NOT NULL,
    environment_binding_id uuid NOT NULL,
    cluster_id uuid NOT NULL,
    platform_target_ref text NOT NULL CHECK (
        length(platform_target_ref) BETWEEN 1 AND 255 AND
        platform_target_ref !~ '[[:cntrl:]]'
    ),
    environment_target_ref text NOT NULL CHECK (
        length(environment_target_ref) BETWEEN 1 AND 255 AND
        environment_target_ref !~ '[[:cntrl:]]'
    ),
    environment_revision text NOT NULL CHECK (
        environment_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    environment_generation bigint NOT NULL CHECK (environment_generation>0),
    catalog_digest text NOT NULL CHECK (
        catalog_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    planned_base_revision text NOT NULL CHECK (
        planned_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    payload_revision text NOT NULL CHECK (
        payload_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    payload_path text NOT NULL CHECK (
        length(payload_path) BETWEEN 1 AND 1024 AND
        payload_path !~ '(^/|/\.\.?(/|$)|//|\\|[[:cntrl:]])'
    ),
    source_directory text NOT NULL CHECK (
        length(source_directory)<=1024 AND
        source_directory !~ '(^/|/\.\.?(/|$)|//|\\|[[:cntrl:]])'
    ),
    application_path text NOT NULL CHECK (
        length(application_path) BETWEEN 1 AND 1024 AND
        application_path !~ '(^/|/\.\.?(/|$)|//|\\|[[:cntrl:]])'
    ),
    operation text NOT NULL CHECK (operation IN ('create','update','delete')),
    precondition text NOT NULL CHECK (
        precondition IN ('create-if-absent','match-etag')
    ),
    expected_etag text NOT NULL CHECK (
        (precondition='create-if-absent' AND expected_etag='') OR
        (precondition='match-etag' AND
         expected_etag ~ '^"sha256:[0-9a-f]{64}"$')
    ),
    content bytea NOT NULL CHECK (octet_length(content) BETWEEN 0 AND 32768),
    content_digest text NOT NULL CHECK (
        content_digest='' OR content_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    intent_digest text NOT NULL CHECK (
        intent_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    commit_trailer text NOT NULL CHECK (
        commit_trailer='Kuberploy-Helm-Application-Intent: '||id::text
    ),
    publisher_contract text NOT NULL CHECK (
        publisher_contract='helm-protected-publisher.v1'
    ),
    publisher_config_digest text NOT NULL CHECK (
        publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    message text NOT NULL CHECK (
        length(message) BETWEEN 1 AND 512 AND
        message !~ '[[:cntrl:]]'
    ),
    state text NOT NULL CHECK (
        state IN ('pending','claimed','git-committed','verified','failed','superseded')
    ),
    next_attempt_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 30),
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
    write_base_revision text NOT NULL DEFAULT '' CHECK (
        write_base_revision='' OR
        write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    write_base_observed_at timestamptz,
    committed_revision text NOT NULL DEFAULT '' CHECK (
        committed_revision='' OR
        committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_parent_revision text NOT NULL DEFAULT '' CHECK (
        committed_parent_revision='' OR
        committed_parent_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_at timestamptz,
    verified_at timestamptz,
    verified_path_digest text NOT NULL DEFAULT '' CHECK (
        verified_path_digest='' OR
        verified_path_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    provider_request text NOT NULL DEFAULT '' CHECK (
        length(provider_request)<=256 AND
        provider_request !~ '[[:cntrl:]]'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    UNIQUE (environment_id,application_id,release_generation),
    FOREIGN KEY (
        release_revision_id,project_id,environment_id,application_id,release_generation
    ) REFERENCES helm_release_revisions(
        id,project_id,environment_id,application_id,generation
    ) ON DELETE RESTRICT,
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
    CHECK ((last_failure_code='')=(consecutive_failures=0)),
    CHECK (
        (write_base_revision='' AND write_base_observed_at IS NULL) OR
        (write_base_revision<>'' AND write_base_observed_at IS NOT NULL AND
         write_base_observed_at>=created_at AND
         write_base_observed_at<=updated_at)
    ),
    CHECK (
        (action='publish' AND operation IN ('create','update') AND
         source_directory<>'' AND octet_length(content)>0 AND
         content_digest<>'') OR
        (action='delete' AND operation='delete' AND source_directory='' AND
         octet_length(content)=0 AND content_digest='')
    ),
    CHECK (
        (operation='create' AND precondition='create-if-absent') OR
        (operation IN ('update','delete') AND precondition='match-etag')
    ),
    CHECK (
        (lease_owner IS NULL AND lease_until IS NULL) OR
        (lease_owner IS NOT NULL AND lease_until IS NOT NULL AND
         lease_epoch>0 AND lease_until>updated_at)
    ),
    CHECK (
        (state='pending' AND lease_owner IS NULL AND committed_revision='' AND
         committed_parent_revision='' AND committed_at IS NULL AND
         verified_at IS NULL AND verified_path_digest='' AND
         provider_request='' AND completed_at IS NULL) OR
        (state='claimed' AND lease_owner IS NOT NULL AND attempts>0 AND
         committed_revision='' AND committed_parent_revision='' AND
         committed_at IS NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NULL) OR
        (state='git-committed' AND lease_owner IS NOT NULL AND
         write_base_revision<>'' AND committed_revision<>'' AND
         committed_parent_revision=write_base_revision AND
         committed_at IS NOT NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NULL) OR
        (state='verified' AND lease_owner IS NULL AND
         write_base_revision<>'' AND committed_revision<>'' AND
         committed_parent_revision=write_base_revision AND
         committed_at IS NOT NULL AND verified_at IS NOT NULL AND
         verified_at>=committed_at AND verified_path_digest=content_digest AND
         provider_request<>'' AND completed_at=verified_at) OR
        (state='failed' AND lease_owner IS NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NOT NULL AND completed_at>=created_at AND
         committed_revision='' AND committed_parent_revision='' AND
         committed_at IS NULL) OR
        (state='superseded' AND lease_owner IS NULL AND
         committed_revision='' AND committed_parent_revision='' AND
         committed_at IS NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NOT NULL AND completed_at>=created_at)
    )
);

ALTER TABLE helm_release_revisions
    ADD CONSTRAINT helm_release_revisions_base_intent_fk
    FOREIGN KEY (base_intent_id)
    REFERENCES helm_protected_application_intents(id) ON DELETE RESTRICT;

CREATE INDEX helm_release_revisions_history_idx
    ON helm_release_revisions(environment_id,application_id,generation DESC);
CREATE INDEX helm_protected_payload_intents_due_idx
    ON helm_protected_payload_intents(next_attempt_at,created_at,id)
    WHERE state IN ('pending','claimed','git-committed');
CREATE INDEX helm_protected_payload_intents_binding_idx
    ON helm_protected_payload_intents(platform_binding_id,state,created_at);
CREATE INDEX helm_protected_application_intents_due_idx
    ON helm_protected_application_intents(next_attempt_at,created_at,id)
    WHERE state IN ('pending','claimed','git-committed');
CREATE INDEX helm_protected_application_intents_binding_idx
    ON helm_protected_application_intents(platform_binding_id,state,created_at);

CREATE OR REPLACE FUNCTION validate_helm_release_revision()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_namespace text;
    durable_release_name text;
    head_revision uuid;
    head_generation bigint;
    parent_row helm_release_revisions%ROWTYPE;
    rollback_row helm_release_revisions%ROWTYPE;
    base_row helm_protected_application_intents%ROWTYPE;
    latest_application_id uuid;
    latest_application_action text;
    command_approval_id uuid;
    command_approval_revision bigint;
    command_project_id uuid;
    command_environment_id uuid;
    command_application_id uuid;
    command_release_name text;
    command_values bytea;
    command_values_digest text;
    command_state text;
    command_attempts integer;
BEGIN
    IF TG_OP IN ('UPDATE','DELETE') THEN
        RAISE EXCEPTION 'Helm desired-release revisions are immutable'
            USING ERRCODE='23514';
    END IF;

    SELECT namespace INTO durable_namespace
      FROM environments
     WHERE id=NEW.environment_id AND project_id=NEW.project_id
     FOR KEY SHARE;
    SELECT slug INTO durable_release_name
      FROM applications
     WHERE id=NEW.application_id AND project_id=NEW.project_id
     FOR KEY SHARE;
    IF durable_namespace IS NULL OR durable_release_name IS NULL OR
       durable_release_name IS DISTINCT FROM NEW.release_name THEN
        RAISE EXCEPTION 'Helm release destination is not the durable application environment'
            USING ERRCODE='23514';
    END IF;

    SELECT revision_id,generation INTO head_revision,head_generation
      FROM helm_release_heads
     WHERE environment_id=NEW.environment_id
       AND application_id=NEW.application_id
     FOR UPDATE;
    IF NEW.generation=1 THEN
        IF head_revision IS NOT NULL OR NEW.action<>'initial' OR
           NEW.parent_revision_id IS NOT NULL THEN
            RAISE EXCEPTION 'Initial Helm release revision conflicts with durable head'
                USING ERRCODE='23514';
        END IF;
    ELSIF head_revision IS NULL OR
          NEW.parent_revision_id IS DISTINCT FROM head_revision OR
          NEW.generation<>head_generation+1 THEN
        RAISE EXCEPTION 'Helm release revision does not advance the exact durable head'
            USING ERRCODE='23514';
    END IF;
    IF NEW.generation>1 AND EXISTS (
        SELECT 1 FROM helm_protected_application_intents
         WHERE release_revision_id=head_revision
           AND state IN ('pending','claimed','git-committed')
    ) THEN
        RAISE EXCEPTION 'Helm release cannot advance while stable Application publication is unresolved'
            USING ERRCODE='23514';
    END IF;

    IF NEW.parent_revision_id IS NOT NULL THEN
        SELECT * INTO parent_row
          FROM helm_release_revisions
         WHERE id=NEW.parent_revision_id
         FOR KEY SHARE;
        IF parent_row.id IS NULL OR parent_row.project_id<>NEW.project_id OR
           parent_row.environment_id<>NEW.environment_id OR
           parent_row.application_id<>NEW.application_id THEN
            RAISE EXCEPTION 'Helm release parent escaped its application environment'
                USING ERRCODE='23514';
        END IF;
    END IF;

    IF NEW.rollback_source_revision_id IS NOT NULL THEN
        SELECT * INTO rollback_row
          FROM helm_release_revisions
         WHERE id=NEW.rollback_source_revision_id
         FOR KEY SHARE;
        IF rollback_row.id IS NULL OR rollback_row.project_id<>NEW.project_id OR
           rollback_row.environment_id<>NEW.environment_id OR
           rollback_row.application_id<>NEW.application_id OR
           rollback_row.generation>=NEW.generation OR
           NOT rollback_row.desired_enabled THEN
            RAISE EXCEPTION 'Helm rollback source escaped its enabled release history'
                USING ERRCODE='23514';
        END IF;
    END IF;

    SELECT id,action INTO latest_application_id,latest_application_action
      FROM helm_protected_application_intents
     WHERE environment_id=NEW.environment_id
       AND application_id=NEW.application_id
       AND state='verified'
     ORDER BY release_generation DESC
     LIMIT 1
     FOR KEY SHARE;
    IF latest_application_action='delete' THEN
        latest_application_id := NULL;
    END IF;
    IF NEW.base_intent_id IS DISTINCT FROM latest_application_id THEN
        RAISE EXCEPTION 'Helm release base is not the latest verified live Application'
            USING ERRCODE='23514';
    END IF;
    IF NEW.base_intent_id IS NOT NULL THEN
        SELECT * INTO base_row
          FROM helm_protected_application_intents
         WHERE id=NEW.base_intent_id
         FOR KEY SHARE;
        IF base_row.id IS NULL OR base_row.state<>'verified' OR
           base_row.action<>'publish' OR base_row.project_id<>NEW.project_id OR
           base_row.environment_id<>NEW.environment_id OR
           base_row.application_id<>NEW.application_id OR
           base_row.release_generation>=NEW.generation THEN
            RAISE EXCEPTION 'Helm release base is not one prior verified publication'
                USING ERRCODE='23514';
        END IF;
    END IF;

    IF NEW.desired_enabled THEN
        SELECT approval_id,approval_revision,project_id,environment_id,
               application_id,release_name,values_yaml,values_digest,state,attempts
          INTO command_approval_id,command_approval_revision,command_project_id,
               command_environment_id,command_application_id,command_release_name,
               command_values,command_values_digest,command_state,command_attempts
          FROM helm_render_commands
         WHERE id=NEW.render_command_id
         FOR KEY SHARE;
        IF command_approval_id IS NULL OR
           command_approval_id<>NEW.approval_id OR
           command_approval_revision<>NEW.approval_revision OR
           command_project_id<>NEW.project_id OR
           command_environment_id<>NEW.environment_id OR
           command_application_id<>NEW.application_id OR
           command_release_name<>NEW.release_name OR
           command_values IS DISTINCT FROM NEW.values_yaml OR
           command_values_digest<>NEW.values_digest OR
           command_state<>'queued' OR command_attempts<>0 THEN
            RAISE EXCEPTION 'Helm release revision does not match one pristine render command'
                USING ERRCODE='23514';
        END IF;
    END IF;

    IF NEW.action IN ('retry','disable') AND (
        parent_row.approval_id<>NEW.approval_id OR
        parent_row.approval_revision<>NEW.approval_revision OR
        parent_row.values_yaml IS DISTINCT FROM NEW.values_yaml OR
        parent_row.values_digest<>NEW.values_digest
    ) THEN
        RAISE EXCEPTION 'Helm retry/disable must copy the exact parent desired input'
            USING ERRCODE='23514';
    END IF;
    IF NEW.action='rollback' AND (
        rollback_row.approval_id<>NEW.approval_id OR
        rollback_row.approval_revision<>NEW.approval_revision OR
        rollback_row.values_yaml IS DISTINCT FROM NEW.values_yaml OR
        rollback_row.values_digest<>NEW.values_digest
    ) THEN
        RAISE EXCEPTION 'Helm rollback must copy the exact selected historical input'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_release_revisions_validate
    BEFORE INSERT OR UPDATE OR DELETE ON helm_release_revisions
    FOR EACH ROW EXECUTE FUNCTION validate_helm_release_revision();

CREATE OR REPLACE FUNCTION validate_helm_release_head()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE revision_row helm_release_revisions%ROWTYPE;
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'Helm release heads cannot be deleted'
            USING ERRCODE='23514';
    END IF;
    SELECT * INTO revision_row
      FROM helm_release_revisions
     WHERE id=NEW.revision_id
     FOR KEY SHARE;
    IF revision_row.id IS NULL OR revision_row.project_id<>NEW.project_id OR
       revision_row.environment_id<>NEW.environment_id OR
       revision_row.application_id<>NEW.application_id OR
       revision_row.generation<>NEW.generation THEN
        RAISE EXCEPTION 'Helm release head does not identify its exact revision'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND NEW.generation<>1 THEN
        RAISE EXCEPTION 'Helm release head must begin at generation one'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND (
        NEW.project_id<>OLD.project_id OR
        NEW.environment_id<>OLD.environment_id OR
        NEW.application_id<>OLD.application_id OR
        NEW.generation<>OLD.generation+1 OR
        revision_row.parent_revision_id IS DISTINCT FROM OLD.revision_id OR
        NEW.updated_at<=OLD.updated_at
    ) THEN
        RAISE EXCEPTION 'Helm release head update skipped or changed identity'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_release_heads_validate
    BEFORE INSERT OR UPDATE OR DELETE ON helm_release_heads
    FOR EACH ROW EXECUTE FUNCTION validate_helm_release_head();

CREATE OR REPLACE FUNCTION validate_helm_protected_payload_intent()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    release_row helm_release_revisions%ROWTYPE;
    platform_row git_repository_bindings%ROWTYPE;
    environment_row git_repository_bindings%ROWTYPE;
    result_manifest bytea;
    result_digest text;
    result_inventory text;
    result_count integer;
    command_state text;
    expected_path text;
    disabled_receipt jsonb;
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'Helm protected payload intents cannot be deleted'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF OLD.state IN ('verified','failed','superseded') THEN
            RAISE EXCEPTION 'Terminal Helm protected payload intents are immutable'
                USING ERRCODE='23514';
        END IF;
        IF ROW(
            NEW.id,NEW.release_revision_id,NEW.release_generation,NEW.project_id,
            NEW.environment_id,NEW.application_id,NEW.action,
            NEW.platform_binding_id,NEW.environment_binding_id,NEW.cluster_id,
            NEW.platform_target_ref,NEW.environment_target_ref,
            NEW.environment_revision,NEW.environment_generation,NEW.catalog_digest,
            NEW.planned_base_revision,NEW.path,NEW.precondition,NEW.expected_etag,
            NEW.content,NEW.content_digest,NEW.manifest_inventory_digest,
            NEW.manifest_resource_count,NEW.intent_digest,NEW.commit_trailer,
            NEW.publisher_contract,NEW.publisher_config_digest,NEW.message,
            NEW.created_at
        ) IS DISTINCT FROM ROW(
            OLD.id,OLD.release_revision_id,OLD.release_generation,OLD.project_id,
            OLD.environment_id,OLD.application_id,OLD.action,
            OLD.platform_binding_id,OLD.environment_binding_id,OLD.cluster_id,
            OLD.platform_target_ref,OLD.environment_target_ref,
            OLD.environment_revision,OLD.environment_generation,OLD.catalog_digest,
            OLD.planned_base_revision,OLD.path,OLD.precondition,OLD.expected_etag,
            OLD.content,OLD.content_digest,OLD.manifest_inventory_digest,
            OLD.manifest_resource_count,OLD.intent_digest,OLD.commit_trailer,
            OLD.publisher_contract,OLD.publisher_config_digest,OLD.message,
            OLD.created_at
        ) THEN
            RAISE EXCEPTION 'Helm protected payload intent identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR
           NEW.lease_epoch>OLD.lease_epoch+1 OR
           NEW.attempts<OLD.attempts OR NEW.attempts>OLD.attempts+1 OR
           NEW.consecutive_failures<OLD.consecutive_failures OR
           NEW.updated_at<OLD.updated_at THEN
            RAISE EXCEPTION 'Helm protected payload fencing or time regressed'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND
           OLD.lease_owner IS NOT NULL AND NEW.lease_owner IS NOT NULL AND
           NEW.lease_owner<>OLD.lease_owner THEN
            RAISE EXCEPTION 'Helm protected payload lease owner changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.committed_revision<>'' AND ROW(
            NEW.committed_revision,NEW.committed_parent_revision,NEW.committed_at
        ) IS DISTINCT FROM ROW(
            OLD.committed_revision,OLD.committed_parent_revision,OLD.committed_at
        ) THEN
            RAISE EXCEPTION 'Helm protected payload commit receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.verified_at IS NOT NULL AND ROW(
            NEW.verified_at,NEW.verified_path_digest,NEW.provider_request
        ) IS DISTINCT FROM ROW(
            OLD.verified_at,OLD.verified_path_digest,OLD.provider_request
        ) THEN
            RAISE EXCEPTION 'Helm protected payload verification receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NOT (
            (OLD.state='pending' AND NEW.state IN ('pending','claimed','failed','superseded')) OR
            (OLD.state='claimed' AND NEW.state IN ('pending','claimed','git-committed','failed','superseded')) OR
            (OLD.state='git-committed' AND NEW.state IN ('git-committed','verified','failed'))
        ) THEN
            RAISE EXCEPTION 'Helm protected payload state transition is invalid'
                USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.state<>'pending' OR NEW.next_attempt_at<>NEW.created_at OR
       NEW.updated_at<>NEW.created_at OR NEW.attempts<>0 OR
       NEW.consecutive_failures<>0 OR NEW.last_failure_code<>'' OR
       NEW.lease_owner IS NOT NULL OR NEW.lease_epoch<>0 OR
       NEW.lease_until IS NOT NULL OR NEW.write_base_revision<>'' OR
       NEW.write_base_observed_at IS NOT NULL OR NEW.committed_revision<>'' OR
       NEW.committed_parent_revision<>'' OR NEW.committed_at IS NOT NULL OR
       NEW.verified_at IS NOT NULL OR NEW.verified_path_digest<>'' OR
       NEW.provider_request<>'' OR NEW.completed_at IS NOT NULL THEN
        RAISE EXCEPTION 'Helm protected payload intents must be pristine pending work'
            USING ERRCODE='23514';
    END IF;

    SELECT * INTO release_row
      FROM helm_release_revisions
     WHERE id=NEW.release_revision_id
     FOR KEY SHARE;
    IF release_row.id IS NULL OR release_row.generation<>NEW.release_generation OR
       release_row.project_id<>NEW.project_id OR
       release_row.environment_id<>NEW.environment_id OR
       release_row.application_id<>NEW.application_id OR
       (release_row.desired_enabled AND NEW.action<>'publish') OR
       (NOT release_row.desired_enabled AND NEW.action<>'disable-receipt') THEN
        RAISE EXCEPTION 'Helm protected payload does not match its desired release revision'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM helm_release_heads
         WHERE environment_id=NEW.environment_id
           AND application_id=NEW.application_id
           AND revision_id=NEW.release_revision_id
           AND generation=NEW.release_generation
         FOR UPDATE
    ) THEN
        RAISE EXCEPTION 'Helm protected payload revision is no longer the durable head'
            USING ERRCODE='23514';
    END IF;

    SELECT * INTO platform_row
      FROM git_repository_bindings
     WHERE id=NEW.platform_binding_id
     FOR KEY SHARE;
    SELECT * INTO environment_row
      FROM git_repository_bindings
     WHERE id=NEW.environment_binding_id
     FOR KEY SHARE;
    IF platform_row.id IS NULL OR platform_row.kind<>'platform' OR
       platform_row.credential_mode<>'github-app' OR
       platform_row.cluster_id<>NEW.cluster_id OR
       platform_row.target_ref<>NEW.platform_target_ref OR
       platform_row.path_prefix<>'clusters/'||NEW.cluster_id::text OR
       platform_row.state NOT IN ('ready','indexing') OR
       platform_row.target_head_revision<>NEW.planned_base_revision OR
       environment_row.id IS NULL OR environment_row.kind<>'environment' OR
       environment_row.project_id<>NEW.project_id OR
       environment_row.environment_id<>NEW.environment_id OR
       environment_row.target_ref<>NEW.environment_target_ref OR
       environment_row.target_head_revision<>NEW.environment_revision OR
       environment_row.indexed_revision<>NEW.environment_revision OR
       environment_row.projection_generation<>NEW.environment_generation OR
       environment_row.state<>'ready' THEN
        RAISE EXCEPTION 'Helm protected payload bindings are stale or mismatched'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM git_projection_generations generation
         WHERE generation.binding_id=NEW.environment_binding_id
           AND generation.generation=NEW.environment_generation
           AND generation.head_revision=NEW.environment_revision
           AND generation.state='active'
    ) OR EXISTS (
        SELECT 1 FROM git_projected_documents document
         WHERE document.binding_id=NEW.environment_binding_id
           AND document.generation=NEW.environment_generation
           AND NOT document.valid
    ) THEN
        RAISE EXCEPTION 'Helm protected payload requires one valid active projection generation'
            USING ERRCODE='23514';
    END IF;

    IF NEW.action='publish' THEN
        expected_path := 'clusters/'||NEW.cluster_id::text||
            '/helm-manifests/environments/'||NEW.environment_id::text||
            '/applications/'||NEW.application_id::text||'/revisions/'||
            NEW.release_revision_id::text||'/release.yaml';
        SELECT result.rendered_manifests,result.manifest_digest,
               result.inventory_digest,result.resource_count,command.state
          INTO result_manifest,result_digest,result_inventory,result_count,command_state
          FROM helm_render_results result
          JOIN helm_render_commands command ON command.id=result.command_id
         WHERE result.command_id=release_row.render_command_id
         FOR KEY SHARE OF result,command;
        IF command_state IS DISTINCT FROM 'succeeded' OR
           result_manifest IS NULL OR result_manifest IS DISTINCT FROM NEW.content OR
           result_digest<>NEW.content_digest OR
           result_inventory<>NEW.manifest_inventory_digest OR
           result_count<>NEW.manifest_resource_count THEN
            RAISE EXCEPTION 'Helm protected payload does not match the durable render result'
                USING ERRCODE='23514';
        END IF;
    ELSE
        expected_path := 'clusters/'||NEW.cluster_id::text||
            '/helm-manifests/environments/'||NEW.environment_id::text||
            '/applications/'||NEW.application_id::text||'/revisions/'||
            NEW.release_revision_id::text||'/disabled.json';
        BEGIN
            disabled_receipt := convert_from(NEW.content,'UTF8')::jsonb;
        EXCEPTION WHEN OTHERS THEN
            RAISE EXCEPTION 'Helm disabled receipt is not valid UTF-8 JSON'
                USING ERRCODE='23514';
        END;
        IF disabled_receipt IS DISTINCT FROM jsonb_build_object(
            'apiVersion','kuberploy.io/v1alpha1',
            'kind','HelmReleaseDisabledReceipt',
            'releaseRevisionId',NEW.release_revision_id::text,
            'generation',NEW.release_generation,
            'projectId',NEW.project_id::text,
            'environmentId',NEW.environment_id::text,
            'applicationId',NEW.application_id::text
        ) THEN
            RAISE EXCEPTION 'Helm disabled receipt identity was not server-derived'
                USING ERRCODE='23514';
        END IF;
    END IF;
    IF NEW.path<>expected_path THEN
        RAISE EXCEPTION 'Helm protected payload path was not server-derived'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_protected_payload_intents_validate
    BEFORE INSERT OR UPDATE OR DELETE ON helm_protected_payload_intents
    FOR EACH ROW EXECUTE FUNCTION validate_helm_protected_payload_intent();

CREATE OR REPLACE FUNCTION validate_helm_protected_application_intent()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    release_row helm_release_revisions%ROWTYPE;
    payload_row helm_protected_payload_intents%ROWTYPE;
    base_row helm_protected_application_intents%ROWTYPE;
    platform_row git_repository_bindings%ROWTYPE;
    expected_application_path text;
    expected_source_directory text;
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'Helm protected Application intents cannot be deleted'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF OLD.state IN ('verified','failed','superseded') THEN
            RAISE EXCEPTION 'Terminal Helm protected Application intents are immutable'
                USING ERRCODE='23514';
        END IF;
        IF ROW(
            NEW.id,NEW.release_revision_id,NEW.payload_intent_id,
            NEW.release_generation,NEW.project_id,NEW.environment_id,
            NEW.application_id,NEW.action,NEW.platform_binding_id,
            NEW.environment_binding_id,NEW.cluster_id,NEW.platform_target_ref,
            NEW.environment_target_ref,NEW.environment_revision,
            NEW.environment_generation,NEW.catalog_digest,NEW.planned_base_revision,
            NEW.payload_revision,NEW.payload_path,NEW.source_directory,
            NEW.application_path,NEW.operation,NEW.precondition,NEW.expected_etag,
            NEW.content,NEW.content_digest,NEW.intent_digest,NEW.commit_trailer,
            NEW.publisher_contract,NEW.publisher_config_digest,NEW.message,
            NEW.created_at
        ) IS DISTINCT FROM ROW(
            OLD.id,OLD.release_revision_id,OLD.payload_intent_id,
            OLD.release_generation,OLD.project_id,OLD.environment_id,
            OLD.application_id,OLD.action,OLD.platform_binding_id,
            OLD.environment_binding_id,OLD.cluster_id,OLD.platform_target_ref,
            OLD.environment_target_ref,OLD.environment_revision,
            OLD.environment_generation,OLD.catalog_digest,OLD.planned_base_revision,
            OLD.payload_revision,OLD.payload_path,OLD.source_directory,
            OLD.application_path,OLD.operation,OLD.precondition,OLD.expected_etag,
            OLD.content,OLD.content_digest,OLD.intent_digest,OLD.commit_trailer,
            OLD.publisher_contract,OLD.publisher_config_digest,OLD.message,
            OLD.created_at
        ) THEN
            RAISE EXCEPTION 'Helm protected Application intent identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR
           NEW.lease_epoch>OLD.lease_epoch+1 OR
           NEW.attempts<OLD.attempts OR NEW.attempts>OLD.attempts+1 OR
           NEW.consecutive_failures<OLD.consecutive_failures OR
           NEW.updated_at<OLD.updated_at THEN
            RAISE EXCEPTION 'Helm protected Application fencing or time regressed'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND
           OLD.lease_owner IS NOT NULL AND NEW.lease_owner IS NOT NULL AND
           NEW.lease_owner<>OLD.lease_owner THEN
            RAISE EXCEPTION 'Helm protected Application lease owner changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.committed_revision<>'' AND ROW(
            NEW.committed_revision,NEW.committed_parent_revision,NEW.committed_at
        ) IS DISTINCT FROM ROW(
            OLD.committed_revision,OLD.committed_parent_revision,OLD.committed_at
        ) THEN
            RAISE EXCEPTION 'Helm protected Application commit receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.verified_at IS NOT NULL AND ROW(
            NEW.verified_at,NEW.verified_path_digest,NEW.provider_request
        ) IS DISTINCT FROM ROW(
            OLD.verified_at,OLD.verified_path_digest,OLD.provider_request
        ) THEN
            RAISE EXCEPTION 'Helm protected Application verification receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NOT (
            (OLD.state='pending' AND NEW.state IN ('pending','claimed','failed','superseded')) OR
            (OLD.state='claimed' AND NEW.state IN ('pending','claimed','git-committed','failed','superseded')) OR
            (OLD.state='git-committed' AND NEW.state IN ('git-committed','verified'))
        ) THEN
            RAISE EXCEPTION 'Helm protected Application state transition is invalid'
                USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.state<>'pending' OR NEW.next_attempt_at<>NEW.created_at OR
       NEW.updated_at<>NEW.created_at OR NEW.attempts<>0 OR
       NEW.consecutive_failures<>0 OR NEW.last_failure_code<>'' OR
       NEW.lease_owner IS NOT NULL OR NEW.lease_epoch<>0 OR
       NEW.lease_until IS NOT NULL OR NEW.write_base_revision<>'' OR
       NEW.write_base_observed_at IS NOT NULL OR NEW.committed_revision<>'' OR
       NEW.committed_parent_revision<>'' OR NEW.committed_at IS NOT NULL OR
       NEW.verified_at IS NOT NULL OR NEW.verified_path_digest<>'' OR
       NEW.provider_request<>'' OR NEW.completed_at IS NOT NULL THEN
        RAISE EXCEPTION 'Helm protected Application intents must be pristine pending work'
            USING ERRCODE='23514';
    END IF;

    SELECT * INTO release_row
      FROM helm_release_revisions
     WHERE id=NEW.release_revision_id
     FOR KEY SHARE;
    SELECT * INTO payload_row
      FROM helm_protected_payload_intents
     WHERE id=NEW.payload_intent_id
     FOR KEY SHARE;
    IF release_row.id IS NULL OR payload_row.id IS NULL OR
       payload_row.state<>'verified' OR
       payload_row.release_revision_id<>NEW.release_revision_id OR
       payload_row.release_generation<>NEW.release_generation OR
       payload_row.project_id<>NEW.project_id OR
       payload_row.environment_id<>NEW.environment_id OR
       payload_row.application_id<>NEW.application_id OR
       payload_row.platform_binding_id<>NEW.platform_binding_id OR
       payload_row.environment_binding_id<>NEW.environment_binding_id OR
       payload_row.cluster_id<>NEW.cluster_id OR
       payload_row.platform_target_ref<>NEW.platform_target_ref OR
       payload_row.environment_target_ref<>NEW.environment_target_ref OR
       payload_row.environment_revision<>NEW.environment_revision OR
       payload_row.environment_generation<>NEW.environment_generation OR
       payload_row.catalog_digest<>NEW.catalog_digest OR
       payload_row.committed_revision<>NEW.payload_revision OR
       payload_row.path<>NEW.payload_path OR
       (release_row.desired_enabled AND
        (NEW.action<>'publish' OR payload_row.action<>'publish')) OR
       (NOT release_row.desired_enabled AND
        (NEW.action<>'delete' OR payload_row.action<>'disable-receipt')) THEN
        RAISE EXCEPTION 'Helm protected Application lacks its exact verified payload'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM helm_release_heads
         WHERE environment_id=NEW.environment_id
           AND application_id=NEW.application_id
           AND revision_id=NEW.release_revision_id
           AND generation=NEW.release_generation
         FOR UPDATE
    ) THEN
        RAISE EXCEPTION 'Helm protected Application revision is no longer the durable head'
            USING ERRCODE='23514';
    END IF;

    SELECT * INTO platform_row
      FROM git_repository_bindings
     WHERE id=NEW.platform_binding_id
     FOR KEY SHARE;
    IF platform_row.id IS NULL OR platform_row.kind<>'platform' OR
       platform_row.credential_mode<>'github-app' OR
       platform_row.cluster_id<>NEW.cluster_id OR
       platform_row.target_ref<>NEW.platform_target_ref OR
       platform_row.path_prefix<>'clusters/'||NEW.cluster_id::text OR
       platform_row.state NOT IN ('ready','indexing') OR
       platform_row.target_head_revision<>NEW.planned_base_revision THEN
        RAISE EXCEPTION 'Helm protected Application binding is stale or mismatched'
            USING ERRCODE='23514';
    END IF;

    expected_application_path := 'clusters/'||NEW.cluster_id::text||
        '/argocd/helm-applications/'||NEW.environment_id::text||'/'||
        NEW.application_id::text||'.yaml';
    expected_source_directory := 'clusters/'||NEW.cluster_id::text||
        '/helm-manifests/environments/'||NEW.environment_id::text||
        '/applications/'||NEW.application_id::text||'/revisions/'||
        NEW.release_revision_id::text;
    IF NEW.application_path<>expected_application_path OR
       (NEW.action='publish' AND NEW.source_directory<>expected_source_directory) OR
       (NEW.action='delete' AND NEW.source_directory<>'') THEN
        RAISE EXCEPTION 'Helm protected Application paths were not server-derived'
            USING ERRCODE='23514';
    END IF;

    IF release_row.base_intent_id IS NULL THEN
        IF NEW.operation<>'create' OR NEW.precondition<>'create-if-absent' OR
           NEW.expected_etag<>'' THEN
            RAISE EXCEPTION 'Initial Helm Application publication must prove absence'
                USING ERRCODE='23514';
        END IF;
    ELSE
        SELECT * INTO base_row
          FROM helm_protected_application_intents
         WHERE id=release_row.base_intent_id
         FOR KEY SHARE;
        IF base_row.id IS NULL OR base_row.state<>'verified' OR
           base_row.action<>'publish' OR
           base_row.application_path<>NEW.application_path OR
           base_row.content_digest='' OR
           NEW.expected_etag<>'"'||base_row.content_digest||'"' OR
           NEW.precondition<>'match-etag' OR
           (NEW.action='publish' AND NEW.operation<>'update') OR
           (NEW.action='delete' AND NEW.operation<>'delete') THEN
            RAISE EXCEPTION 'Helm protected Application before-image is not exact'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_protected_application_intents_validate
    BEFORE INSERT OR UPDATE OR DELETE ON helm_protected_application_intents
    FOR EACH ROW EXECUTE FUNCTION validate_helm_protected_application_intent();

-- A capability may consume this observation only when it is fresh and matches
-- the configured writer exactly. It contains no repository credential.
CREATE TABLE helm_protected_publisher_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    contract_version text NOT NULL CHECK (
        contract_version='helm-protected-publisher.v1'
    ),
    policy_version text NOT NULL CHECK (
        policy_version='helm-protected-git.v1'
    ),
    config_digest text NOT NULL CHECK (
        config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    CHECK (observed_at>=started_at AND lease_until>observed_at)
);

CREATE INDEX helm_protected_publisher_readiness_match_idx
    ON helm_protected_publisher_readiness(
        contract_version,policy_version,config_digest,observed_at DESC
    );

CREATE OR REPLACE FUNCTION protect_helm_protected_publisher_readiness_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id<>OLD.worker_id OR NEW.worker_epoch<OLD.worker_epoch OR
       NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Helm protected publisher readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND (
        NEW.contract_version<>OLD.contract_version OR
        NEW.policy_version<>OLD.policy_version OR
        NEW.config_digest<>OLD.config_digest OR
        NEW.started_at<>OLD.started_at OR NEW.observed_at<OLD.observed_at
    ) THEN
        RAISE EXCEPTION 'Helm protected publisher readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_protected_publisher_readiness_epoch
    BEFORE UPDATE ON helm_protected_publisher_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_helm_protected_publisher_readiness_epoch();

-- No user-visible Helm or rollback capability is implied by these tables.
-- Serving requires exact fresh readiness from the Helm renderer, this protected
-- publisher, and Argo desired-state/credential/root observations.
