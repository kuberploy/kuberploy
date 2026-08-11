-- Scheduling, middleware, and certificate-issuer profiles share one immutable
-- revision lifecycle. Domain-specific material stays in bounded JSON specs;
-- PostgreSQL triggers retain kind-specific scope and transition authority.

CREATE TABLE configuration_profiles (
    id uuid PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('scheduling','middleware','certificate-issuer')),
    name text NOT NULL CHECK (name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'),
    lifecycle text NOT NULL CHECK (lifecycle IN ('active','deactivated')),
    current_revision bigint NOT NULL CHECK (current_revision>0),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    deactivated_by uuid REFERENCES users(id) ON DELETE RESTRICT,
    deactivated_at timestamptz,
    UNIQUE (id,kind),
    CHECK ((lifecycle='active' AND deactivated_by IS NULL AND deactivated_at IS NULL) OR
           (lifecycle='deactivated' AND deactivated_by IS NOT NULL AND deactivated_at IS NOT NULL))
);
CREATE UNIQUE INDEX configuration_profiles_global_name_idx
    ON configuration_profiles(kind,name)
    WHERE kind IN ('scheduling','certificate-issuer');
CREATE INDEX configuration_profiles_catalog_idx ON configuration_profiles(kind,name,id);

CREATE TABLE configuration_profile_revisions (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision>0),
    profile_kind text NOT NULL CHECK (profile_kind IN ('scheduling','middleware','certificate-issuer')),
    solver_type text,
    spec jsonb NOT NULL CHECK (jsonb_typeof(spec)='object' AND octet_length(spec::text)<=65536),
    spec_digest text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    assignments_digest text CHECK (assignments_digest IS NULL OR assignments_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    cloned_from_profile_id uuid,
    cloned_from_revision bigint,
    PRIMARY KEY (profile_id,revision),
    UNIQUE (profile_id,revision,profile_kind),
    FOREIGN KEY (profile_id,profile_kind) REFERENCES configuration_profiles(id,kind) ON DELETE RESTRICT,
    FOREIGN KEY (cloned_from_profile_id,cloned_from_revision,profile_kind)
        REFERENCES configuration_profile_revisions(profile_id,revision,profile_kind) ON DELETE RESTRICT,
    CHECK ((cloned_from_profile_id IS NULL) = (cloned_from_revision IS NULL))
);

CREATE TABLE configuration_profile_assignments (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    profile_kind text NOT NULL CHECK (profile_kind IN ('scheduling','middleware')),
    ordinal integer NOT NULL CHECK (ordinal>=0),
    scope_type text NOT NULL CHECK (scope_type IN ('team','project','environment','application')),
    scope_id uuid NOT NULL,
    PRIMARY KEY (profile_id,revision,ordinal),
    UNIQUE (profile_id,revision,scope_type,scope_id),
    FOREIGN KEY (profile_id,revision,profile_kind)
        REFERENCES configuration_profile_revisions(profile_id,revision,profile_kind) ON DELETE RESTRICT
);

CREATE TABLE configuration_profile_commands (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    profile_kind text NOT NULL CHECK (profile_kind IN ('scheduling','middleware','certificate-issuer')),
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    action text NOT NULL CHECK (action IN ('create','revise','clone','deactivate')),
    request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    profile_id uuid NOT NULL,
    result_revision bigint NOT NULL CHECK (result_revision>0),
    request_id text NOT NULL CHECK (request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (actor_id,profile_kind,idempotency_key),
    FOREIGN KEY (profile_id,profile_kind) REFERENCES configuration_profiles(id,kind) ON DELETE RESTRICT,
    CHECK (profile_kind='middleware' OR action<>'clone')
);

INSERT INTO configuration_profiles
    (id,kind,name,lifecycle,current_revision,created_by,created_at,deactivated_by,deactivated_at)
SELECT id,'scheduling',name,lifecycle,current_revision,created_by,created_at,deactivated_by,deactivated_at
FROM scheduling_profiles;
INSERT INTO configuration_profiles
    (id,kind,name,lifecycle,current_revision,created_by,created_at,deactivated_by,deactivated_at)
SELECT id,'middleware',name,lifecycle,current_revision,created_by,created_at,deactivated_by,deactivated_at
FROM middleware_profiles;
INSERT INTO configuration_profiles
    (id,kind,name,lifecycle,current_revision,created_by,created_at,deactivated_by,deactivated_at)
SELECT id,'certificate-issuer',name,lifecycle,current_revision,created_by,created_at,deactivated_by,deactivated_at
FROM cert_manager_issuer_profiles;

INSERT INTO configuration_profile_revisions
    (profile_id,revision,profile_kind,spec,spec_digest,assignments_digest,created_by,created_at)
SELECT profile_id,revision,'scheduling',spec,spec_digest,assignments_digest,created_by,created_at
FROM scheduling_profile_revisions;
INSERT INTO configuration_profile_revisions
    (profile_id,revision,profile_kind,spec,spec_digest,assignments_digest,created_by,created_at,cloned_from_profile_id,cloned_from_revision)
SELECT profile_id,revision,'middleware',spec,spec_digest,assignments_digest,created_by,created_at,cloned_from_profile_id,cloned_from_revision
FROM middleware_profile_revisions;
INSERT INTO configuration_profile_revisions
    (profile_id,revision,profile_kind,solver_type,spec,spec_digest,created_by,created_at)
SELECT profile_id,revision,'certificate-issuer',solver_type,spec,spec_digest,created_by,created_at
FROM cert_manager_issuer_profile_revisions;

INSERT INTO configuration_profile_assignments
    (profile_id,revision,profile_kind,ordinal,scope_type,scope_id)
SELECT profile_id,revision,'scheduling',ordinal,scope_type,scope_id FROM scheduling_profile_assignments;
INSERT INTO configuration_profile_assignments
    (profile_id,revision,profile_kind,ordinal,scope_type,scope_id)
SELECT profile_id,revision,'middleware',ordinal,scope_type,scope_id FROM middleware_profile_assignments;

INSERT INTO configuration_profile_commands
    (actor_id,profile_kind,idempotency_key,action,request_digest,profile_id,result_revision,request_id,created_at)
SELECT actor_id,'scheduling',idempotency_key,action,request_digest,profile_id,result_revision,request_id,created_at
FROM scheduling_profile_commands;
INSERT INTO configuration_profile_commands
    (actor_id,profile_kind,idempotency_key,action,request_digest,profile_id,result_revision,request_id,created_at)
SELECT actor_id,'middleware',idempotency_key,action,request_digest,profile_id,result_revision,request_id,created_at
FROM middleware_profile_commands;
INSERT INTO configuration_profile_commands
    (actor_id,profile_kind,idempotency_key,action,request_digest,profile_id,result_revision,request_id,created_at)
SELECT actor_id,'certificate-issuer',idempotency_key,action,request_digest,profile_id,result_revision,request_id,created_at
FROM cert_manager_issuer_commands;

ALTER TABLE configuration_profiles ADD CONSTRAINT configuration_profiles_current_revision_fk
    FOREIGN KEY (id,current_revision,kind)
    REFERENCES configuration_profile_revisions(profile_id,revision,profile_kind)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE cert_manager_issuer_observations DROP CONSTRAINT cert_manager_issuer_observations_profile_id_revision_fkey;
ALTER TABLE cert_manager_issuer_observations ADD CONSTRAINT cert_manager_issuer_observations_profile_revision_fkey
    FOREIGN KEY (profile_id,revision) REFERENCES configuration_profile_revisions(profile_id,revision) ON DELETE RESTRICT;
ALTER TABLE cert_manager_issuer_references DROP CONSTRAINT cert_manager_issuer_references_profile_id_revision_fkey;
ALTER TABLE cert_manager_issuer_references ADD CONSTRAINT cert_manager_issuer_references_profile_revision_fkey
    FOREIGN KEY (profile_id,revision) REFERENCES configuration_profile_revisions(profile_id,revision) ON DELETE RESTRICT;
ALTER TABLE middleware_profile_references DROP CONSTRAINT middleware_profile_references_profile_id_revision_fkey;
ALTER TABLE middleware_profile_references ADD CONSTRAINT middleware_profile_references_profile_revision_fkey
    FOREIGN KEY (profile_id,revision) REFERENCES configuration_profile_revisions(profile_id,revision) ON DELETE RESTRICT;

DROP TRIGGER teams_scheduling_assignment_restrict ON teams;
DROP TRIGGER projects_scheduling_assignment_restrict ON projects;
DROP TRIGGER environments_scheduling_assignment_restrict ON environments;

ALTER TABLE scheduling_profiles DROP CONSTRAINT scheduling_profiles_current_revision_fk;
ALTER TABLE middleware_profiles DROP CONSTRAINT middleware_profiles_current_revision_fk;
ALTER TABLE cert_manager_issuer_profiles DROP CONSTRAINT cert_manager_issuer_profiles_current_revision_fk;

DROP TABLE scheduling_profile_commands;
DROP TABLE scheduling_profile_assignments;
DROP TABLE scheduling_profile_revisions;
DROP TABLE scheduling_profiles;
DROP TABLE middleware_profile_commands;
DROP TABLE middleware_profile_assignments;
DROP TABLE middleware_profile_revisions;
DROP TABLE middleware_profiles;
DROP TABLE cert_manager_issuer_commands;
DROP TABLE cert_manager_issuer_profile_revisions;
DROP TABLE cert_manager_issuer_profiles;

CREATE OR REPLACE FUNCTION validate_configuration_profile_revision()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.profile_kind='certificate-issuer' THEN
        IF NEW.solver_type NOT IN ('http01','dns01-cloudflare') OR
           NEW.assignments_digest IS NOT NULL OR NEW.cloned_from_profile_id IS NOT NULL OR
           octet_length(NEW.spec::text)>32768 THEN
            RAISE EXCEPTION 'invalid certificate issuer profile revision' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.profile_kind='scheduling' THEN
        IF NEW.solver_type IS NOT NULL OR NEW.assignments_digest IS NULL OR NEW.cloned_from_profile_id IS NOT NULL THEN
            RAISE EXCEPTION 'invalid scheduling profile revision' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.profile_kind='middleware' THEN
        IF NEW.solver_type IS NOT NULL OR NEW.assignments_digest IS NULL THEN
            RAISE EXCEPTION 'invalid middleware profile revision' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER configuration_profile_revision_validate
    BEFORE INSERT ON configuration_profile_revisions
    FOR EACH ROW EXECUTE FUNCTION validate_configuration_profile_revision();

CREATE OR REPLACE FUNCTION validate_configuration_profile_assignment()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.profile_kind='scheduling' THEN
        IF NEW.scope_type NOT IN ('team','project','environment') OR
           (NEW.scope_type='team' AND NOT EXISTS (SELECT 1 FROM teams WHERE id=NEW.scope_id)) OR
           (NEW.scope_type='project' AND NOT EXISTS (SELECT 1 FROM projects WHERE id=NEW.scope_id)) OR
           (NEW.scope_type='environment' AND NOT EXISTS (SELECT 1 FROM environments WHERE id=NEW.scope_id)) THEN
            RAISE EXCEPTION 'scheduling profile assignment scope does not exist' USING ERRCODE='23503';
        END IF;
    ELSIF NEW.profile_kind='middleware' THEN
        IF NEW.scope_type NOT IN ('project','environment','application') OR
           (NEW.scope_type='project' AND NOT EXISTS (SELECT 1 FROM projects WHERE id=NEW.scope_id)) OR
           (NEW.scope_type='environment' AND NOT EXISTS (SELECT 1 FROM environments WHERE id=NEW.scope_id)) OR
           (NEW.scope_type='application' AND NOT EXISTS (SELECT 1 FROM applications WHERE id=NEW.scope_id)) THEN
            RAISE EXCEPTION 'middleware profile assignment scope does not exist' USING ERRCODE='23503';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER configuration_profile_assignment_validate
    BEFORE INSERT ON configuration_profile_assignments
    FOR EACH ROW EXECUTE FUNCTION validate_configuration_profile_assignment();

CREATE OR REPLACE FUNCTION reject_configuration_profile_immutable_change()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'configuration profile immutable record cannot change' USING ERRCODE='23514';
END;
$$;
CREATE TRIGGER configuration_profile_revisions_immutable
    BEFORE UPDATE OR DELETE ON configuration_profile_revisions
    FOR EACH ROW EXECUTE FUNCTION reject_configuration_profile_immutable_change();
CREATE TRIGGER configuration_profile_assignments_immutable
    BEFORE UPDATE OR DELETE ON configuration_profile_assignments
    FOR EACH ROW EXECUTE FUNCTION reject_configuration_profile_immutable_change();
CREATE TRIGGER configuration_profile_commands_immutable
    BEFORE UPDATE OR DELETE ON configuration_profile_commands
    FOR EACH ROW EXECUTE FUNCTION reject_configuration_profile_immutable_change();

CREATE OR REPLACE FUNCTION protect_configuration_profile()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id,NEW.kind,NEW.name,NEW.created_by,NEW.created_at)
       IS DISTINCT FROM ROW(OLD.id,OLD.kind,OLD.name,OLD.created_by,OLD.created_at) OR
       OLD.lifecycle='deactivated' OR
       NOT ((NEW.lifecycle='active' AND NEW.current_revision IN (OLD.current_revision,OLD.current_revision+1)
             AND NEW.deactivated_by IS NULL AND NEW.deactivated_at IS NULL) OR
            (OLD.lifecycle='active' AND NEW.lifecycle='deactivated' AND NEW.current_revision=OLD.current_revision
             AND NEW.deactivated_by IS NOT NULL AND NEW.deactivated_at IS NOT NULL)) THEN
        RAISE EXCEPTION 'invalid configuration profile transition' USING ERRCODE='23514';
    END IF;
    IF NEW.kind='certificate-issuer' AND
       (NEW.lifecycle='deactivated' OR NEW.current_revision<>OLD.current_revision) AND
       EXISTS (SELECT 1 FROM cert_manager_issuer_references WHERE profile_id=OLD.id) THEN
        RAISE EXCEPTION 'referenced certificate issuer profile cannot change' USING ERRCODE='23503';
    END IF;
    IF NEW.kind='middleware' AND NEW.lifecycle='deactivated' AND
       EXISTS (SELECT 1 FROM middleware_profile_references WHERE profile_id=OLD.id) THEN
        RAISE EXCEPTION 'referenced middleware profile cannot be deactivated' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER configuration_profiles_protect
    BEFORE UPDATE ON configuration_profiles
    FOR EACH ROW EXECUTE FUNCTION protect_configuration_profile();

CREATE OR REPLACE FUNCTION validate_configuration_profile_command()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.profile_kind='certificate-issuer' AND
       NOT EXISTS (SELECT 1 FROM users WHERE id=NEW.actor_id AND role='platform-admin') THEN
        RAISE EXCEPTION 'certificate issuer mutation requires platform-admin' USING ERRCODE='42501';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER configuration_profile_command_validate
    BEFORE INSERT ON configuration_profile_commands
    FOR EACH ROW EXECUTE FUNCTION validate_configuration_profile_command();

CREATE OR REPLACE FUNCTION protect_configuration_assigned_scope()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE scope_kind text := TG_ARGV[0];
BEGIN
    IF EXISTS (SELECT 1 FROM configuration_profile_assignments WHERE scope_type=scope_kind AND scope_id=OLD.id) THEN
        RAISE EXCEPTION 'assigned configuration profile scope cannot be deleted' USING ERRCODE='23503';
    END IF;
    RETURN OLD;
END;
$$;
CREATE TRIGGER teams_configuration_assignment_restrict BEFORE DELETE ON teams
    FOR EACH ROW EXECUTE FUNCTION protect_configuration_assigned_scope('team');
CREATE TRIGGER projects_configuration_assignment_restrict BEFORE DELETE ON projects
    FOR EACH ROW EXECUTE FUNCTION protect_configuration_assigned_scope('project');
CREATE TRIGGER environments_configuration_assignment_restrict BEFORE DELETE ON environments
    FOR EACH ROW EXECUTE FUNCTION protect_configuration_assigned_scope('environment');
CREATE TRIGGER applications_configuration_assignment_restrict BEFORE DELETE ON applications
    FOR EACH ROW EXECUTE FUNCTION protect_configuration_assigned_scope('application');

CREATE OR REPLACE FUNCTION validate_certificate_issuer_profile_child()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM configuration_profile_revisions
        WHERE profile_id=NEW.profile_id AND revision=NEW.revision
          AND profile_kind='certificate-issuer'
    ) THEN
        RAISE EXCEPTION 'certificate issuer child references a non-issuer profile revision' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER cert_manager_issuer_observations_profile_kind
    BEFORE INSERT OR UPDATE ON cert_manager_issuer_observations
    FOR EACH ROW EXECUTE FUNCTION validate_certificate_issuer_profile_child();
CREATE TRIGGER cert_manager_issuer_references_profile_kind
    BEFORE INSERT OR UPDATE ON cert_manager_issuer_references
    FOR EACH ROW EXECUTE FUNCTION validate_certificate_issuer_profile_child();

CREATE OR REPLACE FUNCTION validate_cert_manager_issuer_reference_scope()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM configuration_profiles p
        JOIN applications a ON a.id=NEW.application_id
        JOIN environments e ON e.id=NEW.environment_id AND e.project_id=a.project_id
        JOIN git_repository_bindings b ON b.project_id=e.project_id AND b.environment_id=e.id AND b.kind='environment'
        WHERE p.id=NEW.profile_id AND p.kind='certificate-issuer'
          AND NEW.git_path=b.path_prefix || '/apps/' || a.id::text || '/app.yaml'
    ) THEN
        RAISE EXCEPTION 'certificate issuer reference destination mismatch' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION validate_middleware_profile_reference_scope()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM configuration_profiles p
        JOIN applications a ON a.id=NEW.application_id
        JOIN environments e ON e.id=NEW.environment_id AND e.project_id=a.project_id
        JOIN git_repository_bindings b ON b.project_id=e.project_id AND b.environment_id=e.id AND b.kind='environment'
        WHERE p.id=NEW.profile_id AND p.kind='middleware'
          AND NEW.git_path=b.path_prefix || '/apps/' || NEW.application_id::text || '/app.yaml'
    ) THEN
        RAISE EXCEPTION 'middleware profile reference destination does not match application/environment/Git binding' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION protect_managed_audit_event()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    event_type text;
    revision_value bigint;
    event_profile_kind text;
BEGIN
    IF TG_OP='DELETE' THEN event_type := OLD.target_type; ELSE event_type := NEW.target_type; END IF;
    IF TG_OP <> 'INSERT' THEN
        IF OLD.target_type IN ('scheduling-profile','middleware-profile','certificate-issuer-profile','auto-deploy-policy')
           OR (TG_OP='UPDATE' AND NEW.target_type IN ('scheduling-profile','middleware-profile','certificate-issuer-profile','auto-deploy-policy')) THEN
            RAISE EXCEPTION 'managed audit events are immutable' USING ERRCODE='23514';
        END IF;
        IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
    END IF;
    IF event_type NOT IN ('scheduling-profile','middleware-profile','certificate-issuer-profile','auto-deploy-policy') THEN
        RETURN NEW;
    END IF;
    IF NEW.detail->>'revision' !~ '^[1-9][0-9]*$' THEN
        RAISE EXCEPTION 'managed audit revision is invalid' USING ERRCODE='23514';
    END IF;
    revision_value := (NEW.detail->>'revision')::bigint;
    IF event_type IN ('scheduling-profile','middleware-profile') THEN
        event_profile_kind := CASE event_type WHEN 'scheduling-profile' THEN 'scheduling' ELSE 'middleware' END;
        IF NOT (NEW.detail ?& ARRAY['revision','idempotencyKey','specDigest','assignmentsDigest'])
           OR NEW.detail - ARRAY['revision','idempotencyKey','specDigest','assignmentsDigest'] <> '{}'::jsonb
           OR NEW.detail->>'idempotencyKey' !~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'
           OR NEW.detail->>'specDigest' !~ '^sha256:[0-9a-f]{64}$'
           OR NEW.detail->>'assignmentsDigest' !~ '^sha256:[0-9a-f]{64}$'
           OR (event_type='scheduling-profile' AND NEW.action NOT IN ('scheduling-profile.create','scheduling-profile.revise','scheduling-profile.deactivate'))
           OR (event_type='middleware-profile' AND NEW.action NOT IN ('middleware-profile.create','middleware-profile.revise','middleware-profile.clone','middleware-profile.deactivate'))
           OR NOT EXISTS (SELECT 1 FROM configuration_profile_revisions r
                WHERE r.profile_id=NEW.target_id AND r.revision=revision_value AND r.profile_kind=event_profile_kind
                  AND r.spec_digest=NEW.detail->>'specDigest'
                  AND r.assignments_digest=NEW.detail->>'assignmentsDigest') THEN
            RAISE EXCEPTION 'configuration profile audit authority mismatch' USING ERRCODE='23514';
        END IF;
    ELSIF event_type='certificate-issuer-profile' THEN
        IF NEW.action NOT IN ('certificate-issuer-profile.create','certificate-issuer-profile.revise','certificate-issuer-profile.deactivate')
           OR NOT (NEW.detail ?& ARRAY['revision','idempotencyKey','specDigest'])
           OR NEW.detail - ARRAY['revision','idempotencyKey','specDigest'] <> '{}'::jsonb
           OR NEW.detail->>'idempotencyKey' !~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'
           OR NEW.detail->>'specDigest' !~ '^sha256:[0-9a-f]{64}$'
           OR NOT EXISTS (SELECT 1 FROM users WHERE id=NEW.actor_id AND role='platform-admin')
           OR NOT EXISTS (SELECT 1 FROM configuration_profile_revisions r
                WHERE r.profile_id=NEW.target_id AND r.revision=revision_value AND r.profile_kind='certificate-issuer'
                  AND r.spec_digest=NEW.detail->>'specDigest') THEN
            RAISE EXCEPTION 'certificate issuer audit authority mismatch' USING ERRCODE='23514';
        END IF;
    ELSE
        IF NEW.action NOT IN ('auto-deploy-policy.create','auto-deploy-policy.revise','auto-deploy-policy.enable','auto-deploy-policy.disable')
           OR NOT (NEW.detail ?& ARRAY['revision','serviceActorId','templateDigest'])
           OR NEW.detail - ARRAY['revision','serviceActorId','templateDigest'] <> '{}'::jsonb
           OR NEW.detail->>'serviceActorId' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
           OR NEW.detail->>'templateDigest' !~ '^sha256:[0-9a-f]{64}$'
           OR NOT EXISTS (SELECT 1 FROM auto_deploy_policy_revisions r
                WHERE r.policy_id=NEW.target_id AND r.revision=revision_value
                  AND r.service_actor_id::text=NEW.detail->>'serviceActorId'
                  AND r.template_digest=NEW.detail->>'templateDigest') THEN
            RAISE EXCEPTION 'auto-deploy audit authority mismatch' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

DROP FUNCTION validate_scheduling_profile_assignment();
DROP FUNCTION protect_scheduling_assigned_scope();
DROP FUNCTION reject_scheduling_immutable_change();
DROP FUNCTION protect_scheduling_profile();
DROP FUNCTION validate_middleware_profile_assignment();
DROP FUNCTION reject_middleware_profile_immutable_change();
DROP FUNCTION protect_middleware_profile();
DROP FUNCTION reject_referenced_middleware_profile_deactivation();
DROP FUNCTION reject_cert_manager_issuer_immutable_change();
DROP FUNCTION protect_cert_manager_issuer_profile();
DROP FUNCTION require_cert_manager_issuer_platform_admin();
