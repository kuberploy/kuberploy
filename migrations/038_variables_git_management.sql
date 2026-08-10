-- Human-managed project/environment VariableSets use the same fenced Git and
-- protected-publication machinery as AppConfig, but retain a closed command
-- shape so no generic arbitrary-path writer is introduced. Project scope is
-- deliberately tied to one concrete environment binding/repository in MVP.
CREATE TABLE git_variable_write_commands (
    operation_id uuid PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
    actor_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    scope text NOT NULL CHECK (scope IN ('project','environment')),
    target_ref text NOT NULL,
    path text NOT NULL CHECK (
        (scope='project' AND path='tenants/'||project_id::text||'/variables.yaml') OR
        (scope='environment' AND path='tenants/'||project_id::text||'/environments/'||environment_id::text||'/variables.yaml')
    ),
    base_revision text NOT NULL CHECK (base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    precondition text NOT NULL CHECK (precondition IN ('match-etag','create-if-absent')),
    expected_etag text NOT NULL CHECK (
        (precondition='match-etag' AND expected_etag ~ '^"sha256:[0-9a-f]{64}"$') OR
        (precondition='create-if-absent' AND expected_etag='')
    ),
    parser_version text NOT NULL CHECK (length(parser_version) BETWEEN 1 AND 128 AND parser_version !~ E'[\\x00\\r\\n]'),
    content bytea NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 131072),
    content_sha256 text NOT NULL CHECK (content_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    message text NOT NULL CHECK (length(message) BETWEEN 1 AND 512 AND message !~ E'[\\x00\\r]'),
    publication_mode text NOT NULL CHECK (publication_mode IN ('direct','pull-request')),
    state text NOT NULL CHECK (state IN ('pending','git-committed','indexed')),
    committed_revision text NOT NULL DEFAULT '' CHECK (committed_revision='' OR committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    committed_at timestamptz,
    indexed_generation bigint NOT NULL DEFAULT 0 CHECK (indexed_generation>=0),
    indexed_at timestamptz,
    request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (binding_id,target_ref) REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (binding_id,project_id,environment_id) REFERENCES git_repository_bindings(id,project_id,environment_id) ON DELETE RESTRICT,
    CHECK (updated_at>=created_at),
    CHECK (
        (state='pending' AND committed_revision='' AND committed_at IS NULL AND indexed_generation=0 AND indexed_at IS NULL) OR
        (state='git-committed' AND committed_revision<>'' AND committed_at IS NOT NULL AND committed_at>=created_at AND indexed_generation=0 AND indexed_at IS NULL) OR
        (state='indexed' AND committed_revision<>'' AND committed_at IS NOT NULL AND committed_at>=created_at AND indexed_generation>0 AND indexed_at IS NOT NULL AND indexed_at>=committed_at)
    )
);
CREATE INDEX git_variable_write_commands_binding_state_idx ON git_variable_write_commands(binding_id,state,created_at,operation_id);
CREATE INDEX git_variable_write_commands_committed_idx ON git_variable_write_commands(binding_id,committed_revision) WHERE state IN ('git-committed','indexed');

CREATE OR REPLACE FUNCTION protect_git_variable_write_command()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF TG_OP='DELETE' THEN
		RAISE EXCEPTION 'Git VariableSet write commands are immutable' USING ERRCODE='23514';
	END IF;
	IF TG_OP='INSERT' THEN
		IF NEW.state<>'pending' OR NEW.committed_revision<>'' OR NEW.committed_at IS NOT NULL OR NEW.indexed_generation<>0 OR NEW.indexed_at IS NOT NULL OR NEW.updated_at<>NEW.created_at THEN
			RAISE EXCEPTION 'Git VariableSet write command must start pristine pending' USING ERRCODE='23514';
		END IF;
		RETURN NEW;
	END IF;
    IF ROW(NEW.operation_id,NEW.actor_id,NEW.binding_id,NEW.project_id,NEW.environment_id,NEW.scope,NEW.target_ref,
           NEW.path,NEW.base_revision,NEW.precondition,NEW.expected_etag,NEW.parser_version,NEW.content,NEW.content_sha256,
           NEW.message,NEW.publication_mode,NEW.request_digest,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.operation_id,OLD.actor_id,OLD.binding_id,OLD.project_id,OLD.environment_id,OLD.scope,OLD.target_ref,
           OLD.path,OLD.base_revision,OLD.precondition,OLD.expected_etag,OLD.parser_version,OLD.content,OLD.content_sha256,
           OLD.message,OLD.publication_mode,OLD.request_digest,OLD.created_at) THEN
        RAISE EXCEPTION 'Git VariableSet write command identity is immutable' USING ERRCODE='23514';
    END IF;
	IF NEW.updated_at<OLD.updated_at OR NOT (
		(OLD.publication_mode='direct' AND OLD.state='pending' AND NEW.state='git-committed') OR
		(OLD.publication_mode='direct' AND OLD.state='git-committed' AND NEW.state='indexed') OR
		(OLD.publication_mode='pull-request' AND OLD.state='pending' AND NEW.state='indexed' AND EXISTS (
			SELECT 1 FROM git_pull_request_publications p WHERE p.operation_id=OLD.operation_id AND p.state='merge-verified'
		))
	) THEN RAISE EXCEPTION 'invalid Git VariableSet write command transition' USING ERRCODE='23514'; END IF;
    IF OLD.committed_revision<>'' AND (NEW.committed_revision<>OLD.committed_revision OR NEW.committed_at<>OLD.committed_at) THEN
        RAISE EXCEPTION 'Git VariableSet write result is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER git_variable_write_commands_protect BEFORE INSERT OR UPDATE OR DELETE ON git_variable_write_commands
FOR EACH ROW EXECUTE FUNCTION protect_git_variable_write_command();

CREATE OR REPLACE FUNCTION validate_git_variable_write_operation()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE expected_target uuid;
BEGIN
	expected_target := CASE WHEN NEW.scope='project' THEN NEW.project_id ELSE NEW.environment_id END;
	IF NOT EXISTS (SELECT 1 FROM operations o WHERE o.id=NEW.operation_id AND o.kind='variable-set.git-write'
		AND o.target_type=NEW.scope AND o.target_id=expected_target AND o.status='queued' AND o.generation=1
		AND o.lease_owner IS NULL AND o.lease_until IS NULL) THEN
		RAISE EXCEPTION 'Git VariableSet command operation identity mismatch' USING ERRCODE='23514';
	END IF;
	IF NOT EXISTS (SELECT 1 FROM idempotency_keys i WHERE i.operation_id=NEW.operation_id AND i.actor_id=NEW.actor_id
		AND i.fingerprint=NEW.request_digest) THEN
		RAISE EXCEPTION 'Git VariableSet command request authority mismatch' USING ERRCODE='23514';
	END IF;
	RETURN NEW;
END;
$$;
CREATE CONSTRAINT TRIGGER git_variable_write_commands_operation
AFTER INSERT ON git_variable_write_commands DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_git_variable_write_operation();

CREATE TABLE variable_set_previews (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash)=32),
    actor_id uuid NOT NULL,
    binding_id uuid NOT NULL REFERENCES git_repository_bindings(id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    scope text NOT NULL CHECK (scope IN ('project','environment')),
    path text NOT NULL,
    base_revision text NOT NULL CHECK (base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    base_etag text NOT NULL CHECK (base_etag='' OR base_etag ~ '^"sha256:[0-9a-f]{64}"$'),
    parser_version text NOT NULL CHECK (length(parser_version) BETWEEN 1 AND 128 AND parser_version !~ E'[\\x00\\r\\n]'),
    candidate_hash bytea NOT NULL CHECK (octet_length(candidate_hash)=32),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL,
    FOREIGN KEY (binding_id,project_id,environment_id) REFERENCES git_repository_bindings(id,project_id,environment_id) ON DELETE CASCADE,
    CHECK ((scope='project' AND path='tenants/'||project_id::text||'/variables.yaml') OR
           (scope='environment' AND path='tenants/'||project_id::text||'/environments/'||environment_id::text||'/variables.yaml')),
    CHECK (expires_at>created_at AND (consumed_at IS NULL OR consumed_at>=created_at))
);

CREATE OR REPLACE FUNCTION protect_variable_set_preview()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF TG_OP='DELETE' THEN RAISE EXCEPTION 'VariableSet previews are immutable' USING ERRCODE='23514'; END IF;
	IF TG_OP='INSERT' THEN
		IF NEW.consumed_at IS NOT NULL OR NEW.created_at>now()+interval '1 minute' OR NEW.expires_at>NEW.created_at+interval '15 minutes' THEN
			RAISE EXCEPTION 'VariableSet preview must start pristine and bounded' USING ERRCODE='23514';
		END IF;
		RETURN NEW;
	END IF;
	IF ROW(NEW.token_hash,NEW.actor_id,NEW.binding_id,NEW.project_id,NEW.environment_id,NEW.scope,NEW.path,
		NEW.base_revision,NEW.base_etag,NEW.parser_version,NEW.candidate_hash,NEW.expires_at,NEW.created_at)
		IS DISTINCT FROM ROW(OLD.token_hash,OLD.actor_id,OLD.binding_id,OLD.project_id,OLD.environment_id,OLD.scope,OLD.path,
		OLD.base_revision,OLD.base_etag,OLD.parser_version,OLD.candidate_hash,OLD.expires_at,OLD.created_at) OR
		OLD.consumed_at IS NOT NULL OR NEW.consumed_at IS NULL OR NEW.consumed_at<OLD.created_at OR NEW.consumed_at>now()+interval '1 minute' THEN
		RAISE EXCEPTION 'VariableSet preview update is not an exact consumption' USING ERRCODE='23514';
	END IF;
	RETURN NEW;
END;
$$;
CREATE TRIGGER variable_set_previews_protect BEFORE INSERT OR UPDATE OR DELETE ON variable_set_previews
FOR EACH ROW EXECUTE FUNCTION protect_variable_set_preview();

-- Protected publication receipts may belong to either of the two closed Git
-- command families. The constraint trigger rejects orphan/substituted rows.
ALTER TABLE git_pull_request_publications DROP CONSTRAINT git_pull_request_publications_operation_id_fkey;
ALTER TABLE git_pull_request_publications ADD CONSTRAINT git_pull_request_publications_operation_id_fkey
    FOREIGN KEY (operation_id) REFERENCES operations(id) ON DELETE CASCADE;
CREATE OR REPLACE FUNCTION require_closed_git_publication_command()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ((SELECT count(*) FROM git_deployment_write_commands WHERE operation_id=NEW.operation_id) +
        (SELECT count(*) FROM git_variable_write_commands WHERE operation_id=NEW.operation_id)) <> 1 THEN
        RAISE EXCEPTION 'Git publication must reference exactly one closed command' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE CONSTRAINT TRIGGER git_pull_request_publications_closed_command
AFTER INSERT OR UPDATE ON git_pull_request_publications DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION require_closed_git_publication_command();

CREATE OR REPLACE FUNCTION protect_git_pull_request_publication()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' THEN
		IF NEW.state<>'pending-candidate' OR NEW.write_base_revision<>'' OR NEW.candidate_revision<>'' OR
		   NEW.pull_request_number<>0 OR NEW.pull_request_url<>'' OR NEW.pull_request_state<>'' OR NEW.merge_revision<>'' OR
		   NEW.target_revision<>'' OR NEW.provider_observed_at IS NOT NULL OR NEW.version<>1 OR NEW.updated_at<>NEW.created_at THEN
			RAISE EXCEPTION 'pull request publication must start pristine pending-candidate' USING ERRCODE='23514';
		END IF;
        IF ((SELECT count(*) FROM git_deployment_write_commands c JOIN git_repository_bindings b ON b.id=c.binding_id
              WHERE c.operation_id=NEW.operation_id AND c.publication_mode='pull-request' AND c.binding_id=NEW.binding_id
                AND c.target_ref=NEW.target_ref AND c.base_revision=NEW.base_revision AND b.provider=NEW.provider
                AND b.installation_id=NEW.installation_id AND b.repository_id=NEW.repository_id
                AND b.repository_owner=NEW.repository_owner AND b.repository_name=NEW.repository_name) +
            (SELECT count(*) FROM git_variable_write_commands c JOIN git_repository_bindings b ON b.id=c.binding_id
              WHERE c.operation_id=NEW.operation_id AND c.publication_mode='pull-request' AND c.binding_id=NEW.binding_id
                AND c.target_ref=NEW.target_ref AND c.base_revision=NEW.base_revision AND b.provider=NEW.provider
                AND b.installation_id=NEW.installation_id AND b.repository_id=NEW.repository_id
                AND b.repository_owner=NEW.repository_owner AND b.repository_name=NEW.repository_name)) <> 1 THEN
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
