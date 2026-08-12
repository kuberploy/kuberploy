-- Personal and platform-owned projects intentionally have no team. Preserve
-- the same exact scope authority as team-owned projects by representing that
-- ownership as NULL and independently comparing it with projects.team_id.
ALTER TABLE public.secret_bindings
    ALTER COLUMN organization_id DROP NOT NULL;

CREATE OR REPLACE FUNCTION public.enforce_secret_binding_scope() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    durable_organization_id uuid;
BEGIN
    SELECT team_id INTO durable_organization_id
      FROM public.projects
      WHERE id=NEW.project_id
      FOR KEY SHARE;
    IF NOT FOUND OR NEW.organization_id IS DISTINCT FROM durable_organization_id THEN
        RAISE EXCEPTION 'Secret binding organization does not match project ownership'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER secret_bindings_scope
    BEFORE INSERT OR UPDATE ON public.secret_bindings
    FOR EACH ROW EXECUTE FUNCTION public.enforce_secret_binding_scope();
