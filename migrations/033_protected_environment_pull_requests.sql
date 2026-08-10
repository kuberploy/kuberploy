-- Environment protection is immutable policy. Deployment/config callers never
-- choose direct versus pull-request publication; every accepted Git command
-- snapshots the mode derived from this server-owned environment field.
ALTER TABLE environments
    ADD COLUMN protection_policy text NOT NULL DEFAULT 'protected'
        CHECK (protection_policy IN ('development','protected'));

CREATE OR REPLACE FUNCTION protect_environment_protection_policy()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.protection_policy IS DISTINCT FROM OLD.protection_policy THEN
        RAISE EXCEPTION 'environment protection policy is immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER environments_protection_policy_immutable
    BEFORE UPDATE ON environments
    FOR EACH ROW EXECUTE FUNCTION protect_environment_protection_policy();

-- Existing commands retain the direct publication contract under which they
-- were accepted. New writers must explicitly persist the environment-derived
-- mode; dropping the default prevents an accidental implicit direct command.
ALTER TABLE git_deployment_write_commands
    ADD COLUMN publication_mode text NOT NULL DEFAULT 'direct'
        CHECK (publication_mode IN ('direct','pull-request'));
ALTER TABLE git_deployment_write_commands
    ALTER COLUMN publication_mode DROP DEFAULT;

CREATE OR REPLACE FUNCTION protect_git_deployment_write_command()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.operation_id,NEW.deployment_id,NEW.actor_id,NEW.binding_id,
           NEW.project_id,NEW.environment_id,NEW.application_id,NEW.target_ref,
           NEW.path,NEW.base_revision,NEW.precondition,NEW.expected_etag,
           NEW.chart_digest,NEW.policy_version,NEW.content,NEW.content_sha256,
           NEW.message,NEW.publication_mode,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.operation_id,OLD.deployment_id,OLD.actor_id,OLD.binding_id,
           OLD.project_id,OLD.environment_id,OLD.application_id,OLD.target_ref,
           OLD.path,OLD.base_revision,OLD.precondition,OLD.expected_etag,
           OLD.chart_digest,OLD.policy_version,OLD.content,OLD.content_sha256,
           OLD.message,OLD.publication_mode,OLD.created_at) THEN
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

-- A protected candidate and its provider pull request are durable workflow
-- state only. target_revision is absent until the exact merged revision is
-- verified on the authoritative target ref; target indexing remains the sole
-- desired-state advancement path.
CREATE TABLE git_pull_request_publications (
    operation_id uuid PRIMARY KEY
        REFERENCES git_deployment_write_commands(operation_id) ON DELETE CASCADE,
    binding_id uuid NOT NULL REFERENCES git_repository_bindings(id) ON DELETE RESTRICT,
    provider text NOT NULL CHECK (provider='github'),
    installation_id bigint NOT NULL CHECK (installation_id>0),
    repository_id bigint NOT NULL CHECK (repository_id>0),
    repository_owner text NOT NULL CHECK (
        repository_owner ~ '^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9])?$'
    ),
    repository_name text NOT NULL CHECK (
        repository_name ~ '^[A-Za-z0-9_.-]{1,100}$' AND
        repository_name NOT IN ('.','..') AND
        lower(repository_name)<>'.git' AND lower(repository_name) !~ '\.git$'
    ),
    target_ref text NOT NULL CHECK (
        target_ref ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$' AND
        target_ref !~ '\.\.' AND target_ref !~ '//' AND target_ref !~ '/$'
    ),
    base_revision text NOT NULL CHECK (
        base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    write_base_revision text NOT NULL DEFAULT '' CHECK (
        write_base_revision='' OR write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    candidate_ref text NOT NULL CHECK (
        candidate_ref='refs/heads/kuberploy/operations/'||operation_id::text
    ),
    candidate_revision text NOT NULL DEFAULT '' CHECK (
        candidate_revision='' OR candidate_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    pull_request_number bigint NOT NULL DEFAULT 0 CHECK (pull_request_number>=0),
    pull_request_url text NOT NULL DEFAULT '',
    pull_request_state text NOT NULL DEFAULT '' CHECK (pull_request_state IN ('','open','closed')),
    merge_revision text NOT NULL DEFAULT '' CHECK (
        merge_revision='' OR merge_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    target_revision text NOT NULL DEFAULT '' CHECK (
        target_revision='' OR target_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    state text NOT NULL CHECK (state IN (
        'pending-candidate','write-base-ready','candidate-ready','pull-request-open',
        'pull-request-closed','merge-pending','merge-verified'
    )),
    provider_observed_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    version bigint NOT NULL DEFAULT 1 CHECK (version>0),
    UNIQUE (repository_id,candidate_ref),
    CHECK (candidate_ref<>target_ref),
    CHECK (updated_at>=created_at),
    CHECK (provider_observed_at IS NULL OR
           (provider_observed_at>=created_at AND provider_observed_at<=updated_at)),
    CHECK (
        (state='pending-candidate' AND write_base_revision='' AND candidate_revision='' AND
            pull_request_number=0 AND pull_request_url='' AND pull_request_state='' AND
            merge_revision='' AND target_revision='' AND provider_observed_at IS NULL) OR
        (state='write-base-ready' AND write_base_revision<>'' AND candidate_revision='' AND
            pull_request_number=0 AND pull_request_url='' AND pull_request_state='' AND
            merge_revision='' AND target_revision='' AND provider_observed_at IS NULL) OR
        (state='candidate-ready' AND write_base_revision<>'' AND candidate_revision<>'' AND
            pull_request_number=0 AND pull_request_url='' AND pull_request_state='' AND
            merge_revision='' AND target_revision='' AND provider_observed_at IS NULL) OR
        (state='pull-request-open' AND write_base_revision<>'' AND candidate_revision<>'' AND
            pull_request_number>0 AND pull_request_state='open' AND
            pull_request_url='https://github.com/'||repository_owner||'/'||repository_name||'/pull/'||pull_request_number::text AND
            merge_revision='' AND target_revision='' AND provider_observed_at IS NOT NULL) OR
        (state='pull-request-closed' AND write_base_revision<>'' AND candidate_revision<>'' AND
            pull_request_number>0 AND pull_request_state='closed' AND
            pull_request_url='https://github.com/'||repository_owner||'/'||repository_name||'/pull/'||pull_request_number::text AND
            merge_revision='' AND target_revision='' AND provider_observed_at IS NOT NULL) OR
        (state='merge-pending' AND write_base_revision<>'' AND candidate_revision<>'' AND
            pull_request_number>0 AND pull_request_state='closed' AND
            pull_request_url='https://github.com/'||repository_owner||'/'||repository_name||'/pull/'||pull_request_number::text AND
            merge_revision<>'' AND target_revision='' AND provider_observed_at IS NOT NULL) OR
        (state='merge-verified' AND write_base_revision<>'' AND candidate_revision<>'' AND
            pull_request_number>0 AND pull_request_state='closed' AND
            pull_request_url='https://github.com/'||repository_owner||'/'||repository_name||'/pull/'||pull_request_number::text AND
            merge_revision<>'' AND target_revision<>'' AND provider_observed_at IS NOT NULL)
    )
);
CREATE UNIQUE INDEX git_pull_request_publications_provider_pr_idx
    ON git_pull_request_publications(repository_id,pull_request_number)
    WHERE pull_request_number>0;
CREATE INDEX git_pull_request_publications_reconcile_idx
    ON git_pull_request_publications(state,updated_at,operation_id)
    WHERE state IN ('candidate-ready','pull-request-open','pull-request-closed','merge-pending');

CREATE OR REPLACE FUNCTION protect_git_pull_request_publication()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' THEN
        IF NOT EXISTS (
            SELECT 1
            FROM git_deployment_write_commands c
            JOIN git_repository_bindings b ON b.id=c.binding_id
            WHERE c.operation_id=NEW.operation_id
              AND c.publication_mode='pull-request'
              AND c.binding_id=NEW.binding_id
              AND c.target_ref=NEW.target_ref
              AND c.base_revision=NEW.base_revision
              AND b.provider=NEW.provider
              AND b.installation_id=NEW.installation_id
              AND b.repository_id=NEW.repository_id
              AND b.repository_owner=NEW.repository_owner
              AND b.repository_name=NEW.repository_name
        ) THEN
            RAISE EXCEPTION 'pull request publication identity does not match protected command'
                USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.operation_id,NEW.binding_id,NEW.provider,NEW.installation_id,NEW.repository_id,
           NEW.repository_owner,NEW.repository_name,NEW.target_ref,
           NEW.base_revision,NEW.candidate_ref,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.operation_id,OLD.binding_id,OLD.provider,OLD.installation_id,OLD.repository_id,
           OLD.repository_owner,OLD.repository_name,OLD.target_ref,
           OLD.base_revision,OLD.candidate_ref,OLD.created_at) THEN
        RAISE EXCEPTION 'pull request publication identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.version<>OLD.version+1 OR NEW.updated_at<OLD.updated_at THEN
        RAISE EXCEPTION 'pull request publication update is not fenced'
            USING ERRCODE='23514';
    END IF;
    IF OLD.write_base_revision<>'' AND NEW.write_base_revision<>OLD.write_base_revision THEN
        RAISE EXCEPTION 'pull request write base is immutable'
            USING ERRCODE='23514';
    END IF;
    IF OLD.candidate_revision<>'' AND NEW.candidate_revision<>OLD.candidate_revision THEN
        RAISE EXCEPTION 'pull request candidate revision is immutable'
            USING ERRCODE='23514';
    END IF;
    IF OLD.pull_request_number>0 AND
       (NEW.pull_request_number<>OLD.pull_request_number OR
        NEW.pull_request_url<>OLD.pull_request_url) THEN
        RAISE EXCEPTION 'pull request identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF OLD.merge_revision<>'' AND NEW.merge_revision<>OLD.merge_revision THEN
        RAISE EXCEPTION 'pull request merge revision is immutable'
            USING ERRCODE='23514';
    END IF;
    IF OLD.target_revision<>'' AND NEW.target_revision<>OLD.target_revision THEN
        RAISE EXCEPTION 'verified target revision is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NOT (
        (OLD.state='pending-candidate' AND NEW.state='write-base-ready') OR
        (OLD.state='write-base-ready' AND NEW.state='candidate-ready') OR
        (OLD.state='candidate-ready' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='pull-request-open' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='pull-request-closed' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='merge-pending' AND NEW.state IN ('merge-pending','merge-verified')) OR
        (OLD.state='merge-verified' AND NEW.state='merge-verified')
    ) THEN
        RAISE EXCEPTION 'invalid pull request publication transition'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER git_pull_request_publications_protect
    BEFORE INSERT OR UPDATE ON git_pull_request_publications
    FOR EACH ROW EXECUTE FUNCTION protect_git_pull_request_publication();
