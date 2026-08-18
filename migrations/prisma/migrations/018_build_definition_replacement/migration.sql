-- Permit immutable definition replacement while keeping one active definition
-- per application/source ref. Older definitions stay queryable history.
ALTER TABLE "build_definitions"
  DROP CONSTRAINT "build_definitions_project_id_service_id_repository_id_trigg_key";

CREATE UNIQUE INDEX "build_definitions_active_ref_idx"
  ON "build_definitions"("project_id", "service_id", "repository_id", "trigger_ref")
  WHERE "enabled" = true;
