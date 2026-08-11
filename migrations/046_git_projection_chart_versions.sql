-- Git projection commands bind rendered application bytes to the explicit
-- runtime chart release used to validate them. Releases originally used OCI
-- digests; the public operator contract now uses a readable semantic version.
-- Keep existing digest-backed rows valid while requiring every new textual
-- identity to be an exact stable or RC version.
ALTER TABLE git_deployment_write_commands
    DROP CONSTRAINT git_deployment_write_commands_chart_digest_check,
    ADD CONSTRAINT git_deployment_write_commands_chart_digest_check CHECK (
        chart_digest ~ '^(?:sha256:[0-9a-f]{64}|[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[0-9]+)?)$'
    );

ALTER TABLE deployment_config_previews
    DROP CONSTRAINT deployment_config_previews_git_shape,
    ADD CONSTRAINT deployment_config_previews_git_shape CHECK (
        (git_binding_id IS NULL AND git_base_revision IS NULL AND git_path IS NULL
            AND git_expected_etag IS NULL AND git_chart_digest IS NULL
            AND git_policy_version IS NULL) OR
        (git_binding_id IS NOT NULL
            AND git_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
            AND git_path IS NOT NULL AND length(git_path) BETWEEN 1 AND 1024
            AND git_path !~ '(^/|/\.\.?(/|$)|//|\\)'
            AND git_expected_etag ~ '^"sha256:[0-9a-f]{64}"$'
            AND git_chart_digest ~ '^(?:sha256:[0-9a-f]{64}|[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[0-9]+)?)$'
            AND length(git_policy_version) BETWEEN 1 AND 128
            AND git_policy_version !~ E'[\\x00\\r\\n]')
    );
