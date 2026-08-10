CREATE TABLE IF NOT EXISTS access_grants (
    id uuid PRIMARY KEY,
    subject_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('viewer','developer','project-admin','organization-admin','platform-admin')),
    scope_type text NOT NULL CHECK (scope_type IN ('platform','team','project','environment','namespace','application')),
    scope_id text NOT NULL CHECK (length(scope_id) BETWEEN 1 AND 253),
    permissions text[] NOT NULL DEFAULT ARRAY[]::text[],
    source text NOT NULL DEFAULT 'explicit' CHECK (source IN ('explicit','bootstrap')),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (subject_user_id,role,scope_type,scope_id),
    CHECK (permissions <@ ARRAY['logs.read']::text[]),
    CHECK (cardinality(permissions) <= 1),
    CHECK (role<>'organization-admin' OR scope_type='team'),
    CHECK (role<>'project-admin' OR scope_type='project'),
    CHECK ((role='platform-admin') = (scope_type='platform' AND scope_id='platform')),
    CHECK (scope_type<>'platform' OR (role='platform-admin' AND scope_id='platform')),
    CHECK ((source='bootstrap') = (role='platform-admin')),
    CHECK (
        (scope_type='platform' AND scope_id='platform') OR
        (scope_type='namespace' AND scope_id ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$') OR
        (scope_type IN ('team','project','environment','application') AND
            scope_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')
    )
);
CREATE INDEX IF NOT EXISTS access_grants_subject_idx
    ON access_grants (subject_user_id,scope_type,scope_id);
CREATE INDEX IF NOT EXISTS access_grants_scope_idx
    ON access_grants (scope_type,scope_id,subject_user_id);

-- Installations upgraded from schema 007 retain their administrator through
-- an explicit platform grant. New bootstrap transactions insert the same
-- durable grant before exposing the session.
INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at)
SELECT gen_random_uuid(),u.id,'platform-admin','platform','platform','bootstrap',u.id,u.created_at
FROM users u
WHERE u.role='platform-admin'
ON CONFLICT (subject_user_id,role,scope_type,scope_id) DO NOTHING;
