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
	_, err := validateBindingReferencePlanTx(ctx, tx, plan, false)
	return err
}

func validateBindingReferencePlanTx(ctx context.Context, tx pgx.Tx, plan BindingReferencePlan, allowRetained bool) (map[string]bool, error) {
	if tx == nil || plan.Validate() != nil {
		return nil, ErrInvalid
	}
	retained := make(map[string]bool, len(plan.Uses))
	for _, use := range plan.Uses {
		var organizationID, projectID, environmentID, applicationID, namespace string
		var bindingName, bindingProvider, bindingPurpose, bindingState string
		var activeVersion int64
		var versionID, versionProvider, versionTargetType, versionState string
		var objectName, targetName, manifestDigest, sealingFingerprint, ciphertextDigest sql.NullString
		var deliveryCount int
		err := tx.QueryRow(ctx, `SELECT
			COALESCE(b.organization_id::text,''),b.project_id::text,b.environment_id::text,b.application_id::text,b.target_namespace,
			b.name,b.provider,b.purpose,b.state,b.active_version,
			v.id::text,v.provider,v.target_secret_type,v.state,v.provider_object_name,v.target_secret_name,v.manifest_digest,v.sealed_key_fingerprint,v.ciphertext_digest,
			(SELECT count(*) FROM secret_binding_deliveries d
			 WHERE d.binding_id=b.id AND d.version_id=v.id AND d.source_key=$3 AND d.kind=$4
			 AND (($4='environment' AND d.environment_name=$5 AND d.file_path IS NULL AND d.file_mode IS NULL)
			   OR ($4='file' AND d.environment_name IS NULL AND d.file_path=$6 AND d.file_mode=$7)))
			FROM secret_bindings b
			JOIN secret_binding_versions v ON v.binding_id=b.id AND v.version_number=$2
			WHERE b.id=$1
			FOR UPDATE OF b,v`, use.BindingID, use.Version, use.Key, string(use.Delivery.Kind), use.Delivery.EnvironmentName,
			use.Delivery.FilePath, int(use.Delivery.FileMode)).Scan(
			&organizationID, &projectID, &environmentID, &applicationID, &namespace,
			&bindingName, &bindingProvider, &bindingPurpose, &bindingState, &activeVersion,
			&versionID, &versionProvider, &versionTargetType, &versionState, &objectName, &targetName, &manifestDigest, &sealingFingerprint, &ciphertextDigest,
			&deliveryCount)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, classifyPostgres(err)
		}
		expectedTarget := TargetSecretName(Binding{ID: use.BindingID, Name: use.Name}, use.Version)
		if organizationID != plan.Scope.OrganizationID || projectID != plan.Scope.ProjectID || environmentID != plan.Scope.EnvironmentID ||
			applicationID != plan.Scope.ApplicationID || namespace != plan.Scope.Namespace || bindingName != use.Name {
			return nil, ErrNotFound
		}
		versionReady := versionState == string(VersionActive) && activeVersion == use.Version ||
			allowRetained && versionState == string(VersionRetained) && activeVersion > use.Version
		if bindingProvider != string(ProviderSealedSecrets) || bindingPurpose != string(PurposeRuntimeSecret) ||
			versionProvider != string(ProviderSealedSecrets) || versionTargetType != string(TargetSecretOpaque) ||
			bindingState != string(BindingReady) || !versionReady || versionID != use.VersionID ||
			!objectName.Valid || objectName.String != expectedTarget || !targetName.Valid || targetName.String != expectedTarget ||
			!manifestDigest.Valid || !digestRE.MatchString(manifestDigest.String) || !sealingFingerprint.Valid || !digestRE.MatchString(sealingFingerprint.String) ||
			!ciphertextDigest.Valid || !digestRE.MatchString(ciphertextDigest.String) || deliveryCount != 1 {
			return nil, ErrNotReady
		}
		retained[use.BindingID] = versionState == string(VersionRetained)
	}
	return retained, nil
}

// ReplaceGitCurrentReferencesTx validates the exact plan and reconciles only
// Git-current deletion guards in the caller's transaction. Current-release and
// retained-release rows are never selected or modified.
func ReplaceGitCurrentReferencesTx(ctx context.Context, tx pgx.Tx, plan BindingReferencePlan, actorID, referenceID, revision, requestID string, now time.Time) error {
	return replaceGitCurrentReferencesTx(ctx, tx, plan, actorID, referenceID, revision, requestID, now, false, "", "")
}

// ReplaceIndexedGitCurrentReferencesTx is reserved for an exact AppConfig
// already observed in Git. It preserves deletion guards for an immutable
// retained version only when the same path, binding, version and byte-identical
// AppConfig were already indexed while active. An unrelated descendant commit
// can therefore preserve a running reference, but changed bytes cannot restore
// an old version. All API-authored preview/save plans remain active-only.
func ReplaceIndexedGitCurrentReferencesTx(ctx context.Context, tx pgx.Tx, plan BindingReferencePlan, gitBindingID, referenceID, revision, contentSHA256, requestID string, now time.Time) error {
	return replaceGitCurrentReferencesTx(ctx, tx, plan, "", referenceID, revision, requestID, now, true, gitBindingID, contentSHA256)
}

func replaceGitCurrentReferencesTx(ctx context.Context, tx pgx.Tx, plan BindingReferencePlan, actorID, referenceID, revision, requestID string, now time.Time, allowRetained bool, gitBindingID, contentSHA256 string) error {
	if (actorID != "" && !uuidRE.MatchString(actorID)) || !safeOpaque(referenceID, 256) || !revisionRE.MatchString(revision) ||
		!requestIDRE.MatchString(requestID) || now.IsZero() || allowRetained && (!uuidRE.MatchString(gitBindingID) || !digestRE.MatchString(contentSHA256)) {
		return ErrInvalid
	}
	retained, err := validateBindingReferencePlanTx(ctx, tx, plan, allowRetained)
	if err != nil {
		return err
	}
	type storedReference struct {
		BindingID string
		VersionID string
		Revision  string
		CreatedAt time.Time
	}
	rows, err := tx.Query(ctx, `SELECT binding_id::text,version_id::text,revision,created_at
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
		if err = rows.Scan(&item.BindingID, &item.VersionID, &item.Revision, &item.CreatedAt); err != nil {
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
	existingByBinding := make(map[string]storedReference, len(existing))
	for _, current := range existing {
		existingByBinding[current.BindingID] = current
	}
	// A retained version is continuity only when the exact previous reference
	// was created by a valid projected document with byte-identical content.
	// This admits unrelated descendant commits without allowing changed Git
	// bytes to introduce or restore an old version after rotation.
	for _, item := range desired {
		if !retained[item.BindingID] {
			continue
		}
		current, found := existingByBinding[item.BindingID]
		if !found || current.VersionID != item.VersionID {
			return ErrNotReady
		}
		var matched bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM git_projected_documents d
			WHERE d.binding_id=$1 AND d.path=$2 AND d.source_revision=$3
			AND d.content_sha256=$4 AND d.indexed_at=$5 AND d.valid)`,
			gitBindingID, referenceID, current.Revision, contentSHA256, current.CreatedAt).Scan(&matched); err != nil {
			return classifyPostgres(err)
		}
		if !matched {
			return ErrNotReady
		}
	}
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
