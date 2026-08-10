package middlewareprofiles

import "context"

type Store interface {
	Create(context.Context, Command, string, Spec, []Assignment) (MutationResult, error)
	Revise(context.Context, Command, Ref, Spec, []Assignment) (MutationResult, error)
	Clone(context.Context, Command, Ref, string, []Assignment) (MutationResult, error)
	Deactivate(context.Context, Command, Ref) (MutationResult, error)
	Current(context.Context, string) (Profile, Revision, error)
	Revision(context.Context, Ref) (Profile, Revision, error)
	Catalog(context.Context, int) ([]Entry, error)
	Assigned(context.Context, Target, int) ([]Entry, error)
	References(context.Context, string, int) ([]Reference, error)
}

// ReferenceWriter is implemented by projection stores. Replacing one Git
// document's references is atomic with activation so deactivation can never
// race an attached profile.
type ReferenceWriter interface {
	ReplaceReferences(context.Context, string, string, string, []Reference) error
}

type Resolver struct{ store Store }

func NewResolver(store Store) (*Resolver, error) {
	if store == nil {
		return nil, ErrInvalid
	}
	return &Resolver{store}, nil
}
func (r *Resolver) Resolve(ctx context.Context, ref Ref, target Target) (Resolution, error) {
	if validateRef(ref) != nil || validateTarget(target) != nil {
		return Resolution{}, ErrInvalid
	}
	profile, revision, err := r.store.Revision(ctx, ref)
	if err != nil {
		return Resolution{}, err
	}
	if profile.Lifecycle != Active {
		return Resolution{}, ErrInactive
	}
	if profile.CurrentRevision != ref.Revision || ref.SpecDigest != revision.SpecDigest || ref.AssignmentsDigest != revision.AssignmentsDigest {
		return Resolution{}, ErrConflict
	}
	for _, assignment := range revision.Assignments {
		if assigned(assignment, target) {
			return Resolution{Ref: Ref{profile.ID, revision.Revision, revision.SpecDigest, revision.AssignmentsDigest}, Name: profile.Name, Spec: cloneSpec(revision.Spec)}, nil
		}
	}
	return Resolution{}, ErrUnassigned
}
