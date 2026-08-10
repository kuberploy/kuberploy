-- Git provider verification and shadow indexing perform bounded external I/O,
-- so row locks cannot safely span the work. Add an expiring per-binding lease
-- with a monotonically increasing epoch. The epoch fences a stale process even
-- when it restarts with the same owner identity.
ALTER TABLE git_safety_poll_cursors
    ADD COLUMN lease_owner text,
    ADD COLUMN lease_epoch bigint NOT NULL DEFAULT 0,
    ADD COLUMN lease_until timestamptz,
    ADD COLUMN reconciled_binding_updated_at timestamptz,
    ADD COLUMN last_error_code text NOT NULL DEFAULT '',
    ADD CONSTRAINT git_safety_poll_lease_epoch_valid CHECK (lease_epoch>=0),
    ADD CONSTRAINT git_safety_poll_lease_shape CHECK (
        (lease_owner IS NULL AND lease_until IS NULL) OR
        (lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$' AND lease_epoch>0 AND lease_until>updated_at)
    ),
    ADD CONSTRAINT git_safety_poll_reconciled_time CHECK (
        reconciled_binding_updated_at IS NULL OR reconciled_binding_updated_at<=updated_at
    ),
    ADD CONSTRAINT git_safety_poll_error_code CHECK (
        last_error_code='' OR last_error_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    );

CREATE INDEX IF NOT EXISTS git_safety_poll_reconcile_due_idx
    ON git_safety_poll_cursors(lease_until,next_poll_at,binding_id);

CREATE OR REPLACE FUNCTION protect_git_reconciliation_lease_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.lease_epoch<OLD.lease_epoch THEN
        RAISE EXCEPTION 'Git reconciliation lease epoch cannot move backwards'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_owner IS NOT NULL AND OLD.lease_owner IS NULL
       AND NEW.lease_epoch<>OLD.lease_epoch+1 THEN
        RAISE EXCEPTION 'Git reconciliation acquisition must increment the lease epoch'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_owner IS NOT NULL AND OLD.lease_owner IS NOT NULL
       AND (NEW.lease_owner<>OLD.lease_owner OR NEW.lease_epoch<>OLD.lease_epoch)
       AND NEW.lease_epoch<>OLD.lease_epoch+1 THEN
        RAISE EXCEPTION 'Git reconciliation replacement must increment the lease epoch'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS git_safety_poll_lease_epoch ON git_safety_poll_cursors;
CREATE TRIGGER git_safety_poll_lease_epoch
    BEFORE UPDATE ON git_safety_poll_cursors
    FOR EACH ROW EXECUTE FUNCTION protect_git_reconciliation_lease_epoch();
