-- Preserve the complete, policy-controlled workload contract for the current
-- desired deployment and for each immutable operation input. The legacy
-- columns remain during the compatibility window for older readers.
ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS runtime jsonb;

UPDATE deployments d
SET runtime = jsonb_build_object(
    'replicas', d.replicas,
    'ports', jsonb_build_array(jsonb_build_object(
        'name', 'http',
        'containerPort', d.port,
        'protocol', 'TCP'
    )),
    'env', COALESCE((
        SELECT jsonb_agg(jsonb_build_object('name', entry.key, 'value', entry.value) ORDER BY entry.key)
        FROM jsonb_each_text(d.environment) AS entry
    ), '[]'::jsonb),
    'resources', jsonb_build_object(
        'requests', jsonb_build_object('cpu', '50m', 'memory', '100Mi')
    )
)
WHERE runtime IS NULL;

ALTER TABLE deployments
    ALTER COLUMN runtime SET NOT NULL;
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_runtime_object;
ALTER TABLE deployments
    ADD CONSTRAINT deployments_runtime_object CHECK (jsonb_typeof(runtime) = 'object');

ALTER TABLE deployment_operation_inputs
    ADD COLUMN IF NOT EXISTS runtime jsonb;

UPDATE deployment_operation_inputs i
SET runtime = jsonb_build_object(
    'replicas', i.replicas,
    'ports', jsonb_build_array(jsonb_build_object(
        'name', 'http',
        'containerPort', i.port,
        'protocol', 'TCP'
    )),
    'env', COALESCE((
        SELECT jsonb_agg(jsonb_build_object('name', entry.key, 'value', entry.value) ORDER BY entry.key)
        FROM jsonb_each_text(i.environment) AS entry
    ), '[]'::jsonb),
    'resources', jsonb_build_object(
        'requests', jsonb_build_object('cpu', '50m', 'memory', '100Mi')
    )
)
WHERE runtime IS NULL;

ALTER TABLE deployment_operation_inputs
    ALTER COLUMN runtime SET NOT NULL;
ALTER TABLE deployment_operation_inputs
    DROP CONSTRAINT IF EXISTS deployment_operation_inputs_runtime_object;
ALTER TABLE deployment_operation_inputs
    ADD CONSTRAINT deployment_operation_inputs_runtime_object CHECK (jsonb_typeof(runtime) = 'object');
