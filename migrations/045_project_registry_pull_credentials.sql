-- Project-scoped pull credential catalog. Entries bind a human name to an
-- operator-owned registry target; credential bytes and Kubernetes Secret
-- coordinates never enter these tables or tenant API responses.
CREATE TABLE IF NOT EXISTS project_registry_pull_credentials (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE RESTRICT,
    name text NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name),
    UNIQUE (project_id, registry_target_id),
    CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9 ._-]{0,62}[A-Za-z0-9]$' OR name ~ '^[A-Za-z0-9]$')
);

CREATE TABLE IF NOT EXISTS application_registry_pull_selections (
    application_id uuid PRIMARY KEY REFERENCES applications(id) ON DELETE CASCADE,
    mode text NOT NULL CHECK (mode IN ('public','project-credential')),
    project_credential_id uuid REFERENCES project_registry_pull_credentials(id) ON DELETE RESTRICT,
    updated_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((mode='public' AND project_credential_id IS NULL) OR
           (mode='project-credential' AND project_credential_id IS NOT NULL))
);

CREATE OR REPLACE FUNCTION enforce_application_registry_pull_selection_scope()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    app_project uuid;
    credential_project uuid;
BEGIN
    SELECT project_id INTO STRICT app_project FROM applications WHERE id=NEW.application_id;
    IF NEW.mode='project-credential' THEN
        SELECT project_id INTO STRICT credential_project
          FROM project_registry_pull_credentials WHERE id=NEW.project_credential_id;
        IF app_project <> credential_project THEN
            RAISE EXCEPTION 'registry pull credential belongs to another project' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS application_registry_pull_selection_scope ON application_registry_pull_selections;
CREATE CONSTRAINT TRIGGER application_registry_pull_selection_scope
    AFTER INSERT OR UPDATE ON application_registry_pull_selections
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_application_registry_pull_selection_scope();

CREATE OR REPLACE FUNCTION enforce_project_registry_pull_target()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    pull_ref text;
BEGIN
    SELECT pull_credential_ref INTO STRICT pull_ref FROM registry_targets WHERE id=NEW.registry_target_id;
    IF pull_ref='' THEN
        RAISE EXCEPTION 'registry target has no pull credential' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS project_registry_pull_target ON project_registry_pull_credentials;
CREATE CONSTRAINT TRIGGER project_registry_pull_target
    AFTER INSERT OR UPDATE ON project_registry_pull_credentials
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_project_registry_pull_target();
