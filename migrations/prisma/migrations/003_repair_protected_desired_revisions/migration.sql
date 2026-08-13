-- Keep the exact indexed document authoritative even while an older RC worker
-- remains alive during the pre-upgrade migration window. Installing this
-- trigger takes a table lock before the repair below; an old writer therefore
-- either finishes before the repair or is corrected by the trigger afterward.
CREATE OR REPLACE FUNCTION enforce_protected_deployment_desired_revision()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  authoritative_revision text;
BEGIN
  IF NEW.state = 'git-committed' AND NEW.operation_id IS NOT NULL THEN
    SELECT document.config_revision
      INTO authoritative_revision
    FROM git_write_commands AS command
    JOIN git_pull_request_publications AS publication
      ON publication.operation_id = command.operation_id
     AND publication.binding_id = command.binding_id
     AND publication.target_ref = command.target_ref
     AND publication.state = 'merge-verified'
    JOIN git_projected_documents AS document
      ON document.binding_id = command.binding_id
     AND document.generation = command.indexed_generation
     AND document.path = command.path
     AND document.valid
     AND document.content_sha256 = command.content_sha256
     AND document.raw = command.content
    JOIN operations AS operation
      ON operation.id = command.operation_id
    WHERE command.command_kind = 'deployment'
      AND command.publication_mode = 'pull-request'
      AND command.state = 'indexed'
      AND command.indexed_generation > 0
      AND command.deployment_id = NEW.id
      AND NEW.operation_id = command.operation_id
      AND NEW.generation = operation.generation;

    IF authoritative_revision IS NOT NULL THEN
      NEW.desired_revision := authoritative_revision;
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS protected_deployment_desired_revision_authority ON deployments;
CREATE TRIGGER protected_deployment_desired_revision_authority
BEFORE INSERT OR UPDATE OF state, desired_revision, operation_id, generation
ON deployments
FOR EACH ROW
EXECUTE FUNCTION enforce_protected_deployment_desired_revision();

-- Repair RC protected deployments that stored the provider target tip rather
-- than the exact indexed document's effective config revision. Historical
-- indexed generations remain authoritative even after the binding advances.
UPDATE deployments AS deployment
SET desired_revision = document.config_revision,
    updated_at = GREATEST(deployment.updated_at, command.indexed_at)
FROM git_write_commands AS command
JOIN git_pull_request_publications AS publication
  ON publication.operation_id = command.operation_id
 AND publication.binding_id = command.binding_id
 AND publication.target_ref = command.target_ref
 AND publication.state = 'merge-verified'
JOIN git_projected_documents AS document
  ON document.binding_id = command.binding_id
 AND document.generation = command.indexed_generation
 AND document.path = command.path
 AND document.valid
 AND document.content_sha256 = command.content_sha256
 AND document.raw = command.content
JOIN operations AS operation
  ON operation.id = command.operation_id
WHERE command.command_kind = 'deployment'
  AND command.publication_mode = 'pull-request'
  AND command.state = 'indexed'
  AND command.indexed_generation > 0
  AND command.deployment_id IS NOT NULL
  AND deployment.id = command.deployment_id
  AND deployment.operation_id = command.operation_id
  AND deployment.generation = operation.generation
  AND deployment.desired_revision IS DISTINCT FROM document.config_revision;
