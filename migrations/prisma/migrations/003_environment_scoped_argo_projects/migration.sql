-- Each environment manifest owns its own AppProject. The namespace is already
-- a server-derived, globally unique DNS label and therefore supplies a stable
-- environment-scoped Argo project identity. Historical desired-state and
-- foundation receipts remain immutable evidence; their controllers create new
-- current receipts after this authority changes.
UPDATE public.environments
SET argo_project = namespace
WHERE argo_project IS DISTINCT FROM namespace;
