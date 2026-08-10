-- sslip.io route names are derived only from a freshly observed, public
-- Traefik LoadBalancer IPv4 address. AppConfig and API callers never select an
-- IP address or arbitrary sslip.io hostname. Hostname-based cloud load
-- balancers are eligible only when their live DNS answers contain an exact
-- operator-approved static IPv4.
CREATE TABLE edge_sslip_ingress_observations (
    target_key text NOT NULL,
    profile_revision bigint NOT NULL CHECK (profile_revision > 0),
    desired_digest text NOT NULL CHECK (desired_digest ~ '^sha256:[0-9a-f]{64}$'),
    runtime_config_digest text NOT NULL CHECK (runtime_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    public_ipv4 inet NOT NULL CHECK (
        family(public_ipv4)=4 AND masklen(public_ipv4)=32 AND
        NOT (public_ipv4 <<= inet '0.0.0.0/8') AND
        NOT (public_ipv4 <<= inet '10.0.0.0/8') AND
        NOT (public_ipv4 <<= inet '100.64.0.0/10') AND
        NOT (public_ipv4 <<= inet '127.0.0.0/8') AND
        NOT (public_ipv4 <<= inet '169.254.0.0/16') AND
        NOT (public_ipv4 <<= inet '172.16.0.0/12') AND
        NOT (public_ipv4 <<= inet '192.0.0.0/24') AND
        NOT (public_ipv4 <<= inet '192.0.2.0/24') AND
        NOT (public_ipv4 <<= inet '192.88.99.0/24') AND
        NOT (public_ipv4 <<= inet '192.168.0.0/16') AND
        NOT (public_ipv4 <<= inet '198.18.0.0/15') AND
        NOT (public_ipv4 <<= inet '198.51.100.0/24') AND
        NOT (public_ipv4 <<= inet '203.0.113.0/24') AND
        NOT (public_ipv4 <<= inet '224.0.0.0/4') AND
        NOT (public_ipv4 <<= inet '240.0.0.0/4')
    ),
    source_kind text NOT NULL CHECK (source_kind IN ('service-ip','verified-static-ip')),
    service_uid uuid NOT NULL,
    service_resource_version text NOT NULL CHECK (
        length(service_resource_version) BETWEEN 1 AND 128 AND
        service_resource_version ~ '^[A-Za-z0-9._:/+\-]+$'
    ),
    worker_id text NOT NULL CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    lease_epoch bigint NOT NULL CHECK (lease_epoch > 0),
    observed_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (target_key,profile_revision),
    FOREIGN KEY (target_key,profile_revision)
        REFERENCES edge_runtime_targets(target_key,profile_revision) ON DELETE RESTRICT,
    CHECK (target_key='traefik' AND updated_at=observed_at AND observed_at>=created_at)
);

CREATE INDEX edge_sslip_ingress_fresh_idx
    ON edge_sslip_ingress_observations(runtime_config_digest,observed_at DESC);

CREATE OR REPLACE FUNCTION protect_edge_sslip_ingress_observation()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    target edge_runtime_targets%ROWTYPE;
BEGIN
    IF TG_OP='DELETE' THEN
        SELECT * INTO target
          FROM edge_runtime_targets
         WHERE target_key=OLD.target_key AND profile_revision=OLD.profile_revision
         FOR SHARE;
        IF FOUND AND target.active THEN
            RAISE EXCEPTION 'an active sslip ingress observation cannot be deleted'
                USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;

    SELECT * INTO target
      FROM edge_runtime_targets
     WHERE target_key=NEW.target_key AND profile_revision=NEW.profile_revision
     FOR SHARE;
    IF NOT FOUND OR NOT target.active OR target.kind<>'traefik' OR
       target.desired_digest<>NEW.desired_digest OR
       target.runtime_config_digest<>NEW.runtime_config_digest OR
       target.lease_owner<>NEW.worker_id OR target.lease_epoch<>NEW.lease_epoch OR
       target.worker_contract<>'edge-observer.v1' OR
       target.worker_config_digest<>NEW.runtime_config_digest OR
       target.lease_until IS NULL OR target.lease_until<=NEW.observed_at THEN
        RAISE EXCEPTION 'sslip observation is not fenced by the exact live Traefik lease'
            USING ERRCODE='23514';
    END IF;

    IF TG_OP='INSERT' THEN
        IF NEW.created_at<>NEW.observed_at OR NEW.updated_at<>NEW.observed_at THEN
            RAISE EXCEPTION 'sslip observation creation receipt is not pristine'
                USING ERRCODE='23514';
        END IF;
    ELSE
        IF ROW(NEW.target_key,NEW.profile_revision,NEW.desired_digest,
               NEW.public_ipv4,NEW.source_kind,NEW.service_uid,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.target_key,OLD.profile_revision,OLD.desired_digest,
               OLD.public_ipv4,OLD.source_kind,OLD.service_uid,OLD.created_at) OR
           NEW.lease_epoch<=OLD.lease_epoch OR NEW.observed_at<OLD.observed_at THEN
            RAISE EXCEPTION 'sslip endpoint identity is immutable or observation time regressed'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER edge_sslip_ingress_observation_protect
    BEFORE INSERT OR UPDATE OR DELETE ON edge_sslip_ingress_observations
    FOR EACH ROW EXECUTE FUNCTION protect_edge_sslip_ingress_observation();
