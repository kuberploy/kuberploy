package certificates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/secrets"
)

// ReconcileGitCurrentCertificateReferencesTx resolves active certificate uses
// and replaces only TLS-certificate Git-current deletion guards in the same
// transaction as the caller's accepted AppConfig state.
func (r *PostgreSQLReferenceResolver) ReconcileGitCurrentCertificateReferencesTx(
	ctx context.Context, tx pgx.Tx, scope secrets.Scope, selections []ReferenceSelection,
	expectedDigest, referenceID, revision string, now time.Time,
) error {
	if r == nil || tx == nil || scope.Validate() != nil || referenceID == "" || revision == "" || now.IsZero() {
		return ErrInvalid
	}
	plan := ReferencePlan{Scope: scope, Uses: []ResolvedSelection{}}
	if len(selections) != 0 {
		resolved, err := r.ResolveCertificateReferencesTx(ctx, tx, scope, selections, now.UTC())
		if err != nil {
			return err
		}
		digest, err := resolved.Digest()
		// API preview/save supplies the immutable preview digest. Direct-Git
		// indexing has no preview authority: the exact accepted Git bytes are
		// authoritative, so an empty expected digest means resolve and guard
		// those current bytes inside this transaction.
		if err != nil || expectedDigest != "" && digest != expectedDigest {
			return ErrConflict
		}
		plan = resolved
	} else if expectedDigest != "" {
		return ErrConflict
	}
	type currentReference struct{ bindingID, versionID, revision string }
	rows, err := tx.Query(ctx, `SELECT r.binding_id::text,r.version_id::text,r.revision,
		COALESCE(b.organization_id::text,''),b.project_id::text,b.environment_id::text,b.application_id::text,b.target_namespace
		FROM secret_binding_references r JOIN secret_bindings b ON b.id=r.binding_id
		WHERE r.kind='git-current' AND r.reference_id=$1 AND b.purpose='tls-certificate'
		ORDER BY r.binding_id FOR UPDATE OF r,b`, referenceID)
	if err != nil {
		return ErrUnavailable
	}
	existing := []currentReference{}
	for rows.Next() {
		var current currentReference
		var organizationID, projectID, environmentID, applicationID, namespace string
		if err = rows.Scan(&current.bindingID, &current.versionID, &current.revision, &organizationID,
			&projectID, &environmentID, &applicationID, &namespace); err != nil {
			rows.Close()
			return ErrUnavailable
		}
		if organizationID != scope.OrganizationID || projectID != scope.ProjectID || environmentID != scope.EnvironmentID ||
			applicationID != scope.ApplicationID || namespace != scope.Namespace {
			rows.Close()
			return ErrConflict
		}
		existing = append(existing, current)
	}
	if rows.Err() != nil {
		rows.Close()
		return ErrUnavailable
	}
	rows.Close()
	desired := map[string]string{}
	for _, use := range plan.Uses {
		if version, found := desired[use.Resolved.BindingID]; found && version != use.Resolved.SecretVersionID {
			return ErrConflict
		}
		desired[use.Resolved.BindingID] = use.Resolved.SecretVersionID
	}
	for _, current := range existing {
		versionID, found := desired[current.bindingID]
		if found && versionID == current.versionID && current.revision == revision {
			delete(desired, current.bindingID)
			continue
		}
		result, deleteErr := tx.Exec(ctx, `DELETE FROM secret_binding_references
			WHERE binding_id=$1 AND version_id=$2 AND kind='git-current' AND reference_id=$3`, current.bindingID, current.versionID, referenceID)
		if deleteErr != nil || result.RowsAffected() != 1 {
			return ErrConflict
		}
		if err = insertCertificateReferenceEventTx(ctx, tx, current.bindingID, current.versionID,
			secrets.EventReferenceRemoved, certificateReferenceRequestID(referenceID, revision), now.UTC()); err != nil {
			return err
		}
	}
	ids := sortedCertificateBindingIDs(desired)
	for _, bindingID := range ids {
		result, insertErr := tx.Exec(ctx, `INSERT INTO secret_binding_references(binding_id,version_id,kind,reference_id,revision,created_at)
			SELECT b.id,v.id,'git-current',$3,$4,$5 FROM secret_bindings b JOIN secret_binding_versions v ON v.binding_id=b.id
			WHERE b.id=$1 AND v.id=$2 AND b.purpose='tls-certificate' AND b.state='ready'
			AND b.active_version=v.version_number AND v.state='active'`, bindingID, desired[bindingID], referenceID, revision, now.UTC())
		if insertErr != nil || result.RowsAffected() != 1 {
			return ErrConflict
		}
		if err = insertCertificateReferenceEventTx(ctx, tx, bindingID, desired[bindingID],
			secrets.EventReferenceAdded, certificateReferenceRequestID(referenceID, revision), now.UTC()); err != nil {
			return err
		}
	}
	return nil
}

func insertCertificateReferenceEventTx(
	ctx context.Context, tx pgx.Tx, bindingID, versionID string, kind secrets.EventKind, requestID string, now time.Time,
) error {
	if tx == nil || bindingID == "" || versionID == "" || requestID == "" || now.IsZero() ||
		(kind != secrets.EventReferenceAdded && kind != secrets.EventReferenceRemoved) {
		return ErrInvalid
	}
	result, err := tx.Exec(ctx, `INSERT INTO secret_binding_events(id,binding_id,version_id,actor_id,kind,request_id,occurred_at)
		VALUES($1,$2,$3,NULL,$4,$5,$6)`, id.New(), bindingID, versionID, kind, requestID, now.UTC())
	if err != nil || result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func certificateReferenceRequestID(referenceID, revision string) string {
	digest := sha256.Sum256([]byte(referenceID + "\x00" + revision))
	return "certificate-git:" + hex.EncodeToString(digest[:16])
}

func sortedCertificateBindingIDs(desired map[string]string) []string {
	ids := make([]string, 0, len(desired))
	for id := range desired {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
