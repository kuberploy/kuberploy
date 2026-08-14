-- One immutable receipt binds protected Helm publication to the exact ready
-- environment foundation and latest verified per-environment Argo command.
-- Git ancestry remains verified by the protected publisher against the
-- claim-time provider head; PostgreSQL owns identity, currentness, and CAS.
CREATE TABLE helm_publication_prerequisite_receipts (
    release_revision_id uuid PRIMARY KEY REFERENCES helm_release_revisions(id),
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    platform_binding_id uuid NOT NULL REFERENCES git_repository_bindings(id),
    environment_binding_id uuid NOT NULL REFERENCES git_repository_bindings(id),
    cluster_id uuid NOT NULL,
    environment_revision text NOT NULL CHECK (environment_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    environment_generation bigint NOT NULL CHECK (environment_generation > 0),
    foundation_intent_id uuid NOT NULL REFERENCES environment_foundation_intents(id),
    foundation_revision text NOT NULL CHECK (foundation_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    desired_state_command_id uuid NOT NULL REFERENCES argo_desired_state_commands(id),
    desired_state_revision text NOT NULL CHECK (desired_state_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    planned_base_revision text NOT NULL CHECK (planned_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    created_at timestamptz NOT NULL,
    CONSTRAINT helm_publication_prerequisite_target_key
      UNIQUE (release_revision_id, project_id, environment_id, application_id),
    CONSTRAINT helm_publication_prerequisite_authority_key
      UNIQUE (release_revision_id, platform_binding_id, environment_binding_id,
              environment_revision, environment_generation)
);

CREATE INDEX helm_publication_prerequisite_environment_idx
ON helm_publication_prerequisite_receipts(environment_id, environment_generation, release_revision_id);

ALTER TABLE helm_protected_payload_intents
ADD COLUMN prerequisite_receipt_id uuid REFERENCES helm_publication_prerequisite_receipts(release_revision_id),
ADD COLUMN prerequisite_contract text NOT NULL DEFAULT '',
ADD COLUMN prerequisite_epoch bigint NOT NULL DEFAULT 0,
ADD CONSTRAINT helm_protected_payload_prerequisite_contract_check
  CHECK (prerequisite_contract IN ('','helm-publication-prerequisite.v1')),
ADD CONSTRAINT helm_protected_payload_prerequisite_epoch_check
  CHECK (prerequisite_epoch>=0);

ALTER TABLE helm_protected_application_intents
ADD COLUMN prerequisite_receipt_id uuid REFERENCES helm_publication_prerequisite_receipts(release_revision_id),
ADD COLUMN prerequisite_contract text NOT NULL DEFAULT '',
ADD COLUMN prerequisite_epoch bigint NOT NULL DEFAULT 0,
ADD CONSTRAINT helm_protected_application_prerequisite_contract_check
  CHECK (prerequisite_contract IN ('','helm-publication-prerequisite.v1')),
ADD CONSTRAINT helm_protected_application_prerequisite_epoch_check
  CHECK (prerequisite_epoch>=0);

CREATE FUNCTION validate_helm_publication_prerequisite_receipt()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'Helm publication prerequisite receipts are immutable'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1
        FROM helm_release_revisions release
        JOIN git_repository_bindings platform
          ON platform.id=NEW.platform_binding_id
        JOIN git_repository_bindings environment_binding
          ON environment_binding.id=NEW.environment_binding_id
        JOIN git_projection_generations generation
          ON generation.binding_id=environment_binding.id
         AND generation.generation=NEW.environment_generation
        WHERE release.id=NEW.release_revision_id
          AND release.project_id=NEW.project_id
          AND release.environment_id=NEW.environment_id
          AND release.application_id=NEW.application_id
          AND platform.kind='platform'
          AND platform.credential_mode='github-app'
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
          AND generation.state='active'
          AND generation.head_revision=NEW.environment_revision
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
        FROM argo_desired_state_commands command
        JOIN environments environment ON environment.id=command.environment_id
        WHERE command.id=NEW.desired_state_command_id
          AND command.project_id=NEW.project_id
          AND command.environment_id=NEW.environment_id
          AND command.platform_binding_id=NEW.platform_binding_id
          AND command.environment_binding_id=NEW.environment_binding_id
          AND command.cluster_id=NEW.cluster_id
          AND command.environment_revision=NEW.environment_revision
          AND command.environment_generation=NEW.environment_generation
          AND command.state='verified'
          AND command.committed_revision=NEW.desired_state_revision
          AND command.write_base_revision<>''
          AND command.verified_at IS NOT NULL
          AND command.completed_at=command.verified_at
          AND command.argo_project=environment.argo_project
          AND command.destination_namespace=environment.namespace
          AND NOT EXISTS (
              SELECT 1 FROM argo_desired_state_commands later
              WHERE later.project_id=command.project_id
                AND later.environment_id=command.environment_id
                AND later.state='verified'
                AND later.generation>command.generation
          )
    ) THEN
        RAISE EXCEPTION 'Helm publication requires latest verified exact Argo environment project'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_publication_prerequisite_receipts_validate
BEFORE INSERT OR UPDATE OR DELETE ON helm_publication_prerequisite_receipts
FOR EACH ROW EXECUTE FUNCTION validate_helm_publication_prerequisite_receipt();

-- Backfill only validator-equivalent terminal or recoverable live phase-one
-- receipts. Exact live intents retain their original trailer/commit recovery
-- identity but cannot transition until the new worker adopts the receipt using
-- the monotonic v004 fence below. Invalid legacy live work is superseded.
INSERT INTO helm_publication_prerequisite_receipts(
    release_revision_id,project_id,environment_id,application_id,
    platform_binding_id,environment_binding_id,cluster_id,
    environment_revision,environment_generation,foundation_intent_id,
    foundation_revision,desired_state_command_id,desired_state_revision,
    planned_base_revision,created_at
)
SELECT payload.release_revision_id,payload.project_id,payload.environment_id,
       payload.application_id,payload.platform_binding_id,
       payload.environment_binding_id,payload.cluster_id,
       payload.environment_revision,payload.environment_generation,
       foundation.id,foundation.committed_revision,command.id,
       command.committed_revision,platform.target_head_revision,
       GREATEST(payload.created_at,foundation.completed_at,command.completed_at)
FROM helm_protected_payload_intents payload
JOIN helm_release_revisions release
  ON release.id=payload.release_revision_id
 AND release.project_id=payload.project_id
 AND release.environment_id=payload.environment_id
 AND release.application_id=payload.application_id
JOIN LATERAL (
    SELECT candidate.* FROM environment_foundation_intents candidate
    WHERE candidate.environment_id=payload.environment_id
      AND candidate.project_id=payload.project_id
      AND candidate.platform_binding_id=payload.platform_binding_id
      AND candidate.cluster_id=payload.cluster_id
      AND candidate.state='ready' AND candidate.active
      AND candidate.committed_revision<>''
      AND candidate.published_at IS NOT NULL
      AND candidate.completed_at=candidate.published_at
    ORDER BY candidate.created_at DESC,candidate.id DESC LIMIT 1
) foundation ON true
JOIN LATERAL (
    SELECT candidate.* FROM argo_desired_state_commands candidate
    JOIN environments environment ON environment.id=candidate.environment_id
    WHERE candidate.project_id=payload.project_id
      AND candidate.environment_id=payload.environment_id
      AND candidate.platform_binding_id=payload.platform_binding_id
      AND candidate.environment_binding_id=payload.environment_binding_id
      AND candidate.cluster_id=payload.cluster_id
      AND candidate.environment_revision=payload.environment_revision
      AND candidate.environment_generation=payload.environment_generation
      AND candidate.state='verified' AND candidate.committed_revision<>''
      AND candidate.write_base_revision<>'' AND candidate.verified_at IS NOT NULL
      AND candidate.completed_at=candidate.verified_at
      AND candidate.argo_project=environment.argo_project
      AND candidate.destination_namespace=environment.namespace
      AND NOT EXISTS (
          SELECT 1 FROM argo_desired_state_commands later
          WHERE later.project_id=candidate.project_id
            AND later.environment_id=candidate.environment_id
            AND later.state='verified'
            AND later.generation>candidate.generation
      )
    ORDER BY candidate.generation DESC,candidate.id DESC LIMIT 1
) command ON true
JOIN git_repository_bindings platform
  ON platform.id=payload.platform_binding_id
 AND platform.kind='platform'
 AND platform.credential_mode='github-app'
 AND platform.cluster_id=payload.cluster_id
 AND platform.state IN ('ready','indexing')
 AND platform.target_head_revision IS NOT NULL
JOIN git_repository_bindings environment_binding
  ON environment_binding.id=payload.environment_binding_id
 AND environment_binding.kind='environment'
 AND environment_binding.credential_mode='github-app'
 AND environment_binding.state='ready'
 AND environment_binding.project_id=payload.project_id
 AND environment_binding.environment_id=payload.environment_id
 AND environment_binding.target_head_revision=payload.environment_revision
 AND environment_binding.indexed_revision=payload.environment_revision
 AND environment_binding.projection_generation=payload.environment_generation
JOIN git_projection_generations generation
  ON generation.binding_id=payload.environment_binding_id
 AND generation.generation=payload.environment_generation
 AND generation.head_revision=payload.environment_revision
 AND generation.state='active'
WHERE payload.state IN ('pending','claimed','git-committed','verified')
AND NOT EXISTS (
    SELECT 1 FROM git_projected_documents document
    WHERE document.binding_id=payload.environment_binding_id
      AND document.generation=payload.environment_generation
      AND NOT document.valid
)
ON CONFLICT(release_revision_id) DO NOTHING;

ALTER TABLE helm_protected_application_intents
DISABLE TRIGGER helm_protected_application_intents_validate;
ALTER TABLE helm_protected_payload_intents
DISABLE TRIGGER helm_protected_payload_intents_validate;

UPDATE helm_protected_payload_intents intent
SET prerequisite_receipt_id=intent.release_revision_id,
    prerequisite_contract='helm-publication-prerequisite.v1'
WHERE intent.state IN ('pending','claimed','git-committed')
  AND EXISTS (
      SELECT 1 FROM helm_publication_prerequisite_receipts receipt
      WHERE receipt.release_revision_id=intent.release_revision_id
  );

UPDATE helm_protected_application_intents intent
SET prerequisite_receipt_id=intent.release_revision_id,
    prerequisite_contract='helm-publication-prerequisite.v1'
WHERE intent.state IN ('pending','claimed','git-committed')
  AND EXISTS (
      SELECT 1 FROM helm_publication_prerequisite_receipts receipt
      WHERE receipt.release_revision_id=intent.release_revision_id
  );

UPDATE helm_protected_application_intents intent
SET state='superseded',lease_owner=NULL,lease_until=NULL,
    committed_revision='',committed_parent_revision='',committed_at=NULL,
    verified_at=NULL,verified_path_digest='',provider_request='',
    consecutive_failures=1,last_failure_code='publication-prerequisite-missing',
    completed_at=GREATEST(clock_timestamp(),intent.updated_at),
    updated_at=GREATEST(clock_timestamp(),intent.updated_at)
WHERE intent.state IN ('pending','claimed','git-committed')
  AND NOT EXISTS (
      SELECT 1 FROM helm_publication_prerequisite_receipts receipt
      WHERE receipt.release_revision_id=intent.release_revision_id
  );

UPDATE helm_protected_payload_intents intent
SET state='superseded',lease_owner=NULL,lease_until=NULL,
    committed_revision='',committed_parent_revision='',committed_at=NULL,
    verified_at=NULL,verified_path_digest='',provider_request='',
    consecutive_failures=1,last_failure_code='publication-prerequisite-missing',
    completed_at=GREATEST(clock_timestamp(),intent.updated_at),
    updated_at=GREATEST(clock_timestamp(),intent.updated_at)
WHERE intent.state IN ('pending','claimed','git-committed')
  AND NOT EXISTS (
      SELECT 1 FROM helm_publication_prerequisite_receipts receipt
      WHERE receipt.release_revision_id=intent.release_revision_id
  );

ALTER TABLE helm_protected_payload_intents
ENABLE TRIGGER helm_protected_payload_intents_validate;
ALTER TABLE helm_protected_application_intents
ENABLE TRIGGER helm_protected_application_intents_validate;

-- Fence old API/worker binaries after the online migration. New code creates
-- the exact receipt on insert and advances prerequisite_epoch on every state
-- mutation. Old SQL omits that advance and therefore cannot claim, heartbeat,
-- commit, verify, retry, fail, or supersede an adopted live intent.
CREATE FUNCTION require_helm_publication_prerequisite_receipt()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM helm_publication_prerequisite_receipts receipt
        WHERE receipt.release_revision_id=NEW.release_revision_id
          AND receipt.project_id=NEW.project_id
          AND receipt.environment_id=NEW.environment_id
          AND receipt.application_id=NEW.application_id
          AND receipt.platform_binding_id=NEW.platform_binding_id
          AND receipt.environment_binding_id=NEW.environment_binding_id
          AND receipt.cluster_id=NEW.cluster_id
          AND receipt.environment_revision=NEW.environment_revision
          AND receipt.environment_generation=NEW.environment_generation
    ) THEN
        RAISE EXCEPTION 'Helm protected publication requires an exact prerequisite receipt'
            USING ERRCODE='23514';
    END IF;
    IF NEW.prerequisite_receipt_id<>NEW.release_revision_id OR
       NEW.prerequisite_contract<>'helm-publication-prerequisite.v1' THEN
        RAISE EXCEPTION 'Helm protected publication prerequisite adoption is invalid'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.prerequisite_epoch<>0 THEN
            RAISE EXCEPTION 'Helm protected publication prerequisite epoch must start at zero'
                USING ERRCODE='23514';
        END IF;
    ELSIF OLD.state IN ('pending','claimed','git-committed') THEN
        IF OLD.prerequisite_receipt_id<>OLD.release_revision_id OR
           OLD.prerequisite_contract<>'helm-publication-prerequisite.v1' OR
           NEW.prerequisite_receipt_id IS DISTINCT FROM OLD.prerequisite_receipt_id OR
           NEW.prerequisite_contract IS DISTINCT FROM OLD.prerequisite_contract OR
           NEW.prerequisite_epoch<>OLD.prerequisite_epoch+1 THEN
            RAISE EXCEPTION 'Helm protected publication update lacks v004 prerequisite fencing'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_protected_payload_prerequisite_receipt
BEFORE INSERT OR UPDATE ON helm_protected_payload_intents
FOR EACH ROW EXECUTE FUNCTION require_helm_publication_prerequisite_receipt();

CREATE TRIGGER helm_protected_application_prerequisite_receipt
BEFORE INSERT OR UPDATE ON helm_protected_application_intents
FOR EACH ROW EXECUTE FUNCTION require_helm_publication_prerequisite_receipt();
