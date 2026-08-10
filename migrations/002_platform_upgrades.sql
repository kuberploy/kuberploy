ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_environment_id_application_id_key;

ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS generation bigint NOT NULL DEFAULT 1;

CREATE UNIQUE INDEX IF NOT EXISTS deployments_environment_application_unique
    ON deployments (environment_id, application_id);

CREATE TABLE IF NOT EXISTS deployment_operation_inputs (
    operation_id uuid PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
    deployment_id uuid NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    image text NOT NULL,
    replicas integer NOT NULL CHECK (replicas BETWEEN 1 AND 100),
    port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    environment jsonb NOT NULL DEFAULT '{}'::jsonb,
    route jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS deployment_operation_inputs_deployment_idx
    ON deployment_operation_inputs (deployment_id, created_at);

INSERT INTO deployment_operation_inputs
    (operation_id, deployment_id, image, replicas, port, environment, route, created_at)
SELECT operation_id, id, image, replicas, port, environment, route, created_at
FROM deployments
ON CONFLICT (operation_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS platform_upgrades (
    id uuid PRIMARY KEY,
    version text NOT NULL,
    manifest_digest text NOT NULL,
    manifest jsonb NOT NULL,
    state text NOT NULL CHECK (state IN ('queued','running','succeeded','failed','cancelled')),
    operation_id uuid NOT NULL UNIQUE REFERENCES operations(id) DEFERRABLE INITIALLY DEFERRED,
    runner_ref text NOT NULL DEFAULT '',
    result jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS platform_upgrades_one_active
    ON platform_upgrades ((true))
    WHERE state IN ('queued','running');
