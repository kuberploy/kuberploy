-- Deployment AppConfig and VariableSet Git writes share the same durable
-- commit/index lifecycle. One kind-fenced table removes duplicate transition
-- paths without weakening their distinct target and request authority.

CREATE TABLE git_write_commands (
    operation_id uuid PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
    command_kind text NOT NULL CHECK (command_kind IN ('deployment','variable-set')),
    deployment_id uuid,
    actor_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid,
    variable_scope text,
    target_ref text NOT NULL,
    path text NOT NULL,
    base_revision text NOT NULL CHECK (base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    precondition text NOT NULL CHECK (precondition IN ('match-etag','create-if-absent')),
    expected_etag text NOT NULL DEFAULT '',
    chart_identity text,
    policy_version text NOT NULL CHECK (length(policy_version) BETWEEN 1 AND 128 AND policy_version !~ E'[\\x00\\r\\n]'),
    content bytea NOT NULL,
    content_sha256 text NOT NULL CHECK (content_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    message text NOT NULL CHECK (length(message) BETWEEN 1 AND 512 AND message !~ E'[\\x00\\r]'),
    publication_mode text NOT NULL CHECK (publication_mode IN ('direct','pull-request')),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','git-committed','indexed')),
    committed_revision text NOT NULL DEFAULT '' CHECK (committed_revision='' OR committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    committed_at timestamptz,
    indexed_generation bigint NOT NULL DEFAULT 0 CHECK (indexed_generation>=0),
    indexed_at timestamptz,
    request_digest text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (binding_id,project_id,environment_id)
        REFERENCES git_repository_bindings(id,project_id,environment_id) ON DELETE RESTRICT,
    FOREIGN KEY (binding_id,target_ref)
        REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE RESTRICT,
    FOREIGN KEY (environment_id) REFERENCES environments(id) ON DELETE RESTRICT,
    FOREIGN KEY (application_id) REFERENCES applications(id) ON DELETE RESTRICT,
    FOREIGN KEY (deployment_id) REFERENCES deployments(id) DEFERRABLE INITIALLY DEFERRED,
    CHECK ((precondition='match-etag' AND expected_etag ~ '^"sha256:[0-9a-f]{64}"$') OR
           (precondition='create-if-absent' AND expected_etag='')),
    CHECK (updated_at>=created_at),
    CHECK (
        (state='pending' AND committed_revision='' AND committed_at IS NULL AND indexed_generation=0 AND indexed_at IS NULL) OR
        (state='git-committed' AND committed_revision<>'' AND committed_at IS NOT NULL
            AND committed_at>=created_at AND indexed_generation=0 AND indexed_at IS NULL) OR
        (state='indexed' AND committed_revision<>'' AND committed_at IS NOT NULL
            AND committed_at>=created_at AND indexed_generation>0 AND indexed_at IS NOT NULL AND indexed_at>=committed_at)
    ),
    CHECK (
        (command_kind='deployment' AND deployment_id IS NOT NULL AND application_id IS NOT NULL
            AND variable_scope IS NULL AND request_digest IS NULL
            AND path='tenants/'||project_id::text||'/environments/'||environment_id::text||'/apps/'||application_id::text||'/app.yaml'
            AND chart_identity ~ '^(?:sha256:[0-9a-f]{64}|[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[0-9]+)?)$'
            AND octet_length(content) BETWEEN 1 AND 262144) OR
        (command_kind='variable-set' AND deployment_id IS NULL AND application_id IS NULL
            AND variable_scope IN ('project','environment') AND chart_identity IS NULL
            AND request_digest ~ '^sha256:[0-9a-f]{64}$'
            AND octet_length(content) BETWEEN 1 AND 131072
            AND ((variable_scope='project' AND path='tenants/'||project_id::text||'/variables.yaml') OR
                 (variable_scope='environment' AND path='tenants/'||project_id::text||'/environments/'||environment_id::text||'/variables.yaml')))
    )
);
CREATE INDEX git_write_commands_binding_state_idx
    ON git_write_commands(binding_id,state,created_at,operation_id);
CREATE INDEX git_write_commands_committed_idx
    ON git_write_commands(binding_id,committed_revision)
    WHERE state IN ('git-committed','indexed');

INSERT INTO git_write_commands(operation_id,command_kind,deployment_id,actor_id,binding_id,project_id,
    environment_id,application_id,target_ref,path,base_revision,precondition,expected_etag,chart_identity,
    policy_version,content,content_sha256,message,publication_mode,state,committed_revision,committed_at,
    indexed_generation,indexed_at,created_at,updated_at)
SELECT operation_id,'deployment',deployment_id,actor_id,binding_id,project_id,environment_id,application_id,
    target_ref,path,base_revision,precondition,expected_etag,chart_digest,policy_version,content,content_sha256,
    message,publication_mode,state,committed_revision,committed_at,indexed_generation,indexed_at,created_at,updated_at
FROM git_deployment_write_commands;

INSERT INTO git_write_commands(operation_id,command_kind,actor_id,binding_id,project_id,environment_id,
    variable_scope,target_ref,path,base_revision,precondition,expected_etag,policy_version,content,content_sha256,
    message,publication_mode,state,committed_revision,committed_at,indexed_generation,indexed_at,request_digest,
    created_at,updated_at)
SELECT operation_id,'variable-set',actor_id,binding_id,project_id,environment_id,scope,target_ref,path,
    base_revision,precondition,expected_etag,parser_version,content,content_sha256,message,publication_mode,
    state,committed_revision,committed_at,indexed_generation,indexed_at,request_digest,created_at,updated_at
FROM git_variable_write_commands;

DROP TABLE git_deployment_write_commands;
DROP TABLE git_variable_write_commands;

CREATE OR REPLACE FUNCTION protect_git_write_command()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        IF OLD.command_kind='variable-set' THEN
            RAISE EXCEPTION 'Git VariableSet write commands are immutable' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.state<>'pending' OR NEW.committed_revision<>'' OR NEW.committed_at IS NOT NULL OR
           NEW.indexed_generation<>0 OR NEW.indexed_at IS NOT NULL OR NEW.updated_at<>NEW.created_at THEN
            RAISE EXCEPTION 'Git write command must start pristine pending' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.operation_id,NEW.command_kind,NEW.deployment_id,NEW.actor_id,NEW.binding_id,NEW.project_id,
        NEW.environment_id,NEW.application_id,NEW.variable_scope,NEW.target_ref,NEW.path,NEW.base_revision,
        NEW.precondition,NEW.expected_etag,NEW.chart_identity,NEW.policy_version,NEW.content,NEW.content_sha256,
        NEW.message,NEW.publication_mode,NEW.request_digest,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.operation_id,OLD.command_kind,OLD.deployment_id,OLD.actor_id,OLD.binding_id,OLD.project_id,
        OLD.environment_id,OLD.application_id,OLD.variable_scope,OLD.target_ref,OLD.path,OLD.base_revision,
        OLD.precondition,OLD.expected_etag,OLD.chart_identity,OLD.policy_version,OLD.content,OLD.content_sha256,
        OLD.message,OLD.publication_mode,OLD.request_digest,OLD.created_at) THEN
        RAISE EXCEPTION 'Git write command identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.updated_at<OLD.updated_at THEN
        RAISE EXCEPTION 'Git write command time cannot regress' USING ERRCODE='23514';
    END IF;
    IF OLD.command_kind='variable-set' AND NOT (
        (OLD.publication_mode='direct' AND OLD.state='pending' AND NEW.state='git-committed') OR
        (OLD.publication_mode='direct' AND OLD.state='git-committed' AND NEW.state='indexed') OR
        (OLD.publication_mode='pull-request' AND OLD.state='pending' AND NEW.state='indexed' AND EXISTS (
            SELECT 1 FROM git_pull_request_publications p
            WHERE p.operation_id=OLD.operation_id AND p.state='merge-verified'))
    ) THEN
        RAISE EXCEPTION 'invalid Git VariableSet write command transition' USING ERRCODE='23514';
    END IF;
    IF OLD.command_kind='deployment' AND (
        (OLD.state='git-committed' AND NEW.state='pending') OR
        (OLD.state='indexed' AND NEW.state<>'indexed')) THEN
        RAISE EXCEPTION 'Git deployment write command state cannot regress' USING ERRCODE='23514';
    END IF;
    IF OLD.committed_revision<>'' AND
       (NEW.committed_revision<>OLD.committed_revision OR NEW.committed_at<>OLD.committed_at) THEN
        RAISE EXCEPTION 'Git write result is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER git_write_commands_protect
    BEFORE INSERT OR UPDATE OR DELETE ON git_write_commands
    FOR EACH ROW EXECUTE FUNCTION protect_git_write_command();

CREATE OR REPLACE FUNCTION validate_git_write_operation()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE expected_target uuid;
BEGIN
    IF NEW.command_kind<>'variable-set' THEN RETURN NEW; END IF;
    expected_target := CASE WHEN NEW.variable_scope='project' THEN NEW.project_id ELSE NEW.environment_id END;
    IF NOT EXISTS (SELECT 1 FROM operations o WHERE o.id=NEW.operation_id AND o.kind='variable-set.git-write'
        AND o.target_type=NEW.variable_scope AND o.target_id=expected_target AND o.status='queued' AND o.generation=1
        AND o.lease_owner IS NULL AND o.lease_until IS NULL) THEN
        RAISE EXCEPTION 'Git VariableSet command operation identity mismatch' USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM mutation_receipts i WHERE i.receipt_kind='resource'
        AND i.operation_id=NEW.operation_id AND i.actor_id=NEW.actor_id AND i.request_digest=NEW.request_digest) THEN
        RAISE EXCEPTION 'Git VariableSet command request authority mismatch' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE CONSTRAINT TRIGGER git_write_commands_operation
    AFTER INSERT ON git_write_commands DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION validate_git_write_operation();

CREATE OR REPLACE FUNCTION require_closed_git_publication_command()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (SELECT count(*) FROM git_write_commands WHERE operation_id=NEW.operation_id) <> 1 THEN
        RAISE EXCEPTION 'Git publication must reference exactly one closed command' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION protect_git_pull_request_publication()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' THEN
        IF NEW.state<>'pending-candidate' OR NEW.write_base_revision<>'' OR NEW.candidate_revision<>'' OR
           NEW.pull_request_number<>0 OR NEW.pull_request_url<>'' OR NEW.pull_request_state<>'' OR NEW.merge_revision<>'' OR
           NEW.target_revision<>'' OR NEW.provider_observed_at IS NOT NULL OR NEW.version<>1 OR NEW.updated_at<>NEW.created_at THEN
            RAISE EXCEPTION 'pull request publication must start pristine pending-candidate' USING ERRCODE='23514';
        END IF;
        IF (SELECT count(*) FROM git_write_commands c JOIN git_repository_bindings b ON b.id=c.binding_id
            WHERE c.operation_id=NEW.operation_id AND c.publication_mode='pull-request' AND c.binding_id=NEW.binding_id
              AND c.target_ref=NEW.target_ref AND c.base_revision=NEW.base_revision AND b.provider=NEW.provider
              AND b.installation_id=NEW.installation_id AND b.repository_id=NEW.repository_id
              AND b.repository_owner=NEW.repository_owner AND b.repository_name=NEW.repository_name) <> 1 THEN
            RAISE EXCEPTION 'pull request publication identity does not match exactly one protected command' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.operation_id,NEW.binding_id,NEW.provider,NEW.installation_id,NEW.repository_id,NEW.repository_owner,NEW.repository_name,NEW.target_ref,NEW.base_revision,NEW.candidate_ref,NEW.created_at)
       IS DISTINCT FROM ROW(OLD.operation_id,OLD.binding_id,OLD.provider,OLD.installation_id,OLD.repository_id,OLD.repository_owner,OLD.repository_name,OLD.target_ref,OLD.base_revision,OLD.candidate_ref,OLD.created_at) THEN
        RAISE EXCEPTION 'pull request publication identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.version<>OLD.version+1 OR NEW.updated_at<OLD.updated_at THEN RAISE EXCEPTION 'pull request publication update is not fenced' USING ERRCODE='23514'; END IF;
    IF OLD.write_base_revision<>'' AND NEW.write_base_revision<>OLD.write_base_revision THEN RAISE EXCEPTION 'pull request write base is immutable' USING ERRCODE='23514'; END IF;
    IF OLD.candidate_revision<>'' AND NEW.candidate_revision<>OLD.candidate_revision THEN RAISE EXCEPTION 'pull request candidate revision is immutable' USING ERRCODE='23514'; END IF;
    IF OLD.pull_request_number>0 AND (NEW.pull_request_number<>OLD.pull_request_number OR NEW.pull_request_url<>OLD.pull_request_url) THEN RAISE EXCEPTION 'pull request identity is immutable' USING ERRCODE='23514'; END IF;
    IF OLD.merge_revision<>'' AND NEW.merge_revision<>OLD.merge_revision THEN RAISE EXCEPTION 'pull request merge revision is immutable' USING ERRCODE='23514'; END IF;
    IF OLD.target_revision<>'' AND NEW.target_revision<>OLD.target_revision THEN RAISE EXCEPTION 'verified target revision is immutable' USING ERRCODE='23514'; END IF;
    IF NOT ((OLD.state='pending-candidate' AND NEW.state='write-base-ready') OR
        (OLD.state='write-base-ready' AND NEW.state='candidate-ready') OR
        (OLD.state='candidate-ready' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='pull-request-open' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='pull-request-closed' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='merge-pending' AND NEW.state IN ('merge-pending','merge-verified')) OR
        (OLD.state='merge-verified' AND NEW.state='merge-verified')) THEN
        RAISE EXCEPTION 'invalid pull request publication transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

DROP FUNCTION protect_git_deployment_write_command();
DROP FUNCTION protect_git_variable_write_command();
DROP FUNCTION validate_git_variable_write_operation();
