package helmdirect

import (
	"context"
	"time"
)

type ApplicationAPI interface {
	Apply(context.Context, string, string, []byte) error
	Delete(context.Context, string, string) error
	Observe(context.Context, string, string) (ApplicationState, error)
}

type ApplicationState struct {
	Exists        bool
	EnvironmentID string
	Sync          string
	Health        string
	Operation     string
	ConditionType string
	ReconciledAt  *time.Time
}

type ArgoReconciler struct {
	API       ApplicationAPI
	Namespace string
}

func (r ArgoReconciler) Reconcile(ctx context.Context, revision Revision) error {
	if r.API == nil || !dnsLabelRE.MatchString(r.Namespace) || revision.Validate() != nil {
		return ErrInvalid
	}
	name := ApplicationName(revision.Target.EnvironmentID, revision.Target.ApplicationID)
	legacyName := legacyApplicationName(revision.Target.ApplicationID)
	if !revision.DesiredEnabled {
		if err := r.API.Delete(ctx, r.Namespace, name); err != nil {
			return err
		}
		legacyOwned, err := r.deleteLegacyIfOwned(ctx, revision, legacyName)
		if err != nil {
			return err
		}
		current, err := r.API.Observe(ctx, r.Namespace, name)
		if err != nil {
			return err
		}
		legacyExists := false
		if legacyOwned {
			legacy, observeErr := r.API.Observe(ctx, r.Namespace, legacyName)
			if observeErr != nil {
				return observeErr
			}
			legacyExists = legacy.Exists
		}
		if current.Exists || legacyExists {
			return ErrPending
		}
		return nil
	}
	manifest, err := RenderApplication(revision, r.Namespace)
	if err != nil {
		return err
	}
	if err = r.API.Apply(ctx, r.Namespace, name, manifest); err != nil {
		return err
	}
	if _, err = r.deleteLegacyIfOwned(ctx, revision, legacyName); err != nil {
		return err
	}
	state, err := r.API.Observe(ctx, r.Namespace, name)
	if err != nil {
		return err
	}
	if !state.Exists || state.ReconciledAt == nil || state.ReconciledAt.Before(revision.UpdatedAt) {
		return ErrPending
	}
	if state.ConditionType != "" {
		return ReconcileFailure{Code: conditionFailureCode(state.ConditionType)}
	}
	if state.Operation == "Failed" || state.Operation == "Error" {
		return ReconcileFailure{Code: "argo-sync-failed"}
	}
	if state.Sync != "Synced" || state.Health != "Healthy" {
		return ErrPending
	}
	return nil
}

func (r ArgoReconciler) deleteLegacyIfOwned(ctx context.Context, revision Revision, legacyName string) (bool, error) {
	state, err := r.API.Observe(ctx, r.Namespace, legacyName)
	if err != nil {
		return false, err
	}
	if !state.Exists || state.EnvironmentID != revision.Target.EnvironmentID {
		return false, nil
	}
	if err = r.API.Delete(ctx, r.Namespace, legacyName); err != nil {
		return false, err
	}
	return true, nil
}

func conditionFailureCode(condition string) string {
	switch condition {
	case "InvalidSpecError":
		return "argo-invalid-spec"
	case "ComparisonError":
		return "argo-comparison-failed"
	case "SyncError":
		return "argo-sync-failed"
	default:
		return "argo-reconciliation-failed"
	}
}
