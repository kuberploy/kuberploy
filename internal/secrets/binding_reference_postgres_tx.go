package secrets

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/id"
)

// ValidateBindingReferencePlanTx repeats metadata resolution while holding the
// exact binding/version rows in the caller's PostgreSQL transaction. It is the
// authoritative counterpart to the read-only UI/direct-Git resolver.
func ValidateBindingReferencePlanTx(ctx context.Context, tx pgx.Tx, plan BindingReferencePlan) error {
	if tx == nil || plan.Validate() != nil {
		return ErrInvalid
	}
	for _, use := range plan.Uses {
		var organizationID, projectID, environmentID, applicationID, namespace string
		var bindingName, bindingProvider, bindingPurpose, bindingState string
		var activeVersion int64
		var versionID, versionProvider, versionTargetType, versionState string
		var objectName, targetName, manifestDigest, sealingFingerprint, ciphertextDigest sql.NullString
		var deliveryCount int
		err := tx.QueryRow(ctx, `SELECT
			b.organization_id::text,b.project_id::text,b.environment_id::text,b.application_id::text,b.target_namespace,
			b.name,b.provider,b.purpose,b.state,b.active_version,
			v.id::text,v.provider,v.target_secret_type,v.state,v.provider_object_name,v.target_secret_name,v.manifest_digest,v.sealed_key_fingerprint,v.ciphertext_digest,
			(SELECT count(*) FROM secret_binding_deliveries d
			 WHERE d.binding_id=b.id AND d.version_id=v.id AND d.source_key=$3 AND d.kind='environment' AND d.environment_name=$4)
			FROM secret_bindings b
			JOIN secret_binding_versions v ON v.binding_id=b.id AND v.version_number=$2
			WHERE b.id=$1
			FOR UPDATE OF b,v`, use.BindingID, use.Version, use.Key, use.Delivery.EnvironmentName).Scan(
			&organizationID, &projectID, &environmentID, &applicationID, &namespace,
			&bindingName, &bindingProvider, &bindingPurpose, &bindingState, &activeVersion,
			&versionID, &versionProvider, &versionTargetType, &versionState, &objectName, &targetName, &manifestDigest, &sealingFingerprint, &ciphertextDigest,
			&deliveryCount)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return classifyPostgres(err)
		}
		expectedTarget := TargetSecretName(Binding{ID: use.BindingID, Name: use.Name}, use.Version)
		if organizationID != plan.Scope.OrganizationID || projectID != plan.Scope.ProjectID || environmentID != plan.Scope.EnvironmentID ||
			applicationID != plan.Scope.ApplicationID || namespace != plan.Scope.Namespace || bindingName != use.Name {
			return ErrNotFound
		}
		if bindingProvider != string(ProviderSealedSecrets) || bindingPurpose != string(PurposeRuntimeSecret) ||
			versionProvider != string(ProviderSealedSecrets) || versionTargetType != string(TargetSecretOpaque) ||
			bindingState != string(BindingReady) || activeVersion != use.Version || versionID != use.VersionID || versionState != string(VersionActive) ||
			!objectName.Valid || objectName.String != expectedTarget || !targetName.Valid || targetName.String != expectedTarget ||
			!manifestDigest.Valid || !digestRE.MatchString(manifestDigest.String) || !sealingFingerprint.Valid || !digestRE.MatchString(sealingFingerprint.String) ||
			!ciphertextDigest.Valid || !digestRE.MatchString(ciphertextDigest.String) || deliveryCount != 1 {
			return ErrNotReady
		}
	}
	return nil
}

// ReplaceGitCurrentReferencesTx validates the exact plan and reconciles only
// Git-current deletion guards in the caller's transaction. Current-release and
// retained-release rows are never selected or modified.
func ReplaceGitCurrentReferencesTx(ctx context.Context, tx pgx.Tx, plan BindingReferencePlan, actorID, referenceID, revision, requestID string, now time.Time) error {
	if (actorID != "" && !uuidRE.MatchString(actorID)) || !safeOpaque(referenceID, 256) || !revisionRE.MatchString(revision) ||
		!requestIDRE.MatchString(requestID) || now.IsZero() {
		return ErrInvalid
	}
	if err := ValidateBindingReferencePlanTx(ctx, tx, plan); err != nil {
		return err
	}
	type storedReference struct {
		BindingID string
		VersionID string
		Revision  string
	}
	rows, err := tx.Query(ctx, `SELECT binding_id::text,version_id::text,revision
		FROM secret_binding_references
		WHERE kind='git-current' AND reference_id=$1
		ORDER BY binding_id
		FOR UPDATE`, referenceID)
	if err != nil {
		return classifyPostgres(err)
	}
	existing := []storedReference{}
	for rows.Next() {
		var item storedReference
		if err = rows.Scan(&item.BindingID, &item.VersionID, &item.Revision); err != nil {
			rows.Close()
			return classifyPostgres(err)
		}
		existing = append(existing, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return classifyPostgres(err)
	}
	rows.Close()

	desired := plan.BindingVersions()
	desiredByBinding := make(map[string]ReferenceIdentity, len(desired))
	for _, item := range desired {
		desiredByBinding[item.BindingID] = item
	}
	kept := map[string]struct{}{}
	for _, current := range existing {
		want, found := desiredByBinding[current.BindingID]
		if found && want.VersionID == current.VersionID && current.Revision == revision {
			kept[current.BindingID] = struct{}{}
			continue
		}
		command, deleteErr := tx.Exec(ctx, `DELETE FROM secret_binding_references
			WHERE binding_id=$1 AND kind='git-current' AND reference_id=$2 AND version_id=$3`,
			current.BindingID, referenceID, current.VersionID)
		if deleteErr != nil {
			return classifyPostgres(deleteErr)
		}
		if command.RowsAffected() != 1 {
			return ErrConflict
		}
		if err = insertEvent(ctx, tx, Event{ID: id.New(), BindingID: current.BindingID, VersionID: current.VersionID,
			ActorID: actorID, Kind: EventReferenceRemoved, RequestID: requestID, OccurredAt: now.UTC()}); err != nil {
			return err
		}
	}
	slices.SortFunc(desired, func(left, right ReferenceIdentity) int { return compareStrings(left.BindingID, right.BindingID) })
	for _, item := range desired {
		if _, exists := kept[item.BindingID]; exists {
			continue
		}
		if _, err = tx.Exec(ctx, `INSERT INTO secret_binding_references(binding_id,version_id,kind,reference_id,revision,created_at)
			VALUES($1,$2,'git-current',$3,$4,$5)`, item.BindingID, item.VersionID, referenceID, revision, now.UTC()); err != nil {
			return classifyPostgres(err)
		}
		if err = insertEvent(ctx, tx, Event{ID: id.New(), BindingID: item.BindingID, VersionID: item.VersionID,
			ActorID: actorID, Kind: EventReferenceAdded, RequestID: requestID, OccurredAt: now.UTC()}); err != nil {
			return err
		}
	}
	return nil
}
