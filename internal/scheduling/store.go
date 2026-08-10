package scheduling

import "context"

type Store interface {
	Create(context.Context, Command, string, Spec, []Assignment) (MutationResult, error)
	Revise(context.Context, Command, Ref, Spec, []Assignment) (MutationResult, error)
	Deactivate(context.Context, Command, Ref) (MutationResult, error)
	Current(context.Context, string) (Profile, Revision, error)
	Revision(context.Context, Ref) (Profile, Revision, error)
	Catalog(context.Context, int) ([]Entry, error)
	Assigned(context.Context, Target, int) ([]Entry, error)
}
type Resolver struct{ store Store }

func NewResolver(s Store) (*Resolver, error) {
	if s == nil {
		return nil, ErrInvalid
	}
	return &Resolver{s}, nil
}
func (r *Resolver) Resolve(ctx context.Context, ref Ref, target Target) (PodScheduling, error) {
	resolved, err := r.ResolveExact(ctx, ref, target)
	return resolved.Pod, err
}

func (r *Resolver) ResolveExact(ctx context.Context, ref Ref, target Target) (Resolution, error) {
	if !uuidRE.MatchString(ref.ProfileID) || ref.Revision < 1 || validateTarget(target) != nil {
		return Resolution{}, ErrInvalid
	}
	p, v, e := r.store.Revision(ctx, ref)
	if e != nil {
		return Resolution{}, e
	}
	if p.Lifecycle != Active {
		return Resolution{}, ErrInactive
	}
	if p.CurrentRevision != ref.Revision {
		return Resolution{}, ErrConflict
	}
	for _, a := range v.Assignments {
		if match(a, target) {
			return Resolution{Ref: ref, SpecDigest: v.SpecDigest, AssignmentsDigest: v.AssignmentsDigest, Pod: clonePod(v.Spec.Pod)}, nil
		}
	}
	return Resolution{}, ErrUnassigned
}
