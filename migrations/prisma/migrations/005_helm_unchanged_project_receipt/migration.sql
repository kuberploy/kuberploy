-- One immutable receipt records that the trusted Argo materializer evaluated
-- an exact environment projection generation and resolved it to one verified
-- desired-state command. Multiple receipts per environment generation are
-- intentional: runtime/catalog changes can fail and later revert without a
-- Git projection advance, requiring a fresh proof after the terminal failure.
-- Block pre-upgrade workers while legacy live rows are classified. Pending and
-- claimed-without-write-base rows cannot represent an unacknowledged Git push
-- and are retired. Claimed rows with an immutable write base and git-committed
-- rows retain the only safe trailer/commit recovery identity.
LOCK TABLE argo_desired_state_commands IN SHARE ROW EXCLUSIVE MODE;

ALTER TABLE argo_desired_state_commands
ADD COLUMN policy_digest text,
ADD CONSTRAINT argo_desired_state_commands_policy_digest_check
  CHECK (policy_digest IS NULL OR policy_digest ~ '^sha256:[0-9a-f]{64}$');

ALTER TABLE argo_desired_state_commands
DISABLE TRIGGER argo_desired_state_commands_validate;

UPDATE argo_desired_state_commands command
SET state='superseded',lease_owner=NULL,lease_until=NULL,
    worker_contract=NULL,worker_config_digest=NULL,
    consecutive_failures=LEAST(command.consecutive_failures+1,30),
    last_failure_code='policy-upgrade-superseded',
    completed_at=retired.at,updated_at=retired.at
FROM (SELECT clock_timestamp() AS at) retired
WHERE (command.state='pending' AND command.write_base_revision='')
   OR (command.state='claimed' AND command.write_base_revision='');

ALTER TABLE argo_desired_state_commands
ENABLE TRIGGER argo_desired_state_commands_validate;

CREATE FUNCTION require_argo_desired_state_policy_digest()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.policy_digest IS NULL THEN
        RAISE EXCEPTION 'new Argo desired-state commands require an exact policy digest'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER argo_desired_state_commands_require_policy_digest
BEFORE INSERT ON argo_desired_state_commands
FOR EACH ROW EXECUTE FUNCTION require_argo_desired_state_policy_digest();

CREATE FUNCTION fence_legacy_argo_desired_state_recovery()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.policy_digest IS DISTINCT FROM OLD.policy_digest THEN
        RAISE EXCEPTION 'Argo desired-state policy digest is immutable'
            USING ERRCODE='23514';
    END IF;
    IF OLD.policy_digest IS NULL AND OLD.write_base_revision<>'' AND
       OLD.state IN ('pending','claimed','git-committed') THEN
        IF NEW.write_base_revision='' OR
           NEW.write_base_revision IS DISTINCT FROM OLD.write_base_revision OR
           NEW.write_base_observed_at IS DISTINCT FROM OLD.write_base_observed_at OR
           (OLD.state='pending' AND NEW.state NOT IN ('pending','claimed','failed')) OR
           (OLD.state='claimed' AND NEW.state NOT IN ('pending','claimed','git-committed','failed')) OR
           (OLD.state='git-committed' AND NEW.state NOT IN ('git-committed','verified')) THEN
            RAISE EXCEPTION 'legacy Argo desired-state recovery cannot regress publication authority'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER argo_desired_state_commands_fence_legacy_recovery
BEFORE UPDATE ON argo_desired_state_commands
FOR EACH ROW EXECUTE FUNCTION fence_legacy_argo_desired_state_recovery();

CREATE TABLE argo_desired_state_materialization_receipts (
    id uuid PRIMARY KEY,
    environment_binding_id uuid NOT NULL REFERENCES git_repository_bindings(id),
    environment_revision text NOT NULL CHECK (environment_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    environment_generation bigint NOT NULL CHECK (environment_generation>0),
    project_id uuid NOT NULL REFERENCES projects(id),
    environment_id uuid NOT NULL REFERENCES environments(id),
    platform_binding_id uuid NOT NULL REFERENCES git_repository_bindings(id),
    cluster_id uuid NOT NULL,
    platform_target_ref text NOT NULL CHECK (platform_target_ref ~ '^refs/heads/[A-Za-z0-9._/-]{1,240}$'),
    environment_target_ref text NOT NULL CHECK (environment_target_ref ~ '^refs/heads/[A-Za-z0-9._/-]{1,240}$'),
    desired_state_command_id uuid NOT NULL REFERENCES argo_desired_state_commands(id),
    desired_state_generation bigint NOT NULL CHECK (desired_state_generation>0),
    desired_state_revision text NOT NULL CHECK (desired_state_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    desired_state_content_sha256 text NOT NULL CHECK (desired_state_content_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    catalog_digest text NOT NULL CHECK (catalog_digest ~ '^sha256:[0-9a-f]{64}$'),
    policy_digest text CHECK (policy_digest IS NULL OR policy_digest ~ '^sha256:[0-9a-f]{64}$'),
    chart_repository text NOT NULL CHECK (length(chart_repository)>=7 AND length(chart_repository)<=512 AND chart_repository ~ '^oci://[^/?#@[:space:]]+/[^?#@[:space:]]+$'),
    chart_name text NOT NULL CHECK (chart_name='kuberploy-runtime'),
    chart_version text NOT NULL CHECK (length(chart_version)>=5 AND length(chart_version)<=64 AND chart_version ~ '^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$'),
    chart_digest text NOT NULL CHECK (chart_digest ~ '^sha256:[0-9a-f]{64}$'),
    renderer_image text NOT NULL CHECK (length(renderer_image)>=10 AND length(renderer_image)<=512 AND renderer_image ~ '^[^[:space:]@]+@sha256:[0-9a-f]{64}$'),
    chart_digest_enforcement text NOT NULL CHECK (chart_digest_enforcement='native-oci-digest-v1'),
    created_at timestamptz NOT NULL,
    CONSTRAINT argo_desired_state_materialization_generation_fk
      FOREIGN KEY(environment_binding_id,environment_generation)
      REFERENCES git_projection_generations(binding_id,generation)
);

CREATE INDEX argo_desired_state_materialization_exact_idx
ON argo_desired_state_materialization_receipts(
    environment_binding_id,environment_revision,environment_generation,
    created_at DESC,id DESC
);

CREATE INDEX argo_desired_state_materialization_command_idx
ON argo_desired_state_materialization_receipts(
    desired_state_command_id,desired_state_generation,desired_state_revision
);

-- Migration compatibility: every verified command is immutable proof of the
-- exact projection generation from which its own bytes were rendered. This
-- preserves valid 004 Helm receipts even when their environment later moved.
INSERT INTO argo_desired_state_materialization_receipts(
    id,environment_binding_id,environment_revision,environment_generation,
    project_id,environment_id,platform_binding_id,cluster_id,
    platform_target_ref,environment_target_ref,desired_state_command_id,
    desired_state_generation,desired_state_revision,desired_state_content_sha256,
    catalog_digest,policy_digest,chart_repository,chart_name,chart_version,chart_digest,
    renderer_image,chart_digest_enforcement,created_at
)
SELECT command.id,command.environment_binding_id,command.environment_revision,
       command.environment_generation,command.project_id,command.environment_id,
       command.platform_binding_id,command.cluster_id,command.platform_target_ref,
       command.environment_target_ref,command.id,command.generation,
       command.committed_revision,command.content_sha256,command.catalog_digest,NULL,
       command.chart_repository,command.chart_name,command.chart_version,
       command.chart_digest,command.renderer_image,command.chart_digest_enforcement,
       command.verified_at
FROM argo_desired_state_commands command
WHERE command.state='verified' AND command.committed_revision<>''
  AND command.verified_at IS NOT NULL AND command.completed_at=command.verified_at;

CREATE FUNCTION validate_argo_desired_state_materialization_receipt()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'Argo desired-state materialization receipts are immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.policy_digest IS NULL OR NOT EXISTS (
        SELECT 1
        FROM git_repository_bindings environment_binding
        JOIN git_projection_generations generation
          ON generation.binding_id=environment_binding.id
         AND generation.generation=NEW.environment_generation
        JOIN git_repository_bindings platform
          ON platform.id=NEW.platform_binding_id
        WHERE environment_binding.id=NEW.environment_binding_id
          AND environment_binding.kind='environment'
          AND environment_binding.credential_mode='github-app'
          AND environment_binding.state='ready'
          AND environment_binding.project_id=NEW.project_id
          AND environment_binding.environment_id=NEW.environment_id
          AND environment_binding.target_ref=NEW.environment_target_ref
          AND environment_binding.target_head_revision=NEW.environment_revision
          AND environment_binding.indexed_revision=NEW.environment_revision
          AND environment_binding.projection_generation=NEW.environment_generation
          AND generation.head_revision=NEW.environment_revision
          AND generation.state='active'
          AND platform.kind='platform'
          AND platform.credential_mode='github-app'
          AND platform.cluster_id=NEW.cluster_id
          AND platform.target_ref=NEW.platform_target_ref
          AND platform.state IN ('ready','indexing')
          AND NOT EXISTS (
              SELECT 1 FROM git_projected_documents document
              WHERE document.binding_id=NEW.environment_binding_id
                AND document.generation=NEW.environment_generation
                AND NOT document.valid
          )
    ) THEN
        RAISE EXCEPTION 'Argo materialization receipt requires exact current projection authority'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM argo_desired_state_commands command
        WHERE command.id=NEW.desired_state_command_id
          AND command.generation=NEW.desired_state_generation
          AND command.project_id=NEW.project_id
          AND command.environment_id=NEW.environment_id
          AND command.platform_binding_id=NEW.platform_binding_id
          AND command.environment_binding_id=NEW.environment_binding_id
          AND command.cluster_id=NEW.cluster_id
          AND command.platform_target_ref=NEW.platform_target_ref
          AND command.environment_target_ref=NEW.environment_target_ref
          AND command.state='verified'
          AND command.committed_revision=NEW.desired_state_revision
          AND command.content_sha256=NEW.desired_state_content_sha256
          AND command.write_base_revision<>''
          AND command.verified_at IS NOT NULL
          AND command.completed_at=command.verified_at
    ) THEN
        RAISE EXCEPTION 'Argo materialization receipt requires exact verified desired state'
            USING ERRCODE='23514';
    END IF;
    IF EXISTS (
        SELECT 1 FROM argo_desired_state_commands later
        WHERE later.project_id=NEW.project_id
          AND later.environment_id=NEW.environment_id
          AND later.generation>NEW.desired_state_generation
          AND (
            later.state NOT IN ('failed','superseded') OR
            later.completed_at IS NULL OR later.completed_at>=NEW.created_at
          )
    ) THEN
        RAISE EXCEPTION 'Argo materialization receipt is behind newer desired-state authority'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER argo_desired_state_materialization_receipts_validate
BEFORE INSERT OR UPDATE OR DELETE ON argo_desired_state_materialization_receipts
FOR EACH ROW EXECUTE FUNCTION validate_argo_desired_state_materialization_receipt();

-- A changed command records its own exact generation when verification wins
-- while that projection is still current. If the binding advanced during Git
-- publication, verification must not abort; the materializer will evaluate
-- the newer generation and record its changed/no-change result separately.
CREATE FUNCTION record_verified_argo_desired_state_materialization()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.state='verified' AND NEW.policy_digest IS NOT NULL AND
       (TG_OP='INSERT' OR OLD.state<>'verified') THEN
        INSERT INTO argo_desired_state_materialization_receipts(
            id,environment_binding_id,environment_revision,environment_generation,
            project_id,environment_id,platform_binding_id,cluster_id,
            platform_target_ref,environment_target_ref,desired_state_command_id,
            desired_state_generation,desired_state_revision,desired_state_content_sha256,
            catalog_digest,policy_digest,chart_repository,chart_name,chart_version,chart_digest,
            renderer_image,chart_digest_enforcement,created_at
        )
        SELECT NEW.id,NEW.environment_binding_id,NEW.environment_revision,
               NEW.environment_generation,NEW.project_id,NEW.environment_id,
               NEW.platform_binding_id,NEW.cluster_id,NEW.platform_target_ref,
               NEW.environment_target_ref,NEW.id,NEW.generation,
               NEW.committed_revision,NEW.content_sha256,NEW.catalog_digest,NEW.policy_digest,
               NEW.chart_repository,NEW.chart_name,NEW.chart_version,
               NEW.chart_digest,NEW.renderer_image,NEW.chart_digest_enforcement,
               NEW.verified_at
        FROM git_repository_bindings binding
        JOIN git_projection_generations generation
          ON generation.binding_id=binding.id
         AND generation.generation=NEW.environment_generation
        WHERE binding.id=NEW.environment_binding_id
          AND binding.target_head_revision=NEW.environment_revision
          AND binding.indexed_revision=NEW.environment_revision
          AND binding.projection_generation=NEW.environment_generation
          AND binding.state='ready' AND generation.state='active'
          AND generation.head_revision=NEW.environment_revision;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER argo_desired_state_materialization_on_verified
AFTER INSERT OR UPDATE OF state ON argo_desired_state_commands
FOR EACH ROW EXECUTE FUNCTION record_verified_argo_desired_state_materialization();

-- Replace the 004 Helm validator. New receipts must join one durable exact
-- materialization proof. Existing immutable Helm receipts remain readable
-- after later projection/foundation/desired-state progress.
CREATE OR REPLACE FUNCTION validate_helm_publication_prerequisite_receipt()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'Helm publication prerequisite receipts are immutable'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1
        FROM helm_release_revisions release
        JOIN git_repository_bindings platform ON platform.id=NEW.platform_binding_id
        JOIN git_repository_bindings environment_binding ON environment_binding.id=NEW.environment_binding_id
        JOIN git_projection_generations generation
          ON generation.binding_id=environment_binding.id
         AND generation.generation=NEW.environment_generation
        WHERE release.id=NEW.release_revision_id
          AND release.project_id=NEW.project_id
          AND release.environment_id=NEW.environment_id
          AND release.application_id=NEW.application_id
          AND platform.kind='platform' AND platform.credential_mode='github-app'
          AND platform.cluster_id=NEW.cluster_id
          AND platform.state IN ('ready','indexing')
          AND platform.target_head_revision=NEW.planned_base_revision
          AND environment_binding.kind='environment'
          AND environment_binding.credential_mode='github-app'
          AND environment_binding.state='ready'
          AND environment_binding.project_id=NEW.project_id
          AND environment_binding.environment_id=NEW.environment_id
          AND environment_binding.target_head_revision=NEW.environment_revision
          AND environment_binding.indexed_revision=NEW.environment_revision
          AND environment_binding.projection_generation=NEW.environment_generation
          AND generation.state='active' AND generation.head_revision=NEW.environment_revision
          AND NOT EXISTS (
              SELECT 1 FROM git_projected_documents document
              WHERE document.binding_id=NEW.environment_binding_id
                AND document.generation=NEW.environment_generation
                AND NOT document.valid
          )
    ) THEN
        RAISE EXCEPTION 'Helm publication prerequisite binding is not exact current authority'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM environment_foundation_intents foundation
        WHERE foundation.id=NEW.foundation_intent_id
          AND foundation.environment_id=NEW.environment_id
          AND foundation.project_id=NEW.project_id
          AND foundation.platform_binding_id=NEW.platform_binding_id
          AND foundation.cluster_id=NEW.cluster_id
          AND foundation.state='ready' AND foundation.active
          AND foundation.committed_revision=NEW.foundation_revision
          AND foundation.published_at IS NOT NULL
          AND foundation.completed_at=foundation.published_at
    ) THEN
        RAISE EXCEPTION 'Helm publication requires exact ready environment foundation'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1
        FROM argo_desired_state_materialization_receipts materialization
        JOIN argo_desired_state_commands command
          ON command.id=materialization.desired_state_command_id
        JOIN environments environment ON environment.id=command.environment_id
        WHERE materialization.environment_binding_id=NEW.environment_binding_id
          AND materialization.environment_revision=NEW.environment_revision
          AND materialization.environment_generation=NEW.environment_generation
          AND materialization.project_id=NEW.project_id
          AND materialization.environment_id=NEW.environment_id
          AND materialization.platform_binding_id=NEW.platform_binding_id
          AND materialization.cluster_id=NEW.cluster_id
          AND materialization.desired_state_command_id=NEW.desired_state_command_id
          AND materialization.desired_state_revision=NEW.desired_state_revision
          AND materialization.created_at<=NEW.created_at
          AND materialization.policy_digest IS NOT NULL
          AND command.project_id=NEW.project_id
          AND command.environment_id=NEW.environment_id
          AND command.platform_binding_id=NEW.platform_binding_id
          AND command.environment_binding_id=NEW.environment_binding_id
          AND command.cluster_id=NEW.cluster_id
          AND command.generation=materialization.desired_state_generation
          AND command.state='verified'
          AND command.committed_revision=NEW.desired_state_revision
          AND command.content_sha256=materialization.desired_state_content_sha256
          AND command.write_base_revision<>''
          AND command.verified_at IS NOT NULL
          AND command.completed_at=command.verified_at
          AND command.argo_project=environment.argo_project
          AND command.destination_namespace=environment.namespace
          AND NOT EXISTS (
              SELECT 1 FROM argo_desired_state_commands later
              WHERE later.project_id=command.project_id
                AND later.environment_id=command.environment_id
                AND later.generation>command.generation
                AND (
                  later.state NOT IN ('failed','superseded') OR
                  later.completed_at IS NULL OR
                  later.completed_at>=materialization.created_at
                )
          )
    ) THEN
        RAISE EXCEPTION 'Helm publication requires exact current Argo materialization authority'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
