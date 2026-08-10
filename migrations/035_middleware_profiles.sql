-- Reusable Traefik HTTP middleware profiles are immutable revisions assigned
-- to exact project/environment/application scopes. Kubernetes object names and
-- secret values never enter this catalog.
CREATE TABLE middleware_profiles (
    id uuid PRIMARY KEY,
    name text NOT NULL CHECK (name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'),
    lifecycle text NOT NULL CHECK (lifecycle IN ('active','deactivated')),
    current_revision bigint NOT NULL CHECK (current_revision > 0),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    deactivated_by uuid REFERENCES users(id) ON DELETE RESTRICT,
    deactivated_at timestamptz,
    CHECK ((lifecycle='active' AND deactivated_by IS NULL AND deactivated_at IS NULL) OR
           (lifecycle='deactivated' AND deactivated_by IS NOT NULL AND deactivated_at IS NOT NULL))
);
CREATE TABLE middleware_profile_revisions (
    profile_id uuid NOT NULL REFERENCES middleware_profiles(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision > 0),
    spec jsonb NOT NULL CHECK (jsonb_typeof(spec)='object' AND octet_length(spec::text)<=65536),
    spec_digest text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    assignments_digest text NOT NULL CHECK (assignments_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    cloned_from_profile_id uuid,
    cloned_from_revision bigint,
    PRIMARY KEY (profile_id,revision),
    FOREIGN KEY (cloned_from_profile_id,cloned_from_revision) REFERENCES middleware_profile_revisions(profile_id,revision) ON DELETE RESTRICT,
    CHECK ((cloned_from_profile_id IS NULL) = (cloned_from_revision IS NULL))
);
ALTER TABLE middleware_profiles ADD CONSTRAINT middleware_profiles_current_revision_fk
    FOREIGN KEY (id,current_revision) REFERENCES middleware_profile_revisions(profile_id,revision) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE middleware_profile_assignments (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal>=0),
    scope_type text NOT NULL CHECK (scope_type IN ('project','environment','application')),
    scope_id uuid NOT NULL,
    PRIMARY KEY (profile_id,revision,ordinal),
    UNIQUE (profile_id,revision,scope_type,scope_id),
    FOREIGN KEY (profile_id,revision) REFERENCES middleware_profile_revisions(profile_id,revision) ON DELETE RESTRICT
);
CREATE OR REPLACE FUNCTION validate_middleware_profile_assignment()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.scope_type='project' AND NOT EXISTS (SELECT 1 FROM projects WHERE id=NEW.scope_id)) OR
       (NEW.scope_type='environment' AND NOT EXISTS (SELECT 1 FROM environments WHERE id=NEW.scope_id)) OR
       (NEW.scope_type='application' AND NOT EXISTS (SELECT 1 FROM applications WHERE id=NEW.scope_id)) THEN
        RAISE EXCEPTION 'middleware profile assignment scope does not exist' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER middleware_profile_assignment_validate BEFORE INSERT ON middleware_profile_assignments FOR EACH ROW EXECUTE FUNCTION validate_middleware_profile_assignment();

CREATE TABLE middleware_profile_references (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    application_id uuid NOT NULL REFERENCES applications(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    git_path text NOT NULL CHECK (git_path<>'' AND octet_length(git_path)<=1024),
    logical_name text NOT NULL CHECK (logical_name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (git_path,profile_id,logical_name),
    FOREIGN KEY (profile_id,revision) REFERENCES middleware_profile_revisions(profile_id,revision) ON DELETE RESTRICT
);
CREATE INDEX middleware_profile_references_profile_idx ON middleware_profile_references(profile_id,git_path);
CREATE OR REPLACE FUNCTION validate_middleware_profile_reference_scope()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM applications a
        JOIN environments e ON e.id=NEW.environment_id AND e.project_id=a.project_id
        JOIN git_repository_bindings b ON b.project_id=e.project_id AND b.environment_id=e.id AND b.kind='environment'
        WHERE a.id=NEW.application_id
          AND NEW.git_path = b.path_prefix || '/apps/' || NEW.application_id::text || '/app.yaml'
    ) THEN
        RAISE EXCEPTION 'middleware profile reference destination does not match application/environment/Git binding' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER middleware_profile_reference_scope BEFORE INSERT ON middleware_profile_references FOR EACH ROW EXECUTE FUNCTION validate_middleware_profile_reference_scope();

CREATE TABLE middleware_profile_commands (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    action text NOT NULL CHECK (action IN ('create','revise','clone','deactivate')),
    request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    profile_id uuid NOT NULL REFERENCES middleware_profiles(id) ON DELETE RESTRICT,
    result_revision bigint NOT NULL CHECK (result_revision>0),
    request_id text NOT NULL CHECK (request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (actor_id,idempotency_key)
);
CREATE TABLE middleware_profile_audit (
    id uuid PRIMARY KEY,
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    action text NOT NULL CHECK (action IN ('create','revise','clone','deactivate')),
    profile_id uuid NOT NULL REFERENCES middleware_profiles(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision>0),
    request_id text NOT NULL CHECK (request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    spec_digest text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    assignments_digest text NOT NULL CHECK (assignments_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL
);

CREATE OR REPLACE FUNCTION reject_middleware_profile_immutable_change()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'middleware profile immutable record cannot change' USING ERRCODE='23514'; END; $$;
CREATE TRIGGER middleware_profile_revisions_immutable BEFORE UPDATE OR DELETE ON middleware_profile_revisions FOR EACH ROW EXECUTE FUNCTION reject_middleware_profile_immutable_change();
CREATE TRIGGER middleware_profile_assignments_immutable BEFORE UPDATE OR DELETE ON middleware_profile_assignments FOR EACH ROW EXECUTE FUNCTION reject_middleware_profile_immutable_change();
CREATE TRIGGER middleware_profile_commands_immutable BEFORE UPDATE OR DELETE ON middleware_profile_commands FOR EACH ROW EXECUTE FUNCTION reject_middleware_profile_immutable_change();
CREATE TRIGGER middleware_profile_audit_immutable BEFORE UPDATE OR DELETE ON middleware_profile_audit FOR EACH ROW EXECUTE FUNCTION reject_middleware_profile_immutable_change();

CREATE OR REPLACE FUNCTION protect_middleware_profile()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.id<>OLD.id OR NEW.name<>OLD.name OR NEW.created_by<>OLD.created_by OR NEW.created_at<>OLD.created_at OR OLD.lifecycle='deactivated' OR
       NOT ((NEW.lifecycle='active' AND NEW.current_revision IN (OLD.current_revision,OLD.current_revision+1) AND NEW.deactivated_by IS NULL AND NEW.deactivated_at IS NULL) OR
            (OLD.lifecycle='active' AND NEW.lifecycle='deactivated' AND NEW.current_revision=OLD.current_revision AND NEW.deactivated_by IS NOT NULL AND NEW.deactivated_at IS NOT NULL)) THEN
        RAISE EXCEPTION 'invalid middleware profile transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER middleware_profiles_protect BEFORE UPDATE ON middleware_profiles FOR EACH ROW EXECUTE FUNCTION protect_middleware_profile();

CREATE OR REPLACE FUNCTION reject_referenced_middleware_profile_deactivation()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.lifecycle='deactivated' AND OLD.lifecycle='active' AND EXISTS (SELECT 1 FROM middleware_profile_references WHERE profile_id=OLD.id) THEN
        RAISE EXCEPTION 'referenced middleware profile cannot be deactivated' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER middleware_profiles_referenced BEFORE UPDATE ON middleware_profiles FOR EACH ROW EXECUTE FUNCTION reject_referenced_middleware_profile_deactivation();
