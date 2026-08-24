package builds

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *PostgreSQLStore) LatestBuilderPlatformSettings(ctx context.Context) (BuilderPlatformSettings, error) {
	return builderPlatformSettingsByQuery(ctx, s.pool, `
		SELECT revision,node_isolation,max_concurrent_builders,checkout_resources,dind_resources,agent_resources,updated_by::text,updated_at
		FROM builder_platform_settings_revisions ORDER BY revision DESC LIMIT 1`)
}

func (s *PostgreSQLStore) UpdateBuilderPlatformSettings(ctx context.Context, actorID, idempotencyKey, fingerprint string, expectedRevision int64, input BuilderPlatformSettingsInput, now time.Time) (BuilderPlatformSettings, bool, error) {
	if !uuidRE.MatchString(actorID) || strings.TrimSpace(idempotencyKey) != idempotencyKey || idempotencyKey == "" || len(idempotencyKey) > 128 ||
		!digestRE.MatchString(fingerprint) || expectedRevision < 0 || now.IsZero() || input.settings().Validate() != nil {
		return BuilderPlatformSettings{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return BuilderPlatformSettings{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kuberploy-builder-platform-settings'))`); err != nil {
		return BuilderPlatformSettings{}, false, classifyPostgres(err)
	}
	var replayFingerprint string
	var replayRevision int64
	err = tx.QueryRow(ctx, `SELECT request_fingerprint,revision FROM builder_platform_setting_mutations WHERE actor_id=$1 AND idempotency_key=$2`, actorID, idempotencyKey).Scan(&replayFingerprint, &replayRevision)
	if err == nil {
		if replayFingerprint != fingerprint {
			return BuilderPlatformSettings{}, false, ErrConflict
		}
		settings, queryErr := builderPlatformSettingsByQuery(ctx, tx, `
			SELECT revision,node_isolation,max_concurrent_builders,checkout_resources,dind_resources,agent_resources,updated_by::text,updated_at
			FROM builder_platform_settings_revisions WHERE revision=$1`, replayRevision)
		return settings, true, queryErr
	}
	if err != pgx.ErrNoRows {
		return BuilderPlatformSettings{}, false, classifyPostgres(err)
	}
	var currentRevision int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(revision),0) FROM builder_platform_settings_revisions`).Scan(&currentRevision); err != nil {
		return BuilderPlatformSettings{}, false, classifyPostgres(err)
	}
	if currentRevision != expectedRevision {
		return BuilderPlatformSettings{}, false, ErrConflict
	}
	checkoutJSON, _ := json.Marshal(input.CheckoutResources)
	dindJSON, _ := json.Marshal(input.DinDResources)
	agentJSON, _ := json.Marshal(input.AgentResources)
	settings := input.settings()
	settings.Revision = currentRevision + 1
	settings.UpdatedBy = actorID
	settings.UpdatedAt = now.UTC()
	if _, err = tx.Exec(ctx, `INSERT INTO builder_platform_settings_revisions
		(revision,node_isolation,max_concurrent_builders,checkout_resources,dind_resources,agent_resources,updated_by,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, settings.Revision, settings.NodeIsolation, settings.MaxConcurrentBuilders,
		checkoutJSON, dindJSON, agentJSON, actorID, settings.UpdatedAt); err != nil {
		return BuilderPlatformSettings{}, false, classifyPostgres(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO builder_platform_setting_mutations(actor_id,idempotency_key,request_fingerprint,revision,created_at) VALUES($1,$2,$3,$4,$5)`,
		actorID, idempotencyKey, fingerprint, settings.Revision, settings.UpdatedAt); err != nil {
		return BuilderPlatformSettings{}, false, classifyPostgres(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return BuilderPlatformSettings{}, false, classifyPostgres(err)
	}
	return settings, false, nil
}

type builderSettingsQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func builderPlatformSettingsByQuery(ctx context.Context, query builderSettingsQuery, statement string, args ...any) (BuilderPlatformSettings, error) {
	var settings BuilderPlatformSettings
	var checkoutJSON, dindJSON, agentJSON []byte
	err := query.QueryRow(ctx, statement, args...).Scan(&settings.Revision, &settings.NodeIsolation, &settings.MaxConcurrentBuilders,
		&checkoutJSON, &dindJSON, &agentJSON, &settings.UpdatedBy, &settings.UpdatedAt)
	if err != nil {
		return BuilderPlatformSettings{}, classifyPostgres(err)
	}
	if json.Unmarshal(checkoutJSON, &settings.CheckoutResources) != nil || json.Unmarshal(dindJSON, &settings.DinDResources) != nil ||
		json.Unmarshal(agentJSON, &settings.AgentResources) != nil || settings.Validate() != nil {
		return BuilderPlatformSettings{}, ErrInvalid
	}
	return settings, nil
}
