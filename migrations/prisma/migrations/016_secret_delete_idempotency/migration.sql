-- Durable secret-binding DELETE receipts use the existing mutation_receipts
-- identity/fingerprint columns. Keep the trigger aligned with the API's
-- idempotency contract while retaining the strict key shape.
CREATE OR REPLACE FUNCTION public.validate_mutation_receipt() RETURNS trigger
    LANGUAGE plpgsql
    AS $_$
BEGIN
    IF TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'mutation receipts are immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.receipt_kind='configuration-profile' AND NEW.namespace='certificate-issuer' AND
       NOT EXISTS (SELECT 1 FROM users WHERE id=NEW.actor_id AND role='platform-admin') THEN
        RAISE EXCEPTION 'certificate issuer mutation requires platform-admin' USING ERRCODE='42501';
    END IF;
    IF NEW.receipt_kind IN ('auto-deploy-policy','configuration-profile') AND
       (NEW.request_id !~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$' OR
        NEW.idempotency_key !~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$') THEN
        RAISE EXCEPTION 'mutation receipt identifier is invalid' USING ERRCODE='23514';
    END IF;
    IF NEW.receipt_kind='build-api' AND
       (NEW.namespace NOT IN ('definition.create','attempt.cancel','attempt.retry') OR
        length(NEW.idempotency_key) NOT BETWEEN 16 AND 128) THEN
        RAISE EXCEPTION 'build API mutation receipt is invalid' USING ERRCODE='23514';
    END IF;
    IF NEW.receipt_kind='secret-binding' AND
       (NEW.namespace NOT IN ('create','rotate','delete') OR NEW.idempotency_key !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$') THEN
        RAISE EXCEPTION 'secret binding mutation receipt is invalid' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$_$;
