-- Exact asset bytes, rather than a JSONB reserialization, bind every durable
-- upgrade to the release-manifest digest that the API verified.
ALTER TABLE platform_upgrades
    ADD COLUMN IF NOT EXISTS manifest_bytes bytea NOT NULL DEFAULT ''::bytea;

-- New upgrades must always provide the exact bytes. The temporary default
-- above only permits migration of installations with historical rows; those
-- rows fail closed in the runner because an empty manifest cannot verify.
ALTER TABLE platform_upgrades
    ALTER COLUMN manifest_bytes DROP DEFAULT;
