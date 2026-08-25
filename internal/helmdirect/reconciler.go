package helmdirect

import "context"

type ApplicationAPI interface {
	Apply(context.Context, string, string, []byte) error
	Delete(context.Context, string, string) error
}

type ArgoReconciler struct {
	API       ApplicationAPI
	Namespace string
}

func (r ArgoReconciler) Reconcile(ctx context.Context, revision Revision) error {
	if r.API == nil || !dnsLabelRE.MatchString(r.Namespace) || revision.Validate() != nil {
		return ErrInvalid
	}
	name := ApplicationName(revision.Target.ApplicationID)
	if !revision.DesiredEnabled {
		return r.API.Delete(ctx, r.Namespace, name)
	}
	manifest, err := RenderApplication(revision, r.Namespace)
	if err != nil {
		return err
	}
	return r.API.Apply(ctx, r.Namespace, name, manifest)
}
