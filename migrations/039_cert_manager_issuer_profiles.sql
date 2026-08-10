-- Platform-admin cert-manager ClusterIssuer profiles. Desired specifications
-- are immutable revisions; tenant callers only see readiness-gated identities.
CREATE TABLE cert_manager_issuer_profiles (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'),
    lifecycle text NOT NULL CHECK (lifecycle IN ('active','deactivated')),
    current_revision bigint NOT NULL CHECK (current_revision > 0),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    deactivated_by uuid REFERENCES users(id) ON DELETE RESTRICT,
    deactivated_at timestamptz,
    CHECK ((lifecycle='active' AND deactivated_by IS NULL AND deactivated_at IS NULL) OR
           (lifecycle='deactivated' AND deactivated_by IS NOT NULL AND deactivated_at IS NOT NULL))
);

CREATE TABLE cert_manager_issuer_profile_revisions (
    profile_id uuid NOT NULL REFERENCES cert_manager_issuer_profiles(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision > 0),
    solver_type text NOT NULL CHECK (solver_type IN ('http01','dns01-cloudflare')),
    spec jsonb NOT NULL CHECK (jsonb_typeof(spec)='object' AND octet_length(spec::text)<=32768),
    spec_digest text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (profile_id,revision)
);
ALTER TABLE cert_manager_issuer_profiles ADD CONSTRAINT cert_manager_issuer_profiles_current_revision_fk
    FOREIGN KEY (id,current_revision) REFERENCES cert_manager_issuer_profile_revisions(profile_id,revision) DEFERRABLE INITIALLY DEFERRED;

-- One mutable observation per desired revision. It is never tenant authority:
-- the catalog requires an exact ready digest before returning an identity.
CREATE TABLE cert_manager_issuer_observations (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    state text NOT NULL CHECK (state IN ('pending','ready','degraded')),
    observed_spec_digest text CHECK (observed_spec_digest IS NULL OR observed_spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    observed_generation bigint CHECK (observed_generation IS NULL OR observed_generation>0),
    reason text NOT NULL DEFAULT '' CHECK (octet_length(reason)<=1024),
    observed_at timestamptz,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (profile_id,revision),
    FOREIGN KEY (profile_id,revision) REFERENCES cert_manager_issuer_profile_revisions(profile_id,revision) ON DELETE RESTRICT,
    CHECK ((state='pending' AND observed_spec_digest IS NULL AND observed_generation IS NULL AND observed_at IS NULL) OR
           (state IN ('ready','degraded') AND observed_spec_digest IS NOT NULL AND observed_generation IS NOT NULL AND observed_at IS NOT NULL))
);

CREATE TABLE cert_manager_issuer_references (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    application_id uuid NOT NULL REFERENCES applications(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    git_path text NOT NULL CHECK (git_path<>'' AND octet_length(git_path)<=1024),
    hostname text NOT NULL CHECK (hostname<>'' AND octet_length(hostname)<=253),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (git_path,hostname),
    FOREIGN KEY (profile_id,revision) REFERENCES cert_manager_issuer_profile_revisions(profile_id,revision) ON DELETE RESTRICT
);
CREATE INDEX cert_manager_issuer_references_profile_idx ON cert_manager_issuer_references(profile_id,git_path);
CREATE OR REPLACE FUNCTION validate_cert_manager_issuer_reference_scope()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM applications a JOIN environments e ON e.id=NEW.environment_id AND e.project_id=a.project_id JOIN git_repository_bindings b ON b.project_id=e.project_id AND b.environment_id=e.id AND b.kind='environment' WHERE a.id=NEW.application_id AND NEW.git_path=b.path_prefix || '/apps/' || a.id::text || '/app.yaml') THEN
        RAISE EXCEPTION 'cert-manager issuer reference destination mismatch' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER cert_manager_issuer_reference_scope BEFORE INSERT ON cert_manager_issuer_references FOR EACH ROW EXECUTE FUNCTION validate_cert_manager_issuer_reference_scope();

CREATE TABLE cert_manager_issuer_commands (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    action text NOT NULL CHECK (action IN ('create','revise','deactivate')),
    request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    profile_id uuid NOT NULL REFERENCES cert_manager_issuer_profiles(id) ON DELETE RESTRICT,
    result_revision bigint NOT NULL CHECK (result_revision>0),
    request_id text NOT NULL CHECK (request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (actor_id,idempotency_key)
);
CREATE TABLE cert_manager_issuer_audit (
    id uuid PRIMARY KEY,
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    action text NOT NULL CHECK (action IN ('create','revise','deactivate')),
    profile_id uuid NOT NULL REFERENCES cert_manager_issuer_profiles(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision>0),
    request_id text NOT NULL CHECK (request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    spec_digest text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL
);

CREATE OR REPLACE FUNCTION reject_cert_manager_issuer_immutable_change()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'cert-manager issuer immutable record cannot change' USING ERRCODE='23514'; END; $$;
CREATE TRIGGER cert_manager_issuer_revisions_immutable BEFORE UPDATE OR DELETE ON cert_manager_issuer_profile_revisions FOR EACH ROW EXECUTE FUNCTION reject_cert_manager_issuer_immutable_change();
CREATE TRIGGER cert_manager_issuer_commands_immutable BEFORE UPDATE OR DELETE ON cert_manager_issuer_commands FOR EACH ROW EXECUTE FUNCTION reject_cert_manager_issuer_immutable_change();
CREATE TRIGGER cert_manager_issuer_audit_immutable BEFORE UPDATE OR DELETE ON cert_manager_issuer_audit FOR EACH ROW EXECUTE FUNCTION reject_cert_manager_issuer_immutable_change();

CREATE OR REPLACE FUNCTION protect_cert_manager_issuer_profile()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.id<>OLD.id OR NEW.name<>OLD.name OR NEW.created_by<>OLD.created_by OR NEW.created_at<>OLD.created_at OR OLD.lifecycle='deactivated' OR
       NOT ((NEW.lifecycle='active' AND NEW.current_revision IN (OLD.current_revision,OLD.current_revision+1) AND NEW.deactivated_by IS NULL AND NEW.deactivated_at IS NULL) OR
            (OLD.lifecycle='active' AND NEW.lifecycle='deactivated' AND NEW.current_revision=OLD.current_revision AND NEW.deactivated_by IS NOT NULL AND NEW.deactivated_at IS NOT NULL)) THEN
        RAISE EXCEPTION 'invalid cert-manager issuer profile transition' USING ERRCODE='23514';
    END IF;
    IF (NEW.lifecycle='deactivated' OR NEW.current_revision<>OLD.current_revision) AND EXISTS (SELECT 1 FROM cert_manager_issuer_references WHERE profile_id=OLD.id) THEN
        RAISE EXCEPTION 'referenced cert-manager issuer profile cannot be revised or deactivated' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER cert_manager_issuer_profiles_protect BEFORE UPDATE ON cert_manager_issuer_profiles FOR EACH ROW EXECUTE FUNCTION protect_cert_manager_issuer_profile();

-- The SQL boundary also enforces platform-admin authority even if a future
-- transport accidentally calls the store with a tenant actor.
CREATE OR REPLACE FUNCTION require_cert_manager_issuer_platform_admin()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM users WHERE id=NEW.actor_id AND role='platform-admin') THEN
        RAISE EXCEPTION 'cert-manager issuer mutation requires platform-admin' USING ERRCODE='42501';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER cert_manager_issuer_commands_admin BEFORE INSERT ON cert_manager_issuer_commands FOR EACH ROW EXECUTE FUNCTION require_cert_manager_issuer_platform_admin();
CREATE TRIGGER cert_manager_issuer_audit_admin BEFORE INSERT ON cert_manager_issuer_audit FOR EACH ROW EXECUTE FUNCTION require_cert_manager_issuer_platform_admin();
