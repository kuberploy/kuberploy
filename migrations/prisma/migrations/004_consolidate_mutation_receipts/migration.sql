-- Idempotent mutation receipts share one immutable authority table. Each kind
-- retains closed resource references and exact request fingerprints.

CREATE TABLE mutation_receipts (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    receipt_kind text NOT NULL CHECK (receipt_kind IN (
        'resource','build-api','secret-binding','auto-deploy-policy','configuration-profile'
    )),
    namespace text NOT NULL CHECK (length(namespace) BETWEEN 1 AND 128 AND namespace !~ '[[:cntrl:]]'),
    scope_key text NOT NULL CHECK (
        scope_key='global' OR
        scope_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    ),
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 256 AND idempotency_key !~ '[[:cntrl:]]'),
    request_digest text,
    request_fingerprint bytea,
    resource_type text,
    resource_id uuid,
    operation_id uuid,
    profile_id uuid,
    auto_deploy_policy_id uuid,
    secret_binding_id uuid,
    secret_version_id uuid,
    action text,
    result_revision bigint,
    request_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (actor_id,receipt_kind,namespace,scope_key,idempotency_key),
    FOREIGN KEY (profile_id,namespace) REFERENCES configuration_profiles(id,kind) ON DELETE RESTRICT,
    FOREIGN KEY (profile_id,result_revision,namespace)
        REFERENCES configuration_profile_revisions(profile_id,revision,profile_kind) ON DELETE RESTRICT,
    FOREIGN KEY (auto_deploy_policy_id,result_revision)
        REFERENCES auto_deploy_policy_revisions(policy_id,revision) ON DELETE RESTRICT,
    FOREIGN KEY (secret_binding_id) REFERENCES secret_bindings(id) ON DELETE RESTRICT,
    FOREIGN KEY (secret_version_id,secret_binding_id)
        REFERENCES secret_binding_versions(id,binding_id) ON DELETE RESTRICT,
    CHECK (
        (receipt_kind='resource' AND request_digest IS NOT NULL AND request_fingerprint IS NULL
            AND resource_type IS NOT NULL AND resource_id IS NOT NULL AND profile_id IS NULL
            AND auto_deploy_policy_id IS NULL AND secret_binding_id IS NULL AND secret_version_id IS NULL
            AND action IS NULL AND result_revision IS NULL AND request_id IS NULL AND scope_key='global') OR
        (receipt_kind='build-api' AND request_digest ~ '^sha256:[0-9a-f]{64}$' AND request_fingerprint IS NULL
            AND resource_type IS NULL AND resource_id IS NOT NULL AND operation_id IS NULL AND profile_id IS NULL
            AND auto_deploy_policy_id IS NULL AND secret_binding_id IS NULL AND secret_version_id IS NULL
            AND action IS NULL AND result_revision IS NULL AND request_id IS NULL AND scope_key<>'global') OR
        (receipt_kind='secret-binding' AND request_digest IS NULL AND octet_length(request_fingerprint)=32
            AND resource_type IS NULL AND resource_id IS NULL AND operation_id IS NULL AND profile_id IS NULL
            AND auto_deploy_policy_id IS NULL AND secret_binding_id IS NOT NULL AND secret_version_id IS NOT NULL
            AND action IS NULL AND result_revision IS NULL AND request_id IS NULL AND scope_key<>'global') OR
        (receipt_kind='auto-deploy-policy' AND request_digest ~ '^sha256:[0-9a-f]{64}$' AND request_fingerprint IS NULL
            AND resource_type IS NULL AND resource_id IS NULL AND operation_id IS NULL AND profile_id IS NULL
            AND auto_deploy_policy_id IS NOT NULL AND secret_binding_id IS NULL AND secret_version_id IS NULL
            AND action IN ('create','revise') AND result_revision>0 AND request_id IS NOT NULL
            AND namespace='auto-deploy-policy' AND scope_key='global') OR
        (receipt_kind='configuration-profile' AND request_digest ~ '^sha256:[0-9a-f]{64}$' AND request_fingerprint IS NULL
            AND resource_type IS NULL AND resource_id IS NULL AND operation_id IS NULL AND profile_id IS NOT NULL
            AND auto_deploy_policy_id IS NULL AND secret_binding_id IS NULL AND secret_version_id IS NULL
            AND action IN ('create','revise','clone','deactivate') AND result_revision>0 AND request_id IS NOT NULL
            AND namespace IN ('scheduling','middleware','certificate-issuer')
            AND (namespace='middleware' OR action<>'clone') AND scope_key='global')
    )
);
CREATE INDEX mutation_receipts_operation_idx ON mutation_receipts(operation_id) WHERE operation_id IS NOT NULL;
CREATE INDEX mutation_receipts_resource_idx ON mutation_receipts(receipt_kind,resource_type,resource_id);

INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,request_digest,
    resource_type,resource_id,operation_id,created_at)
SELECT actor_id,'resource',scope,'global',key,fingerprint,resource_type,resource_id,operation_id,created_at
FROM idempotency_keys;

INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,request_digest,
    resource_id,created_at)
SELECT actor_id,'build-api',operation,scope_id::text,idempotency_key,request_fingerprint,resource_id,created_at
FROM build_api_idempotency;

INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,request_fingerprint,
    secret_binding_id,secret_version_id,created_at)
SELECT actor_id,'secret-binding',operation,application_id::text,idempotency_key,request_fingerprint,
    binding_id,version_id,created_at
FROM secret_binding_idempotency;

INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,request_digest,
    auto_deploy_policy_id,action,result_revision,request_id,created_at)
SELECT actor_id,'auto-deploy-policy','auto-deploy-policy','global',idempotency_key,request_digest,
    policy_id,action,result_revision,request_id,created_at
FROM auto_deploy_policy_commands;

INSERT INTO mutation_receipts(actor_id,receipt_kind,namespace,scope_key,idempotency_key,request_digest,
    profile_id,action,result_revision,request_id,created_at)
SELECT actor_id,'configuration-profile',profile_kind,'global',idempotency_key,request_digest,
    profile_id,action,result_revision,request_id,created_at
FROM configuration_profile_commands;

DROP TABLE idempotency_keys;
DROP TABLE build_api_idempotency;
DROP TABLE secret_binding_idempotency;
DROP TABLE auto_deploy_policy_commands;
DROP TABLE configuration_profile_commands;

CREATE OR REPLACE FUNCTION validate_mutation_receipt()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'mutation receipts are immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.receipt_kind='configuration-profile' AND NEW.namespace='certificate-issuer' AND
       NOT EXISTS (SELECT 1 FROM users WHERE id=NEW.actor_id AND role='platform-admin') THEN
        RAISE EXCEPTION 'certificate issuer mutation requires platform-admin' USING ERRCODE='42501';
    END IF;
    IF NEW.receipt_kind IN ('auto-deploy-policy','configuration-profile') AND
       (NEW.request_id !~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$' OR
        NEW.idempotency_key !~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$') THEN
        RAISE EXCEPTION 'mutation receipt identifier is invalid' USING ERRCODE='23514';
    END IF;
    IF NEW.receipt_kind='build-api' AND
       (NEW.namespace NOT IN ('definition.create','attempt.cancel','attempt.retry') OR
        length(NEW.idempotency_key) NOT BETWEEN 16 AND 128) THEN
        RAISE EXCEPTION 'build API mutation receipt is invalid' USING ERRCODE='23514';
    END IF;
    IF NEW.receipt_kind='secret-binding' AND
       (NEW.namespace NOT IN ('create','rotate') OR NEW.idempotency_key !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$') THEN
        RAISE EXCEPTION 'secret binding mutation receipt is invalid' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER mutation_receipts_validate
    BEFORE INSERT OR UPDATE OR DELETE ON mutation_receipts
    FOR EACH ROW EXECUTE FUNCTION validate_mutation_receipt();

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
    IF NOT EXISTS (SELECT 1 FROM mutation_receipts i WHERE i.receipt_kind='resource'
        AND i.operation_id=NEW.operation_id AND i.actor_id=NEW.actor_id AND i.request_digest=NEW.request_digest) THEN
        RAISE EXCEPTION 'Git VariableSet command request authority mismatch' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

DROP FUNCTION reject_secret_binding_idempotency_mutation();
DROP FUNCTION validate_configuration_profile_command();
