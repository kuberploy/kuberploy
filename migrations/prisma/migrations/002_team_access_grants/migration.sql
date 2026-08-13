-- One existing grant relation owns both user and team subjects. This avoids a
-- parallel grant table and keeps authorization evaluation uniform.
ALTER TABLE access_grants
  ALTER COLUMN subject_user_id DROP NOT NULL,
  ADD COLUMN subject_team_id uuid;

ALTER TABLE access_grants
  ADD CONSTRAINT access_grants_subject_team_id_fkey
    FOREIGN KEY (subject_team_id) REFERENCES teams(id) ON DELETE CASCADE,
  ADD CONSTRAINT access_grants_exactly_one_subject_check
    CHECK ((subject_user_id IS NOT NULL) <> (subject_team_id IS NOT NULL));

ALTER TABLE access_grants
  DROP CONSTRAINT access_grants_subject_user_id_role_scope_type_scope_id_key;

DROP INDEX access_grants_scope_idx;
DROP INDEX access_grants_subject_idx;

CREATE UNIQUE INDEX access_grants_user_subject_unique
  ON access_grants(subject_user_id, role, scope_type, scope_id)
  WHERE subject_user_id IS NOT NULL;
CREATE UNIQUE INDEX access_grants_team_subject_unique
  ON access_grants(subject_team_id, role, scope_type, scope_id)
  WHERE subject_team_id IS NOT NULL;
CREATE INDEX access_grants_scope_idx
  ON access_grants(scope_type, scope_id, subject_user_id, subject_team_id);
CREATE INDEX access_grants_user_subject_idx
  ON access_grants(subject_user_id, scope_type, scope_id)
  WHERE subject_user_id IS NOT NULL;
CREATE INDEX access_grants_team_subject_idx
  ON access_grants(subject_team_id, scope_type, scope_id)
  WHERE subject_team_id IS NOT NULL;
