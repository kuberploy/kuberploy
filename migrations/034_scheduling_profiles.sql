-- Scheduling profiles are platform-admin-owned, immutable Pod scheduling
-- policy. Workload callers select an exact revision; they never submit node,
-- taint, NodePool, or NodeClass mutations.
CREATE TABLE scheduling_profiles (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'),
    lifecycle text NOT NULL CHECK (lifecycle IN ('active','deactivated')),
    current_revision bigint NOT NULL CHECK (current_revision>0),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    deactivated_by uuid REFERENCES users(id) ON DELETE RESTRICT,
    deactivated_at timestamptz,
    CHECK ((lifecycle='active' AND deactivated_by IS NULL AND deactivated_at IS NULL) OR
           (lifecycle='deactivated' AND deactivated_by IS NOT NULL AND deactivated_at IS NOT NULL))
);

CREATE TABLE scheduling_profile_revisions (
    profile_id uuid NOT NULL REFERENCES scheduling_profiles(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision>0),
    spec jsonb NOT NULL CHECK (jsonb_typeof(spec)='object' AND octet_length(spec::text)<=65536),
    spec_digest text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    assignments_digest text NOT NULL CHECK (assignments_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (profile_id,revision)
);
ALTER TABLE scheduling_profiles ADD CONSTRAINT scheduling_profiles_current_revision_fk
    FOREIGN KEY (id,current_revision) REFERENCES scheduling_profile_revisions(profile_id,revision)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE scheduling_profile_assignments (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal>=0),
    scope_type text NOT NULL CHECK (scope_type IN ('team','project','environment')),
    scope_id uuid NOT NULL,
    PRIMARY KEY (profile_id,revision,ordinal),
    UNIQUE (profile_id,revision,scope_type,scope_id),
    FOREIGN KEY (profile_id,revision) REFERENCES scheduling_profile_revisions(profile_id,revision) ON DELETE RESTRICT
);

CREATE OR REPLACE FUNCTION validate_scheduling_profile_assignment()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.scope_type='team' AND NOT EXISTS (SELECT 1 FROM teams WHERE id=NEW.scope_id)) OR
       (NEW.scope_type='project' AND NOT EXISTS (SELECT 1 FROM projects WHERE id=NEW.scope_id)) OR
       (NEW.scope_type='environment' AND NOT EXISTS (SELECT 1 FROM environments WHERE id=NEW.scope_id)) THEN
        RAISE EXCEPTION 'scheduling profile assignment scope does not exist' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER scheduling_profile_assignment_validate BEFORE INSERT ON scheduling_profile_assignments
    FOR EACH ROW EXECUTE FUNCTION validate_scheduling_profile_assignment();

CREATE OR REPLACE FUNCTION protect_scheduling_assigned_scope()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE kind text := TG_ARGV[0];
BEGIN
    IF EXISTS (SELECT 1 FROM scheduling_profile_assignments WHERE scope_type=kind AND scope_id=OLD.id) THEN
        RAISE EXCEPTION 'assigned scheduling scope cannot be deleted' USING ERRCODE='23503';
    END IF;
    RETURN OLD;
END;
$$;
CREATE TRIGGER teams_scheduling_assignment_restrict BEFORE DELETE ON teams
    FOR EACH ROW EXECUTE FUNCTION protect_scheduling_assigned_scope('team');
CREATE TRIGGER projects_scheduling_assignment_restrict BEFORE DELETE ON projects
    FOR EACH ROW EXECUTE FUNCTION protect_scheduling_assigned_scope('project');
CREATE TRIGGER environments_scheduling_assignment_restrict BEFORE DELETE ON environments
    FOR EACH ROW EXECUTE FUNCTION protect_scheduling_assigned_scope('environment');

CREATE TABLE scheduling_profile_commands (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    action text NOT NULL CHECK (action IN ('create','revise','deactivate')),
    request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    profile_id uuid NOT NULL REFERENCES scheduling_profiles(id) ON DELETE RESTRICT,
    result_revision bigint NOT NULL CHECK (result_revision>0),
    request_id text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (actor_id,idempotency_key)
);
CREATE TABLE scheduling_profile_audit (
    id uuid PRIMARY KEY,
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    action text NOT NULL CHECK (action IN ('create','revise','deactivate')),
    profile_id uuid NOT NULL REFERENCES scheduling_profiles(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision>0),
    request_id text NOT NULL,
    idempotency_key text NOT NULL,
    spec_digest text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    assignments_digest text NOT NULL CHECK (assignments_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL
);

CREATE OR REPLACE FUNCTION reject_scheduling_immutable_change()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'scheduling profile immutable record cannot change' USING ERRCODE='23514';
END; $$;
CREATE TRIGGER scheduling_profile_revisions_immutable BEFORE UPDATE OR DELETE ON scheduling_profile_revisions FOR EACH ROW EXECUTE FUNCTION reject_scheduling_immutable_change();
CREATE TRIGGER scheduling_profile_assignments_immutable BEFORE UPDATE OR DELETE ON scheduling_profile_assignments FOR EACH ROW EXECUTE FUNCTION reject_scheduling_immutable_change();
CREATE TRIGGER scheduling_profile_commands_immutable BEFORE UPDATE OR DELETE ON scheduling_profile_commands FOR EACH ROW EXECUTE FUNCTION reject_scheduling_immutable_change();
CREATE TRIGGER scheduling_profile_audit_immutable BEFORE UPDATE OR DELETE ON scheduling_profile_audit FOR EACH ROW EXECUTE FUNCTION reject_scheduling_immutable_change();

CREATE OR REPLACE FUNCTION protect_scheduling_profile()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.id<>OLD.id OR NEW.name<>OLD.name OR NEW.created_by<>OLD.created_by OR NEW.created_at<>OLD.created_at OR
       NEW.current_revision<OLD.current_revision OR NEW.current_revision>OLD.current_revision+1 OR
       OLD.lifecycle='deactivated' OR
       (NEW.lifecycle='active' AND (NEW.deactivated_by IS NOT NULL OR NEW.deactivated_at IS NOT NULL)) THEN
        RAISE EXCEPTION 'invalid scheduling profile transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER scheduling_profiles_protect BEFORE UPDATE ON scheduling_profiles FOR EACH ROW EXECUTE FUNCTION protect_scheduling_profile();
