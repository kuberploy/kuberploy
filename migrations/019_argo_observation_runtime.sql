-- Fenced scheduler state for the namespace-wide Argo Application observer.
-- Kubernetes resourceVersion is an opaque repair cursor; correctness comes
-- from the monotonically increasing lease epoch, not lexical RV comparison.
-- One logical application may have one deployment in several environments,
-- so the Argo Application and its observation are deployment-scoped. The old
-- application-only key would collide inside a shared Argo namespace.
ALTER TABLE deployments
    ADD CONSTRAINT deployments_id_application_environment_unique
    UNIQUE (id,application_id,environment_id);

ALTER TABLE argo_application_observations
    ADD COLUMN deployment_id uuid;

UPDATE argo_application_observations observation
SET deployment_id=(
    SELECT deployment.id
    FROM deployments deployment
    WHERE deployment.application_id=observation.application_id
      AND deployment.environment_id=observation.environment_id
);

ALTER TABLE argo_application_observations
    ALTER COLUMN deployment_id SET NOT NULL;

ALTER TABLE argo_application_observations
    DROP CONSTRAINT argo_application_observations_pkey;

ALTER TABLE argo_application_observations
    ADD CONSTRAINT argo_application_observations_pkey PRIMARY KEY (deployment_id),
    ADD CONSTRAINT argo_application_observations_application_environment_unique UNIQUE (application_id,environment_id),
    ADD CONSTRAINT argo_application_observations_deployment_identity_fk
        FOREIGN KEY (deployment_id,application_id,environment_id)
        REFERENCES deployments(id,application_id,environment_id) ON DELETE CASCADE;

CREATE TABLE IF NOT EXISTS argo_observation_runtime (
    argo_namespace text PRIMARY KEY
        CHECK (argo_namespace ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'),
    lease_owner text NOT NULL DEFAULT ''
        CHECK (lease_owner='' OR lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$'),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    lease_until timestamptz,
    snapshot_resource_version text NOT NULL DEFAULT ''
        CHECK (length(snapshot_resource_version)<=128
            AND position(chr(10) IN snapshot_resource_version)=0
            AND position(chr(13) IN snapshot_resource_version)=0),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 32),
    last_failure_code text NOT NULL DEFAULT ''
        CHECK (last_failure_code='' OR last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'),
    next_poll_at timestamptz NOT NULL,
    last_completed_at timestamptz,
    updated_at timestamptz NOT NULL,
    CHECK ((lease_owner='' AND lease_until IS NULL) OR (lease_owner<>'' AND lease_epoch>0 AND lease_until IS NOT NULL)),
    CHECK ((consecutive_failures=0 AND last_failure_code='') OR (consecutive_failures>0 AND last_failure_code<>'')),
    CHECK (last_completed_at IS NULL OR last_completed_at<=updated_at)
);

CREATE INDEX IF NOT EXISTS argo_observation_runtime_due_idx
    ON argo_observation_runtime(next_poll_at,argo_namespace);
