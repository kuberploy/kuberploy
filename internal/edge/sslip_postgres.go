package edge

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type SSLIPHostnameRequest struct {
	ApplicationID string
	EnvironmentID string
	ProjectID     string
	Namespace     string
}

func (r SSLIPHostnameRequest) Validate() error {
	if !uuidPattern.MatchString(r.ApplicationID) || !uuidPattern.MatchString(r.EnvironmentID) ||
		!uuidPattern.MatchString(r.ProjectID) || !dnsLabelPattern.MatchString(r.Namespace) {
		return ErrInvalid
	}
	return nil
}

type SSLIPHostnameResolution struct {
	Hostname   string
	Source     string
	ObservedAt time.Time
}

func (r SSLIPHostnameResolution) Validate() error {
	if !validDNSName(r.Hostname) || !stringsHasSSLIPSuffix(r.Hostname) ||
		(r.Source != SSLIPSourceServiceIP && r.Source != SSLIPSourceVerifiedStaticIP) || r.ObservedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

func stringsHasSSLIPSuffix(value string) bool {
	return len(value) > len(SSLIPZone)+1 && value[len(value)-len(SSLIPZone)-1:] == "."+SSLIPZone
}

type PostgreSQLSSLIPResolver struct {
	pool   *pgxpool.Pool
	owned  bool
	Config RuntimeConfig
	Now    func() time.Time
}

func NewPostgreSQLSSLIPResolver(pool *pgxpool.Pool, config RuntimeConfig) (*PostgreSQLSSLIPResolver, error) {
	if pool == nil || config.Validate() != nil || !config.Enabled || config.Profiles.Traefik == nil || config.Profiles.Traefik.SSLIP == nil {
		return nil, ErrUnavailable
	}
	return &PostgreSQLSSLIPResolver{pool: pool, Config: config}, nil
}

func OpenPostgreSQLSSLIPResolver(ctx context.Context, databaseURL string, config RuntimeConfig) (*PostgreSQLSSLIPResolver, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrInvalid
	}
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "kuberploy-sslip-resolver"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	resolver, err := NewPostgreSQLSSLIPResolver(pool, config)
	if err != nil {
		pool.Close()
		return nil, err
	}
	resolver.owned = true
	return resolver, nil
}

func (r *PostgreSQLSSLIPResolver) Close() {
	if r != nil && r.pool != nil && r.owned {
		r.pool.Close()
		r.pool = nil
	}
}

func NewPostgreSQLSSLIPResolverFromStore(store *PostgreSQLStore, config RuntimeConfig) (*PostgreSQLSSLIPResolver, error) {
	if store == nil || store.pool == nil {
		return nil, ErrUnavailable
	}
	return NewPostgreSQLSSLIPResolver(store.pool, config)
}

func (r *PostgreSQLSSLIPResolver) ResolveHostname(ctx context.Context, request SSLIPHostnameRequest) (SSLIPHostnameResolution, error) {
	if r == nil || r.pool == nil || request.Validate() != nil {
		return SSLIPHostnameResolution{}, ErrInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return SSLIPHostnameResolution{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	result, err := r.ResolveHostnameTx(ctx, tx, request, r.now())
	if err != nil {
		return SSLIPHostnameResolution{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return SSLIPHostnameResolution{}, classifyPostgreSQL(err)
	}
	return result, nil
}

// Probe verifies the exact current Traefik target and its admitted worker
// runtime. Managed ExternalDNS targets are dynamic, so the static API startup
// digest cannot be used as the Traefik readiness identity.
func (r *PostgreSQLSSLIPResolver) Probe(ctx context.Context) error {
	if r == nil || r.pool == nil {
		return ErrUnavailable
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = r.currentObservationTx(ctx, tx, r.now()); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return classifyPostgreSQL(err)
	}
	return nil
}

func (r *PostgreSQLSSLIPResolver) ResolveHostnameTx(
	ctx context.Context,
	tx pgx.Tx,
	request SSLIPHostnameRequest,
	now time.Time,
) (SSLIPHostnameResolution, error) {
	if r == nil || tx == nil || request.Validate() != nil || now.IsZero() || r.Config.Validate() != nil ||
		!r.Config.Enabled || r.Config.Profiles.Traefik == nil || r.Config.Profiles.Traefik.SSLIP == nil {
		return SSLIPHostnameResolution{}, ErrInvalid
	}
	var marker int
	err := tx.QueryRow(ctx, `SELECT 1
		FROM applications a
		JOIN environments e ON e.id=$2 AND e.project_id=a.project_id
		WHERE a.id=$1 AND a.project_id=$3 AND e.namespace=$4
		FOR SHARE OF a,e`, request.ApplicationID, request.EnvironmentID, request.ProjectID, request.Namespace).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return SSLIPHostnameResolution{}, ErrNotFound
	}
	if err != nil {
		return SSLIPHostnameResolution{}, classifyPostgreSQL(err)
	}
	observation, err := r.currentObservationTx(ctx, tx, now)
	if err != nil {
		return SSLIPHostnameResolution{}, err
	}
	hostname, err := SSLIPHostname(request.ApplicationID, request.EnvironmentID, observation.Endpoint.PublicIPv4)
	if err != nil {
		return SSLIPHostnameResolution{}, ErrConflict
	}
	result := SSLIPHostnameResolution{Hostname: hostname, Source: observation.Endpoint.Source, ObservedAt: observation.ObservedAt.UTC()}
	if result.Validate() != nil {
		return SSLIPHostnameResolution{}, ErrConflict
	}
	return result, nil
}

func (r *PostgreSQLSSLIPResolver) currentObservationTx(
	ctx context.Context,
	tx pgx.Tx,
	now time.Time,
) (SSLIPIngressObservation, error) {
	if r == nil || tx == nil || now.IsZero() || r.Config.Validate() != nil || !r.Config.Enabled ||
		r.Config.Profiles.Traefik == nil || r.Config.Profiles.Traefik.SSLIP == nil {
		return SSLIPIngressObservation{}, ErrInvalid
	}
	configDigest, err := r.Config.Digest()
	if err != nil {
		return SSLIPIngressObservation{}, ErrInvalid
	}
	desired, err := (TargetProfile{Kind: KindTraefik, Traefik: r.Config.Profiles.Traefik}).Desired(configDigest)
	if err != nil {
		return SSLIPIngressObservation{}, ErrInvalid
	}
	var observation SSLIPIngressObservation
	var publicIPv4, targetState, targetDesiredDigest, targetRuntimeDigest string
	var targetActive bool
	var targetLastObserved time.Time
	err = tx.QueryRow(ctx, `SELECT target.active,target.runtime_state,target.desired_digest,target.runtime_config_digest,
		target.last_observed_at,observation.target_key,observation.profile_revision,observation.desired_digest,
		observation.runtime_config_digest,host(observation.public_ipv4),observation.source_kind,
		observation.service_uid::text,observation.service_resource_version,observation.observed_at
		FROM edge_runtime_targets target
		JOIN edge_sslip_ingress_observations observation
		  ON observation.target_key=target.target_key AND observation.profile_revision=target.profile_revision
		WHERE target.target_key=$1 AND target.profile_revision=$2
		FOR SHARE OF target,observation`, desired.Key, desired.Revision).Scan(
		&targetActive, &targetState, &targetDesiredDigest, &targetRuntimeDigest, &targetLastObserved,
		&observation.TargetKey, &observation.ProfileRevision, &observation.DesiredDigest,
		&observation.RuntimeConfigDigest, &publicIPv4, &observation.Endpoint.Source,
		&observation.Endpoint.ServiceUID, &observation.Endpoint.ServiceResourceVersion, &observation.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SSLIPIngressObservation{}, ErrUnavailable
	}
	if err != nil {
		return SSLIPIngressObservation{}, classifyPostgreSQL(err)
	}
	observation.Endpoint.PublicIPv4 = publicIPv4
	durableDesired := desired
	durableDesired.RuntimeConfigDigest = targetRuntimeDigest
	profile := r.Config.Profiles.Traefik.SSLIP
	modeMatches := profile.Mode == SSLIPAutoFirstIP && observation.Endpoint.Source == SSLIPSourceServiceIP ||
		profile.Mode == SSLIPVerifiedStaticIP && observation.Endpoint.PublicIPv4 == profile.StaticPublicIPv4
	if !targetActive || targetState != string(StateReady) || targetDesiredDigest != desired.DesiredDigest ||
		observation.Validate(durableDesired) != nil || !targetLastObserved.Equal(observation.ObservedAt) || !modeMatches ||
		observation.ObservedAt.After(now.UTC().Add(5*time.Second)) || observation.ObservedAt.Before(now.UTC().Add(-r.Config.ReadinessMaxAge)) {
		return SSLIPIngressObservation{}, ErrUnavailable
	}
	var workerReady bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM runtime_readiness WHERE runtime_kind='edge' AND scope_key='global'
		AND contract_version=$1 AND config_digest=$2
		  AND (identity->>'targetCount')::integer=(SELECT count(*) FROM edge_runtime_targets WHERE active)
		  AND observed_at BETWEEN $3::timestamptz-make_interval(secs=>$4) AND $3::timestamptz+interval '5 seconds'
		  AND lease_until>$3::timestamptz
	)`, RuntimeContract, targetRuntimeDigest, now.UTC(), int64(r.Config.ReadinessMaxAge.Seconds())).Scan(&workerReady)
	if err != nil {
		return SSLIPIngressObservation{}, classifyPostgreSQL(err)
	}
	if !workerReady {
		return SSLIPIngressObservation{}, ErrUnavailable
	}
	return observation, nil
}

func (r *PostgreSQLSSLIPResolver) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}
