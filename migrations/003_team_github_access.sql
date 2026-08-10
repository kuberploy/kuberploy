ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users
    ADD CONSTRAINT users_role_check CHECK (role IN ('platform-admin','developer'));

CREATE TABLE IF NOT EXISTS user_invitations (
    id uuid PRIMARY KEY,
    token_hash bytea NOT NULL UNIQUE,
    display_name text NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    accepted_user_id uuid REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((accepted_at IS NULL) = (accepted_user_id IS NULL))
);
CREATE INDEX IF NOT EXISTS user_invitations_expires_idx
    ON user_invitations (expires_at) WHERE accepted_at IS NULL;

CREATE TABLE IF NOT EXISTS teams (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    slug text NOT NULL UNIQUE,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS team_memberships (
    team_id uuid NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('owner','member')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id,user_id)
);
CREATE INDEX IF NOT EXISTS team_memberships_user_idx
    ON team_memberships (user_id,team_id);

CREATE TABLE IF NOT EXISTS github_installations (
    id uuid PRIMARY KEY,
    github_installation_id bigint NOT NULL UNIQUE CHECK (github_installation_id > 0),
    account_login text NOT NULL,
    account_type text NOT NULL CHECK (account_type IN ('User','Organization')),
    owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    visibility text NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','team')),
    team_id uuid REFERENCES teams(id) ON DELETE RESTRICT,
    repository_selection text NOT NULL CHECK (repository_selection IN ('all','selected')),
    repository_count integer NOT NULL CHECK (repository_count >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((visibility='private' AND team_id IS NULL) OR (visibility='team' AND team_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS github_installations_owner_idx
    ON github_installations (owner_user_id,created_at);
CREATE INDEX IF NOT EXISTS github_installations_team_idx
    ON github_installations (team_id,created_at) WHERE visibility='team';

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS team_id uuid REFERENCES teams(id) ON DELETE RESTRICT;
CREATE INDEX IF NOT EXISTS projects_team_idx ON projects (team_id,created_at);
