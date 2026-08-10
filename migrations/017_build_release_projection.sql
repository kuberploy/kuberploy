-- Durable, replay-safe projection of a verified source-build result into the
-- registry release/cache lifecycle. The build result remains authoritative in
-- build_attempts; this table is only the recoverable handoff state machine.

CREATE TABLE IF NOT EXISTS build_release_projections (
    attempt_id uuid PRIMARY KEY REFERENCES build_attempts(id) ON DELETE RESTRICT,
    state text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending','processing','succeeded','failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 20),
    available_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    failure_code text NOT NULL DEFAULT '',
    release_id uuid,
    cache_generation_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    CHECK ((lease_owner IS NULL) = (lease_until IS NULL)),
    CHECK ((state='processing') = (lease_owner IS NOT NULL)),
    CHECK ((state IN ('succeeded','failed')) = (completed_at IS NOT NULL)),
    CHECK (state='succeeded' OR release_id IS NULL),
    CHECK (state<>'succeeded' OR failure_code='')
);

CREATE INDEX IF NOT EXISTS build_release_projections_work_idx
    ON build_release_projections(available_at,created_at,attempt_id)
    WHERE state IN ('pending','processing');

CREATE OR REPLACE FUNCTION enqueue_build_release_projection()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state <> 'succeeded' THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.state = 'succeeded' THEN
        RETURN NEW;
    END IF;
    INSERT INTO build_release_projections(attempt_id,available_at,created_at,updated_at)
    VALUES(NEW.id,NEW.completed_at,NEW.completed_at,NEW.completed_at)
    ON CONFLICT(attempt_id) DO NOTHING;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS build_attempts_enqueue_release_projection ON build_attempts;
CREATE TRIGGER build_attempts_enqueue_release_projection
    AFTER INSERT OR UPDATE OF state ON build_attempts
    FOR EACH ROW EXECUTE FUNCTION enqueue_build_release_projection();

-- Existing successful attempts are made recoverable when upgrading an older
-- installation. Deterministic release/cache identities make replay harmless.
INSERT INTO build_release_projections(attempt_id,available_at,created_at,updated_at)
SELECT id,completed_at,completed_at,completed_at
FROM build_attempts
WHERE state='succeeded' AND completed_at IS NOT NULL
ON CONFLICT(attempt_id) DO NOTHING;
