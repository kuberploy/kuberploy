package helmapps

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const protectedCascadeTable = "public.helm_application_cascade_preflights"

func terminalCascadeDeleteExistsSQL(preflightAlias string) string {
	return `EXISTS(SELECT 1 FROM public.helm_protected_application_intents terminal
		WHERE terminal.release_revision_id=` + preflightAlias + `.release_revision_id
		AND terminal.payload_intent_id=` + preflightAlias + `.payload_intent_id
		AND terminal.release_generation=` + preflightAlias + `.release_generation
		AND terminal.project_id=` + preflightAlias + `.project_id
		AND terminal.environment_id=` + preflightAlias + `.environment_id
		AND terminal.application_id=` + preflightAlias + `.application_id
		AND terminal.action='delete' AND terminal.state='verified'
		AND terminal.cascade_required AND terminal.cascade_receipt_id=` + preflightAlias + `.id
		AND terminal.cascade_contract='helm-application-cascade-preflight.v1')`
}

func (s *PostgresProtectedPublicationStore) ActivateCascadeObserver(ctx context.Context,
	owner string, workerEpoch int64, publisher ProtectedPublisherIdentity, now time.Time) (int64, error) {
	return retryProtectedTransaction(ctx, func() (int64, error) {
		return s.activateCascadeObserverOnce(ctx, owner, workerEpoch, publisher, now)
	})
}

func (s *PostgresProtectedPublicationStore) activateCascadeObserverOnce(ctx context.Context,
	owner string, workerEpoch int64, publisher ProtectedPublisherIdentity, now time.Time) (int64, error) {
	if s == nil || s.pool == nil || s.cascadeIdentity == nil || s.cascadeArgoObservation == nil ||
		ctx == nil || !workerIDRE.MatchString(owner) || workerEpoch < 1 ||
		publisher.Validate() != nil || now.IsZero() || s.cascadeArgoObservation.Validate() != nil {
		return 0, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return 0, classifyPostgres(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var dbNow time.Time
	if err = tx.QueryRow(ctx, `SELECT pg_catalog.clock_timestamp()`).Scan(&dbNow); err != nil ||
		now.UTC().Before(dbNow.Add(-30*time.Second)) || now.UTC().After(dbNow.Add(30*time.Second)) {
		if err != nil {
			return 0, classifyPostgres(err)
		}
		return 0, ErrConflict
	}
	observation := *s.cascadeArgoObservation
	identity := observation.DesiredStateRuntimeIdentity
	var argoWorkerEpoch int64
	err = tx.QueryRow(ctx, `SELECT worker_epoch FROM public.runtime_readiness
		WHERE runtime_kind='argo-desired-state' AND scope_key='global' AND worker_id=$1
		AND contract_version=$2 AND config_digest=$3 AND platform_binding_id=$4
		AND started_at=$5 AND observed_at<= $6 AND observed_at>= $6-interval '5 minutes'
		AND lease_until>$6 AND lease_until<=observed_at+interval '5 minutes'
		AND (identity->>'githubAppId')::bigint=$7 AND identity->>'clusterId'=$8
		AND identity->>'argoNamespace'=$9 AND identity->>'rootApplicationName'=$10
		AND identity->>'repositorySecretName'=$11 AND identity->>'chartRepository'=$12
		AND identity->>'chartName'=$13 AND identity->>'chartVersion'=$14
		AND identity->>'chartDigest'=$15 AND identity->>'rendererImage'=$16
		AND identity->>'chartDigestEnforcement'=$17`, observation.WorkerID,
		identity.ContractVersion, identity.ConfigDigest, identity.PlatformBindingID,
		observation.StartedAt.UTC(), dbNow, identity.GitHubAppID, identity.ClusterID,
		identity.ArgoNamespace, identity.RootApplicationName, identity.RepositorySecretName,
		identity.Runtime.ChartRepository, identity.Runtime.ChartName, identity.Runtime.ChartVersion,
		identity.Runtime.ChartDigest, identity.Runtime.RendererImage, identity.DigestEnforcement).
		Scan(&argoWorkerEpoch)
	if err != nil {
		return 0, classifyPostgres(err)
	}
	var activationEpoch int64
	err = tx.QueryRow(ctx, `SELECT public.activate_helm_application_cascade_observer(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, identity.PlatformBindingID,
		publisher.Contract, publisher.PolicyVersion, publisher.ConfigDigest, owner, workerEpoch,
		identity.ContractVersion, identity.ConfigDigest, observation.WorkerID, argoWorkerEpoch).
		Scan(&activationEpoch)
	if err != nil || activationEpoch < 1 {
		if err != nil {
			return 0, classifyPostgres(err)
		}
		return 0, ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, classifyPostgres(err)
	}
	return activationEpoch, nil
}

const protectedCascadeColumns = `id::text,delete_intent_id::text,release_revision_id::text,
	payload_intent_id::text,base_application_intent_id::text,release_generation,
	payload_revision,
	project_id::text,environment_id::text,application_id::text,platform_binding_id::text,
	environment_binding_id::text,cluster_id::text,platform_target_ref,environment_target_ref,
	environment_revision,environment_generation,catalog_digest,planned_base_revision,
	argo_namespace,application_path,source_content,source_content_digest,adopted_content,
	adopted_content_digest,operation,precondition,expected_etag,intent_digest,commit_trailer,
	contract,publisher_contract,publisher_config_digest,original_publisher_config_digest,
	publisher_adoption_epoch,state,next_attempt_at,attempts,consecutive_failures,last_failure_code,
	COALESCE(lease_owner,''),lease_epoch,lease_until,write_base_revision,write_base_observed_at,
	committed_revision,committed_parent_revision,committed_at,verified_at,verified_path_digest,
	provider_request,created_at,updated_at,completed_at`

func scanProtectedCascade(row rowScanner) (ProtectedApplicationCascadePreflight, error) {
	var value ProtectedApplicationCascadePreflight
	err := row.Scan(&value.ID, &value.DeleteIntentID, &value.ReleaseRevisionID,
		&value.PayloadIntentID, &value.BaseApplicationIntentID, &value.ReleaseGeneration,
		&value.PayloadRevision,
		&value.Target.ProjectID, &value.Target.EnvironmentID, &value.Target.ApplicationID,
		&value.Binding.PlatformBindingID, &value.Binding.EnvironmentBindingID,
		&value.Binding.ClusterID, &value.Binding.PlatformTargetRef,
		&value.Binding.EnvironmentTargetRef, &value.Binding.EnvironmentRevision,
		&value.Binding.EnvironmentGeneration, &value.Binding.CatalogDigest,
		&value.Binding.PlannedBaseRevision, &value.ArgoNamespace, &value.ApplicationPath,
		&value.SourceContent, &value.SourceContentDigest, &value.AdoptedContent,
		&value.AdoptedContentDigest, &value.Operation, &value.Precondition,
		&value.ExpectedETag, &value.IntentDigest, &value.CommitTrailer, &value.Contract,
		&value.Publisher.Contract, &value.Publisher.ConfigDigest,
		&value.OriginalPublisherConfigDigest, &value.PublisherAdoptionEpoch, &value.State,
		&value.NextAttemptAt, &value.Attempts, &value.ConsecutiveFailures,
		&value.LastFailureCode, &value.LeaseOwner, &value.LeaseEpoch, &value.LeaseUntil,
		&value.WriteBaseRevision, &value.WriteBaseObservedAt, &value.CommittedRevision,
		&value.CommittedParentRevision, &value.CommittedAt, &value.VerifiedAt,
		&value.VerifiedPathDigest, &value.ProviderRequest, &value.CreatedAt,
		&value.UpdatedAt, &value.CompletedAt)
	value.Publisher.PolicyVersion = ProtectedGitPolicy
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, classifyPostgres(err)
	}
	if value.Validate() != nil {
		return ProtectedApplicationCascadePreflight{}, ErrConflict
	}
	value.SourceContent = append([]byte(nil), value.SourceContent...)
	value.AdoptedContent = append([]byte(nil), value.AdoptedContent...)
	return value, nil
}

func (s *PostgresProtectedPublicationStore) CreateCascadePreflightForPayload(ctx context.Context,
	preflightID, deleteIntentID, payloadID string, runtime ProtectedApplicationRuntime,
	publisher ProtectedPublisherIdentity, now time.Time) (ProtectedApplicationCascadePreflight, bool, error) {
	type result struct {
		preflight ProtectedApplicationCascadePreflight
		replay    bool
	}
	value, err := retryProtectedTransaction(ctx, func() (result, error) {
		preflight, replay, createErr := s.createCascadePreflightForPayloadOnce(ctx,
			preflightID, deleteIntentID, payloadID, runtime, publisher, now)
		return result{preflight: preflight, replay: replay}, createErr
	})
	return value.preflight, value.replay, err
}

func (s *PostgresProtectedPublicationStore) createCascadePreflightForPayloadOnce(ctx context.Context,
	preflightID, deleteIntentID, payloadID string, runtime ProtectedApplicationRuntime,
	publisher ProtectedPublisherIdentity, now time.Time) (ProtectedApplicationCascadePreflight, bool, error) {
	if !uuidRE.MatchString(preflightID) || !uuidRE.MatchString(deleteIntentID) ||
		!uuidRE.MatchString(payloadID) || runtime.Validate() != nil || publisher.Validate() != nil || now.IsZero() {
		return ProtectedApplicationCascadePreflight{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if existing, getErr := scanProtectedCascade(tx.QueryRow(ctx, `SELECT `+protectedCascadeColumns+`
		FROM public.helm_application_cascade_preflights WHERE payload_intent_id=$1
		AND state<>'superseded' FOR KEY SHARE`, payloadID)); getErr == nil {
		if existing.DeleteIntentID != deleteIntentID || existing.ID != preflightID {
			return ProtectedApplicationCascadePreflight{}, false, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return ProtectedApplicationCascadePreflight{}, false, classifyPostgres(err)
		}
		return existing, true, nil
	} else if !errors.Is(getErr, pgx.ErrNoRows) && !errors.Is(getErr, ErrNotFound) {
		return ProtectedApplicationCascadePreflight{}, false, getErr
	}
	var replacementAllowed bool
	err = tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM public.helm_application_cascade_preflights prior
		WHERE prior.payload_intent_id=$1) OR EXISTS(
		  SELECT 1 FROM public.helm_application_cascade_preflights prior
		  WHERE prior.payload_intent_id=$1 AND prior.state='superseded'
		    AND prior.last_failure_code='cascade-projection-superseded'
		    AND prior.lease_epoch=0 AND prior.attempts=0 AND prior.lease_owner IS NULL
		    AND prior.lease_until IS NULL AND prior.write_base_revision=''
		    AND prior.write_base_observed_at IS NULL AND prior.committed_revision=''
		    AND prior.committed_parent_revision='' AND prior.committed_at IS NULL
		    AND prior.verified_at IS NULL AND prior.verified_path_digest=''
		    AND prior.provider_request='' AND prior.completed_at IS NOT NULL
		    AND NOT EXISTS(SELECT 1 FROM public.helm_application_cascade_preflights newer
		      WHERE newer.payload_intent_id=prior.payload_intent_id
		        AND (newer.created_at,newer.id)>(prior.created_at,prior.id)))`, payloadID).Scan(&replacementAllowed)
	if err != nil || !replacementAllowed {
		if err != nil {
			return ProtectedApplicationCascadePreflight{}, false, classifyPostgres(err)
		}
		return ProtectedApplicationCascadePreflight{}, false, ErrConflict
	}
	payload, err := scanProtectedPayload(tx.QueryRow(ctx, `SELECT `+protectedPayloadColumns+`
		FROM public.helm_protected_payload_intents WHERE id=$1 FOR KEY SHARE`, payloadID))
	if err != nil || payload.State != ProtectedVerified || payload.Action != ProtectedPayloadDisable {
		if err != nil {
			return ProtectedApplicationCascadePreflight{}, false, err
		}
		return ProtectedApplicationCascadePreflight{}, false, ErrConflict
	}
	release, err := scanReleaseRevision(tx.QueryRow(ctx, releaseRevisionSelect+` WHERE id=$1 FOR KEY SHARE`, payload.ReleaseRevisionID))
	if err != nil || release.Action != ReleaseDisable || release.DesiredEnabled ||
		release.BaseApplicationIntentID == "" || now.Before(payload.UpdatedAt) {
		if err != nil {
			return ProtectedApplicationCascadePreflight{}, false, classifyPostgres(err)
		}
		return ProtectedApplicationCascadePreflight{}, false, ErrConflict
	}
	base, err := scanProtectedApplication(tx.QueryRow(ctx, `SELECT `+protectedApplicationColumns+`
		FROM public.helm_protected_application_intents WHERE id=$1 FOR KEY SHARE`, release.BaseApplicationIntentID))
	if err != nil || base.State != ProtectedVerified || base.Action != ProtectedApplicationPublish {
		if err != nil {
			return ProtectedApplicationCascadePreflight{}, false, err
		}
		return ProtectedApplicationCascadePreflight{}, false, ErrConflict
	}
	var platformHead, platformTargetRef string
	err = tx.QueryRow(ctx, `SELECT target_head_revision,target_ref FROM public.git_repository_bindings
		WHERE id=$1 AND kind='platform' AND credential_mode='github-app'
		  AND cluster_id=$2 FOR KEY SHARE`, payload.Binding.PlatformBindingID,
		payload.Binding.ClusterID).Scan(&platformHead, &platformTargetRef)
	if err != nil || !gitCommitRE.MatchString(platformHead) {
		if err != nil {
			return ProtectedApplicationCascadePreflight{}, false, classifyPostgres(err)
		}
		return ProtectedApplicationCascadePreflight{}, false, ErrConflict
	}
	binding := payload.Binding
	binding.PlannedBaseRevision, binding.PlatformTargetRef = platformHead, platformTargetRef
	adopted, changed, err := adoptProtectedArgoResourcesFinalizer(base.Content)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, false, err
	}
	operation := "observe"
	if changed {
		operation = "update"
	}
	value := ProtectedApplicationCascadePreflight{ID: preflightID, DeleteIntentID: deleteIntentID,
		ReleaseRevisionID: release.ID, PayloadIntentID: payload.ID,
		BaseApplicationIntentID: base.ID, PayloadRevision: payload.CommittedRevision,
		ArgoNamespace: runtime.ArgoNamespace, ReleaseGeneration: release.Generation,
		Target: release.Target, Binding: binding, ApplicationPath: base.ApplicationPath,
		SourceContent: append([]byte(nil), base.Content...), SourceContentDigest: base.ContentDigest,
		AdoptedContent: adopted, AdoptedContentDigest: digestBytes(adopted), Operation: operation,
		Precondition: "match-etag", ExpectedETag: `"` + base.ContentDigest + `"`,
		CommitTrailer: "Kuberploy-Helm-Cascade-Preflight: " + preflightID,
		Contract:      protectedCascadeContract, Publisher: publisher,
		OriginalPublisherConfigDigest: publisher.ConfigDigest, State: ProtectedPending,
		NextAttemptAt: now.UTC(), CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	value.IntentDigest, err = cascadePreflightIntentDigest(value)
	if err != nil || value.Validate() != nil {
		return ProtectedApplicationCascadePreflight{}, false, ErrInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.helm_application_cascade_preflights(
		id,delete_intent_id,release_revision_id,payload_intent_id,base_application_intent_id,
		release_generation,payload_revision,project_id,environment_id,application_id,platform_binding_id,
		environment_binding_id,cluster_id,platform_target_ref,environment_target_ref,
		environment_revision,environment_generation,catalog_digest,planned_base_revision,
		argo_namespace,application_path,source_content,source_content_digest,adopted_content,
		adopted_content_digest,content_digest,operation,precondition,expected_etag,intent_digest,
		commit_trailer,contract,publisher_contract,publisher_policy_version,publisher_config_digest,
		original_publisher_config_digest,publisher_adoption_epoch,prerequisite_epoch,state,
		next_attempt_at,attempts,consecutive_failures,last_failure_code,lease_epoch,
		write_base_revision,committed_revision,committed_parent_revision,verified_path_digest,
		provider_request,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,
		$21,$22,$23,$24,$25,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,0,0,'pending',$36,
		0,0,'',0,'','','','','',$36,$36)`, value.ID, value.DeleteIntentID,
		value.ReleaseRevisionID, value.PayloadIntentID, value.BaseApplicationIntentID,
		value.ReleaseGeneration, value.PayloadRevision, value.Target.ProjectID, value.Target.EnvironmentID,
		value.Target.ApplicationID, value.Binding.PlatformBindingID,
		value.Binding.EnvironmentBindingID, value.Binding.ClusterID,
		value.Binding.PlatformTargetRef, value.Binding.EnvironmentTargetRef,
		value.Binding.EnvironmentRevision, value.Binding.EnvironmentGeneration,
		value.Binding.CatalogDigest, value.Binding.PlannedBaseRevision, value.ArgoNamespace,
		value.ApplicationPath, value.SourceContent, value.SourceContentDigest,
		value.AdoptedContent, value.AdoptedContentDigest, value.Operation, value.Precondition,
		value.ExpectedETag, value.IntentDigest, value.CommitTrailer, value.Contract,
		value.Publisher.Contract, value.Publisher.PolicyVersion, value.Publisher.ConfigDigest,
		value.OriginalPublisherConfigDigest, value.CreatedAt)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, false, classifyPostgres(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ProtectedApplicationCascadePreflight{}, false, classifyPostgres(err)
	}
	return value, false, nil
}

func (s *PostgresProtectedPublicationStore) CascadePreflight(ctx context.Context, preflightID string) (ProtectedApplicationCascadePreflight, error) {
	if !uuidRE.MatchString(preflightID) {
		return ProtectedApplicationCascadePreflight{}, ErrInvalid
	}
	return scanProtectedCascade(s.pool.QueryRow(ctx, `SELECT `+protectedCascadeColumns+`
		FROM public.helm_application_cascade_preflights WHERE id=$1`, preflightID))
}

func (s *PostgresProtectedPublicationStore) ClaimCascadePreflight(ctx context.Context, owner string,
	publisher ProtectedPublisherIdentity, now time.Time, duration time.Duration) (ProtectedApplicationCascadePreflight, ProtectedIntentLease, error) {
	id, err := s.claimProtected(ctx, protectedCascadeTable, owner, publisher, now, duration)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedIntentLease{}, err
	}
	value, err := s.CascadePreflight(ctx, id)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedIntentLease{}, err
	}
	lease := cascadePreflightLease(value)
	return value, lease, lease.Validate()
}

func (s *PostgresProtectedPublicationStore) AdoptCascadePreflight(ctx context.Context, owner string,
	workerEpoch int64, publisher ProtectedPublisherIdentity, duration time.Duration) (ProtectedApplicationCascadePreflight, ProtectedIntentLease, error) {
	id, err := s.adoptProtected(ctx, protectedCascadeTable, owner, workerEpoch, publisher, duration)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedIntentLease{}, err
	}
	value, err := s.CascadePreflight(ctx, id)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedIntentLease{}, err
	}
	lease := cascadePreflightLease(value)
	return value, lease, lease.Validate()
}

func cascadePreflightLease(value ProtectedApplicationCascadePreflight) ProtectedIntentLease {
	lease := ProtectedIntentLease{IntentID: value.ID, Owner: value.LeaseOwner,
		Epoch: value.LeaseEpoch, Publisher: value.Publisher}
	if value.LeaseUntil != nil {
		lease.Until = *value.LeaseUntil
	}
	return lease
}

func (s *PostgresProtectedPublicationStore) HeartbeatCascadePreflight(ctx context.Context, lease ProtectedIntentLease,
	now time.Time, duration time.Duration) (ProtectedIntentLease, error) {
	if err := s.heartbeatProtected(ctx, protectedCascadeTable, lease, now, duration); err != nil {
		return ProtectedIntentLease{}, err
	}
	value, err := s.CascadePreflight(ctx, lease.IntentID)
	if err != nil {
		return ProtectedIntentLease{}, err
	}
	return cascadePreflightLease(value), nil
}

func (s *PostgresProtectedPublicationStore) BindCascadePreflightWriteBase(ctx context.Context, lease ProtectedIntentLease,
	revision string, observedAt, now time.Time) (ProtectedApplicationCascadePreflight, error) {
	if err := s.bindProtectedWriteBase(ctx, protectedCascadeTable, lease, revision, observedAt, now); err != nil {
		return ProtectedApplicationCascadePreflight{}, err
	}
	return s.CascadePreflight(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) RebindCascadePreflightWriteBase(ctx context.Context, lease ProtectedIntentLease,
	previous, revision string, observedAt, now time.Time) (ProtectedApplicationCascadePreflight, error) {
	if err := s.rebindProtectedWriteBase(ctx, protectedCascadeTable, lease, previous, revision, observedAt, now); err != nil {
		return ProtectedApplicationCascadePreflight{}, err
	}
	return s.CascadePreflight(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) MarkCascadePreflightCommitted(ctx context.Context, lease ProtectedIntentLease,
	revision, parent string, now time.Time) (ProtectedApplicationCascadePreflight, error) {
	if err := s.markProtectedCommitted(ctx, protectedCascadeTable, lease, revision, parent, now); err != nil {
		return ProtectedApplicationCascadePreflight{}, err
	}
	return s.CascadePreflight(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) VerifyCascadePreflight(ctx context.Context, lease ProtectedIntentLease,
	revision, pathDigest, providerRequest string, now time.Time) (ProtectedApplicationCascadePreflight, error) {
	current, err := s.CascadePreflight(ctx, lease.IntentID)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, err
	}
	if current.Operation == "observe" {
		if lease.Validate() != nil || !gitCommitRE.MatchString(revision) ||
			pathDigest != current.AdoptedContentDigest || len(providerRequest) < 1 ||
			len(providerRequest) > 256 || containsControl(providerRequest) || now.IsZero() {
			return ProtectedApplicationCascadePreflight{}, ErrInvalid
		}
		result, updateErr := s.pool.Exec(ctx, `UPDATE public.helm_application_cascade_preflights SET
			state='verified',verified_at=$8,verified_path_digest=$7,provider_request=$6,
			completed_at=$8,lease_owner=NULL,lease_until=NULL,updated_at=$8,prerequisite_epoch=prerequisite_epoch+1
			WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND publisher_contract=$4
			AND publisher_config_digest=$5 AND lease_until>$8 AND state='claimed'
			AND write_base_revision=$9 AND write_base_observed_at IS NOT NULL
			AND content_digest=$7`, lease.IntentID, lease.Owner, lease.Epoch,
			lease.Publisher.Contract, lease.Publisher.ConfigDigest, providerRequest,
			pathDigest, now.UTC(), revision)
		if updateErr != nil {
			return ProtectedApplicationCascadePreflight{}, classifyPostgres(updateErr)
		}
		if result.RowsAffected() != 1 {
			return ProtectedApplicationCascadePreflight{}, ErrConflict
		}
		return s.CascadePreflight(ctx, lease.IntentID)
	}
	if err = s.verifyProtected(ctx, protectedCascadeTable, lease, revision, pathDigest, providerRequest, now); err != nil {
		return ProtectedApplicationCascadePreflight{}, err
	}
	return s.CascadePreflight(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) RetryCascadePreflight(ctx context.Context, lease ProtectedIntentLease,
	code string, nextAttemptAt, now time.Time) (ProtectedApplicationCascadePreflight, error) {
	if err := s.retryProtected(ctx, protectedCascadeTable, lease, code, nextAttemptAt, now); err != nil {
		return ProtectedApplicationCascadePreflight{}, err
	}
	return s.CascadePreflight(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) FailCascadePreflight(ctx context.Context, lease ProtectedIntentLease,
	code string, now time.Time) (ProtectedApplicationCascadePreflight, error) {
	if err := s.failProtected(ctx, protectedCascadeTable, lease, code, now); err != nil {
		return ProtectedApplicationCascadePreflight{}, err
	}
	return s.CascadePreflight(ctx, lease.IntentID)
}

func (s *PostgresProtectedPublicationStore) FailCascadePreflightPathAbsent(ctx context.Context,
	lease ProtectedIntentLease, proof ProtectedCascadePathAbsenceProof,
	now time.Time) (ProtectedApplicationCascadePreflight, error) {
	return retryProtectedTransaction(ctx, func() (ProtectedApplicationCascadePreflight, error) {
		return s.failCascadePreflightPathAbsentOnce(ctx, lease, proof, now)
	})
}

func (s *PostgresProtectedPublicationStore) failCascadePreflightPathAbsentOnce(ctx context.Context,
	lease ProtectedIntentLease, proof ProtectedCascadePathAbsenceProof,
	now time.Time) (ProtectedApplicationCascadePreflight, error) {
	if s == nil || s.pool == nil || ctx == nil || lease.Validate() != nil ||
		proof.Validate() != nil || now.IsZero() {
		return ProtectedApplicationCascadePreflight{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, classifyPostgres(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var dbNow time.Time
	if err = tx.QueryRow(ctx, `SELECT pg_catalog.clock_timestamp()`).Scan(&dbNow); err != nil {
		return ProtectedApplicationCascadePreflight{}, classifyPostgres(err)
	}
	if now.UTC().Before(dbNow.Add(-30*time.Second)) || now.UTC().After(dbNow.Add(30*time.Second)) ||
		proof.ProviderObservedAt.After(dbNow) || proof.ProviderObservedAt.Before(dbNow.Add(-5*time.Minute)) {
		return ProtectedApplicationCascadePreflight{}, ErrConflict
	}
	var recordedAt time.Time
	err = tx.QueryRow(ctx, `INSERT INTO public.helm_application_cascade_absence_receipts(
		cascade_preflight_id,provider_head,provider_request,provider_observed_at,
		operation_commit_absent)
		VALUES($1,$2,$3,$4,$5)
		RETURNING recorded_at`, lease.IntentID, proof.ProviderHead, proof.ProviderRequest,
		proof.ProviderObservedAt.UTC(), proof.OperationCommitAbsent).Scan(&recordedAt)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, classifyPostgres(err)
	}
	result, err := tx.Exec(ctx, `UPDATE public.helm_application_cascade_preflights SET
		state='failed',consecutive_failures=LEAST(consecutive_failures+1,30),
		last_failure_code='cascade-path-absent-recovery-required',
		lease_owner=NULL,lease_until=NULL,completed_at=$6,updated_at=$6,
		prerequisite_epoch=prerequisite_epoch+1
		WHERE id=$1 AND state='claimed' AND operation='update'
		  AND lease_owner=$2 AND lease_epoch=$3 AND publisher_contract=$4
		  AND publisher_config_digest=$5 AND lease_until>$6
		  AND committed_revision='' AND committed_parent_revision=''
		  AND committed_at IS NULL AND verified_at IS NULL
		  AND verified_path_digest='' AND provider_request=''`, lease.IntentID,
		lease.Owner, lease.Epoch, lease.Publisher.Contract, lease.Publisher.ConfigDigest,
		recordedAt)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ProtectedApplicationCascadePreflight{}, ErrConflict
	}
	value, err := scanProtectedCascade(tx.QueryRow(ctx, `SELECT `+protectedCascadeColumns+`
		FROM public.helm_application_cascade_preflights WHERE id=$1`, lease.IntentID))
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ProtectedApplicationCascadePreflight{}, classifyPostgres(err)
	}
	return value, nil
}

func (s *PostgresProtectedPublicationStore) ClaimCascadeObservation(ctx context.Context, owner string,
	workerEpoch int64, publisher ProtectedPublisherIdentity, now time.Time,
	duration time.Duration) (ProtectedApplicationCascadePreflight, ProtectedCascadeObservationLease, error) {
	type result struct {
		preflight ProtectedApplicationCascadePreflight
		lease     ProtectedCascadeObservationLease
	}
	value, err := retryProtectedTransaction(ctx, func() (result, error) {
		preflight, lease, claimErr := s.claimCascadeObservationOnce(ctx, owner, workerEpoch,
			publisher, now, duration)
		return result{preflight: preflight, lease: lease}, claimErr
	})
	return value.preflight, value.lease, err
}

func (s *PostgresProtectedPublicationStore) claimCascadeObservationOnce(ctx context.Context, owner string,
	workerEpoch int64, publisher ProtectedPublisherIdentity, now time.Time,
	duration time.Duration) (ProtectedApplicationCascadePreflight, ProtectedCascadeObservationLease, error) {
	if s == nil || s.pool == nil || s.cascadeIdentity == nil || ctx == nil ||
		!workerIDRE.MatchString(owner) || workerEpoch < 1 || publisher.Validate() != nil ||
		now.IsZero() || !validProtectedLeaseDuration(duration) {
		return ProtectedApplicationCascadePreflight{}, ProtectedCascadeObservationLease{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedCascadeObservationLease{}, classifyPostgres(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var dbNow time.Time
	if err = tx.QueryRow(ctx, `SELECT pg_catalog.clock_timestamp()`).Scan(&dbNow); err != nil ||
		now.UTC().Before(dbNow.Add(-30*time.Second)) || now.UTC().After(dbNow.Add(30*time.Second)) {
		if err != nil {
			return ProtectedApplicationCascadePreflight{}, ProtectedCascadeObservationLease{}, classifyPostgres(err)
		}
		return ProtectedApplicationCascadePreflight{}, ProtectedCascadeObservationLease{}, ErrConflict
	}
	identity := *s.cascadeIdentity
	observation := *s.cascadeArgoObservation
	var activationEpoch int64
	err = tx.QueryRow(ctx, `SELECT activation.activation_epoch
		FROM public.helm_application_cascade_observer_activations activation
		WHERE activation.platform_binding_id=$1 AND activation.publisher_worker_id=$2
		AND activation.publisher_worker_epoch=$3 AND activation.publisher_contract=$4
		AND activation.publisher_policy_version=$5 AND activation.publisher_config_digest=$6
		AND activation.argo_worker_id=$7 AND activation.argo_contract=$8
		AND activation.argo_config_digest=$9 AND activation.activation_epoch=(
		  SELECT MAX(current.activation_epoch)
		  FROM public.helm_application_cascade_observer_activations current
		  WHERE current.platform_binding_id=$1)
		AND public.helm_application_cascade_active_observer_is_exact(
		  activation.platform_binding_id,activation.publisher_worker_id,
		  activation.publisher_worker_epoch,activation.publisher_contract,
		  activation.publisher_policy_version,activation.publisher_config_digest,
		  activation.argo_worker_id,activation.argo_worker_epoch,
		  activation.argo_contract,activation.argo_config_digest,$10)`, identity.PlatformBindingID,
		owner, workerEpoch, publisher.Contract, publisher.PolicyVersion, publisher.ConfigDigest,
		observation.WorkerID, identity.ContractVersion, identity.ConfigDigest, dbNow).Scan(&activationEpoch)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedCascadeObservationLease{}, classifyPostgres(err)
	}
	if activationEpoch < 1 {
		return ProtectedApplicationCascadePreflight{}, ProtectedCascadeObservationLease{}, ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.helm_application_cascade_observation_jobs(
		cascade_preflight_id,platform_binding_id,activation_epoch,
		publisher_contract,publisher_policy_version,publisher_config_digest,
		state,next_attempt_at,attempts,consecutive_failures,last_failure_code,lease_epoch,created_at,updated_at)
		SELECT preflight.id,preflight.platform_binding_id,$4,$1,$2,$3,'pending',$5,0,0,'',0,$5,$5
		FROM public.helm_application_cascade_preflights preflight
		JOIN public.helm_release_heads head ON head.environment_id=preflight.environment_id
		 AND head.application_id=preflight.application_id AND head.revision_id=preflight.release_revision_id
		 AND head.generation=preflight.release_generation
		JOIN public.git_repository_bindings binding ON binding.id=preflight.platform_binding_id
		 AND binding.kind='platform' AND binding.credential_mode='github-app'
		 AND binding.cluster_id=preflight.cluster_id AND binding.target_ref=preflight.platform_target_ref
		WHERE preflight.state='verified' AND preflight.platform_binding_id=$6
		  AND public.helm_application_cascade_preflight_is_fresh(preflight.id)
		  AND NOT `+terminalCascadeDeleteExistsSQL("preflight")+`
		ON CONFLICT(cascade_preflight_id,activation_epoch) DO NOTHING`, publisher.Contract,
		publisher.PolicyVersion, publisher.ConfigDigest, activationEpoch, dbNow, identity.PlatformBindingID)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedCascadeObservationLease{}, classifyPostgres(err)
	}
	_, err = tx.Exec(ctx, `UPDATE public.helm_application_cascade_observation_jobs job SET
		state='pending',next_attempt_at=$3,attempts=0,consecutive_failures=0,last_failure_code='',
		completed_at=NULL,updated_at=$3 FROM public.helm_application_cascade_preflights preflight
		WHERE preflight.id=job.cascade_preflight_id
		AND job.activation_epoch=$1 AND job.publisher_config_digest=$2
		AND job.state='verified'
		AND public.helm_application_cascade_preflight_is_fresh(job.cascade_preflight_id)
		AND NOT public.helm_application_cascade_observation_is_exact(job.cascade_preflight_id,$2,$3)
		AND NOT `+terminalCascadeDeleteExistsSQL("preflight"),
		activationEpoch, publisher.ConfigDigest, dbNow)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedCascadeObservationLease{}, classifyPostgres(err)
	}
	var preflightID string
	lease := ProtectedCascadeObservationLease{Owner: owner, WorkerEpoch: workerEpoch, Publisher: publisher}
	err = tx.QueryRow(ctx, `SELECT job.cascade_preflight_id::text,job.lease_epoch+1,job.attempts+1
		FROM public.helm_application_cascade_observation_jobs job
		JOIN public.helm_application_cascade_preflights preflight ON preflight.id=job.cascade_preflight_id
		JOIN public.helm_release_heads head ON head.environment_id=preflight.environment_id
		 AND head.application_id=preflight.application_id AND head.revision_id=preflight.release_revision_id
		 AND head.generation=preflight.release_generation
		WHERE job.activation_epoch=$5 AND job.publisher_contract=$1
		 AND job.publisher_policy_version=$2 AND job.publisher_config_digest=$3
		 AND job.attempts<30 AND job.next_attempt_at<=$4
		 AND public.helm_application_cascade_preflight_is_fresh(preflight.id)
		 AND NOT `+terminalCascadeDeleteExistsSQL("preflight")+`
		 AND (job.state='pending' OR (job.state='claimed' AND job.lease_until<=$4))
		 AND NOT public.helm_application_cascade_observation_is_exact(preflight.id,$3,$4)
		ORDER BY job.next_attempt_at,job.created_at,job.cascade_preflight_id
		FOR UPDATE OF job SKIP LOCKED LIMIT 1`, publisher.Contract, publisher.PolicyVersion,
		publisher.ConfigDigest, dbNow, activationEpoch).Scan(&preflightID, &lease.Epoch, &lease.Attempts)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedCascadeObservationLease{}, classifyPostgres(err)
	}
	lease.CascadePreflightID, lease.Until = preflightID, dbNow.UTC().Add(duration)
	err = tx.QueryRow(ctx, `UPDATE public.helm_application_cascade_observation_jobs SET
		state='claimed',attempts=$6,lease_owner=$2,worker_epoch=$3,lease_epoch=$4,lease_until=$5,
		updated_at=$7 WHERE cascade_preflight_id=$1 AND activation_epoch=$8
		RETURNING activation_epoch`, preflightID,
		owner, workerEpoch, lease.Epoch, lease.Until, lease.Attempts, dbNow, activationEpoch).
		Scan(&lease.ObserverActivationEpoch)
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedCascadeObservationLease{}, classifyPostgres(err)
	}
	if lease.Validate() != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedCascadeObservationLease{}, ErrConflict
	}
	preflight, err := scanProtectedCascade(tx.QueryRow(ctx, `SELECT `+protectedCascadeColumns+`
		FROM public.helm_application_cascade_preflights WHERE id=$1 FOR KEY SHARE`, preflightID))
	if err != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedCascadeObservationLease{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ProtectedApplicationCascadePreflight{}, ProtectedCascadeObservationLease{}, classifyPostgres(err)
	}
	return preflight, lease, nil
}

func (s *PostgresProtectedPublicationStore) RecordCascadeObservation(ctx context.Context,
	lease ProtectedCascadeObservationLease, receipt ProtectedApplicationCascadeReceipt,
	now time.Time) (ProtectedApplicationCascadeReceipt, error) {
	return retryProtectedTransaction(ctx, func() (ProtectedApplicationCascadeReceipt, error) {
		return s.recordCascadeObservationOnce(ctx, lease, receipt, now)
	})
}

func (s *PostgresProtectedPublicationStore) recordCascadeObservationOnce(ctx context.Context,
	lease ProtectedCascadeObservationLease, receipt ProtectedApplicationCascadeReceipt,
	now time.Time) (ProtectedApplicationCascadeReceipt, error) {
	if s == nil || s.pool == nil || s.cascadeIdentity == nil || ctx == nil || lease.Validate() != nil ||
		now.IsZero() || receipt.validateProposal(now.UTC()) != nil || receipt.CascadePreflightID != lease.CascadePreflightID ||
		receipt.ObservationLeaseEpoch != lease.Epoch || receipt.Publisher != lease.Publisher ||
		receipt.ObserverActivationEpoch != lease.ObserverActivationEpoch ||
		receipt.ArgoContract != s.cascadeIdentity.ContractVersion ||
		receipt.ArgoConfigDigest != s.cascadeIdentity.ConfigDigest ||
		receipt.WorkerID != lease.Owner || receipt.WorkerEpoch != lease.WorkerEpoch {
		return ProtectedApplicationCascadeReceipt{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, classifyPostgres(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	preflight, err := scanProtectedCascade(tx.QueryRow(ctx, `SELECT `+protectedCascadeColumns+`
		FROM public.helm_application_cascade_preflights WHERE id=$1 FOR UPDATE`, receipt.CascadePreflightID))
	if err != nil || preflight.State != ProtectedVerified || preflight.DeleteIntentID != receipt.DeleteIntentID ||
		preflight.ReleaseRevisionID != receipt.ReleaseRevisionID || preflight.PayloadIntentID != receipt.PayloadIntentID ||
		preflight.BaseApplicationIntentID != receipt.BaseApplicationIntentID || preflight.Target.ProjectID != receipt.ProjectID ||
		preflight.Target.EnvironmentID != receipt.EnvironmentID || preflight.Target.ApplicationID != receipt.ApplicationID ||
		preflight.Binding.ClusterID != receipt.ClusterID || preflight.ApplicationPath != receipt.ApplicationPath ||
		preflight.SourceContentDigest != receipt.SourceContentDigest || preflight.AdoptedContentDigest != receipt.AdoptedContentDigest {
		if err != nil {
			return ProtectedApplicationCascadeReceipt{}, err
		}
		return ProtectedApplicationCascadeReceipt{}, ErrConflict
	}
	expectedAdoptionRevision, expectedAdoptionParent := preflight.WriteBaseRevision, preflight.WriteBaseRevision
	if preflight.Operation == "update" {
		expectedAdoptionRevision, expectedAdoptionParent = preflight.CommittedRevision, preflight.CommittedParentRevision
	}
	if receipt.AdoptionRevision != expectedAdoptionRevision || receipt.AdoptionParentRevision != expectedAdoptionParent {
		return ProtectedApplicationCascadeReceipt{}, ErrConflict
	}
	expectedChild, err := preflight.ApplicationExpectation()
	if err != nil || receipt.ChildReleaseRevisionID != expectedChild.ReleaseRevisionID ||
		receipt.ChildPayloadRevision != expectedChild.TargetRevision || receipt.ChildPayloadPath != expectedChild.PayloadPath ||
		receipt.ChildPayloadDigest != expectedChild.PayloadDigest || receipt.ChildSpecDigest != expectedChild.SpecDigest ||
		receipt.FinalizerDigest != expectedChild.FinalizerDigest {
		return ProtectedApplicationCascadeReceipt{}, ErrConflict
	}
	var dbNow time.Time
	err = tx.QueryRow(ctx, `SELECT pg_catalog.clock_timestamp()`).Scan(&dbNow)
	if err != nil || now.UTC().Before(dbNow.Add(-30*time.Second)) || now.UTC().After(dbNow.Add(30*time.Second)) {
		if err != nil {
			return ProtectedApplicationCascadeReceipt{}, classifyPostgres(err)
		}
		return ProtectedApplicationCascadeReceipt{}, ErrConflict
	}
	var platformHead string
	err = tx.QueryRow(ctx, `SELECT target_head_revision FROM public.git_repository_bindings
		WHERE id=$1 AND kind='platform' AND credential_mode='github-app'
		  AND cluster_id=$2 AND target_ref=$3 FOR UPDATE`, preflight.Binding.PlatformBindingID,
		preflight.Binding.ClusterID, preflight.Binding.PlatformTargetRef).Scan(&platformHead)
	if err != nil || platformHead != receipt.ProviderHead || receipt.RootObservedRevision != platformHead {
		if err != nil {
			return ProtectedApplicationCascadeReceipt{}, classifyPostgres(err)
		}
		return ProtectedApplicationCascadeReceipt{}, ErrConflict
	}
	binding, err := scanCascadePlatformBinding(tx.QueryRow(ctx, `SELECT id,kind,scope_id::text,
		COALESCE(project_id::text,''),COALESCE(environment_id::text,''),COALESCE(cluster_id::text,''),
		provider,installation_id,repository_id,repository_owner,repository_name,target_ref,path_prefix,
		credential_mode,credential_secret_name,state,target_head_revision,indexed_revision,
		projection_generation,parser_version,target_head_observed_at,indexed_at,created_at,updated_at
		FROM public.git_repository_bindings WHERE id=$1 FOR KEY SHARE`, preflight.Binding.PlatformBindingID))
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, err
	}
	head := gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository,
		TargetRef: binding.TargetRef, Commit: platformHead, Source: gitprojection.ObservationWrite,
		ProviderRequest: "cascade-db-authority", ObservedAt: dbNow.UTC()}
	expectedRoot, err := argo.NewPlatformRootApplicationExpectation(*s.cascadeIdentity, binding, head)
	if err != nil || receipt.RootSpecDigest != expectedRoot.SpecDigest || receipt.RootSyncStatus != "Synced" {
		return ProtectedApplicationCascadeReceipt{}, ErrConflict
	}
	var jobState string
	err = tx.QueryRow(ctx, `SELECT state FROM public.helm_application_cascade_observation_jobs
		WHERE cascade_preflight_id=$1 AND activation_epoch=$2 AND publisher_config_digest=$3
		AND lease_owner=$4 AND worker_epoch=$5 AND lease_epoch=$6 AND lease_until>$7 FOR UPDATE`,
		preflight.ID, lease.ObserverActivationEpoch, lease.Publisher.ConfigDigest,
		lease.Owner, lease.WorkerEpoch, lease.Epoch, dbNow).Scan(&jobState)
	if err != nil || jobState != "claimed" {
		if err != nil {
			return ProtectedApplicationCascadeReceipt{}, classifyPostgres(err)
		}
		return ProtectedApplicationCascadeReceipt{}, ErrLeaseLost
	}
	err = tx.QueryRow(ctx, `INSERT INTO public.helm_application_cascade_receipts(
		id,delete_intent_id,cascade_preflight_id,observation_epoch,observation_lease_epoch,release_revision_id,
		payload_intent_id,base_application_intent_id,project_id,environment_id,application_id,
		cluster_id,application_path,source_content_digest,adopted_content_digest,
		adoption_revision,adoption_parent_revision,provider_head,root_observed_revision,
		root_uid,root_resource_version,root_spec_digest,root_sync_status,child_uid,child_resource_version,
		child_spec_digest,finalizer_digest,child_release_revision_id,child_payload_revision,
		child_payload_path,child_payload_digest,publisher_contract,publisher_policy_version,
		publisher_config_digest,worker_id,worker_epoch,observer_activation_epoch,observed_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,
		$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38)
		RETURNING observation_epoch,observed_at,observer_activation_epoch,
		argo_contract,argo_config_digest,argo_worker_id,argo_worker_epoch,
		argo_started_at,argo_readiness_observed_at,argo_readiness_lease_until`, receipt.ID,
		receipt.DeleteIntentID, receipt.CascadePreflightID, receipt.ObservationEpoch, receipt.ObservationLeaseEpoch,
		receipt.ReleaseRevisionID, receipt.PayloadIntentID, receipt.BaseApplicationIntentID,
		receipt.ProjectID, receipt.EnvironmentID, receipt.ApplicationID, receipt.ClusterID,
		receipt.ApplicationPath, receipt.SourceContentDigest, receipt.AdoptedContentDigest,
		receipt.AdoptionRevision, receipt.AdoptionParentRevision, receipt.ProviderHead,
		receipt.RootObservedRevision, receipt.RootUID, receipt.RootResourceVersion,
		receipt.RootSpecDigest, receipt.RootSyncStatus, receipt.ChildUID, receipt.ChildResourceVersion,
		receipt.ChildSpecDigest, receipt.FinalizerDigest, receipt.ChildReleaseRevisionID,
		receipt.ChildPayloadRevision, receipt.ChildPayloadPath, receipt.ChildPayloadDigest,
		receipt.Publisher.Contract, receipt.Publisher.PolicyVersion, receipt.Publisher.ConfigDigest,
		receipt.WorkerID, receipt.WorkerEpoch, receipt.ObserverActivationEpoch, receipt.ObservedAt).Scan(&receipt.ObservationEpoch,
		&receipt.ObservedAt, &receipt.ObserverActivationEpoch, &receipt.ArgoContract,
		&receipt.ArgoConfigDigest, &receipt.ArgoWorkerID, &receipt.ArgoWorkerEpoch,
		&receipt.ArgoStartedAt, &receipt.ArgoReadinessObservedAt, &receipt.ArgoReadinessLeaseUntil)
	if err != nil {
		return ProtectedApplicationCascadeReceipt{}, classifyPostgres(err)
	}
	if receipt.Validate(receipt.ObservedAt) != nil || receipt.ArgoContract != s.cascadeIdentity.ContractVersion ||
		receipt.ArgoConfigDigest != s.cascadeIdentity.ConfigDigest {
		return ProtectedApplicationCascadeReceipt{}, ErrConflict
	}
	result, err := tx.Exec(ctx, `UPDATE public.helm_application_cascade_observation_jobs SET
		state='verified',consecutive_failures=0,last_failure_code='',lease_owner=NULL,worker_epoch=NULL,
		lease_until=NULL,completed_at=$7,updated_at=$7 WHERE cascade_preflight_id=$1
		AND activation_epoch=$2 AND publisher_config_digest=$3 AND lease_owner=$4
		AND worker_epoch=$5 AND lease_epoch=$6 AND state='claimed'`, preflight.ID,
		lease.ObserverActivationEpoch, lease.Publisher.ConfigDigest, lease.Owner,
		lease.WorkerEpoch, lease.Epoch, receipt.ObservedAt)
	if err != nil || result.RowsAffected() != 1 {
		if err != nil {
			return ProtectedApplicationCascadeReceipt{}, classifyPostgres(err)
		}
		return ProtectedApplicationCascadeReceipt{}, ErrLeaseLost
	}
	if err = tx.Commit(ctx); err != nil {
		return ProtectedApplicationCascadeReceipt{}, classifyPostgres(err)
	}
	return receipt, nil
}

func (s *PostgresProtectedPublicationStore) RetryCascadeObservation(ctx context.Context,
	lease ProtectedCascadeObservationLease, code string, nextAttemptAt, now time.Time) error {
	if s == nil || s.pool == nil || ctx == nil || lease.Validate() != nil || !failureCodeRE.MatchString(code) ||
		now.IsZero() || nextAttemptAt.Before(now) || nextAttemptAt.After(now.Add(maximumProtectedRetry)) {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `UPDATE public.helm_application_cascade_observation_jobs SET
		state=CASE WHEN attempts>=30 THEN 'failed' ELSE 'pending' END,
		consecutive_failures=consecutive_failures+1,last_failure_code=$7,
		next_attempt_at=CASE WHEN attempts>=30 THEN next_attempt_at ELSE $8 END,
		lease_owner=NULL,worker_epoch=NULL,lease_until=NULL,
		completed_at=CASE WHEN attempts>=30 THEN $9 ELSE NULL END,updated_at=$9
		WHERE cascade_preflight_id=$1 AND activation_epoch=$2 AND publisher_config_digest=$3
		AND lease_owner=$4 AND worker_epoch=$5 AND lease_epoch=$6 AND lease_until>$9
		AND state='claimed'`, lease.CascadePreflightID, lease.ObserverActivationEpoch,
		lease.Publisher.ConfigDigest, lease.Owner, lease.WorkerEpoch, lease.Epoch,
		code, nextAttemptAt.UTC(), now.UTC())
	if err != nil {
		return classifyPostgres(err)
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}
