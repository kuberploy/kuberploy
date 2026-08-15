package helmapps

import (
	"context"
	"time"
)

type PublicationCandidateKind string

const (
	PublicationPayload     PublicationCandidateKind = "payload"
	PublicationCascade     PublicationCandidateKind = "cascade"
	PublicationApplication PublicationCandidateKind = "application"
)

// PublicationCandidate contains only durable identifiers. In particular it
// never carries a binding snapshot supplied by an API caller.
type PublicationCandidate struct {
	Kind              PublicationCandidateKind
	ReleaseRevisionID string
	PayloadIntentID   string
	ReservedIntentID  string
	Target            ReleaseTarget
}

func (c PublicationCandidate) Validate() error {
	if !uuidRE.MatchString(c.ReleaseRevisionID) || c.Target.Validate() != nil {
		return ErrInvalid
	}
	switch c.Kind {
	case PublicationPayload:
		if c.PayloadIntentID != "" || c.ReservedIntentID != "" {
			return ErrInvalid
		}
	case PublicationCascade:
		if !uuidRE.MatchString(c.PayloadIntentID) || c.ReservedIntentID != "" {
			return ErrInvalid
		}
	case PublicationApplication:
		if !uuidRE.MatchString(c.PayloadIntentID) {
			return ErrInvalid
		}
		if c.ReservedIntentID != "" && !uuidRE.MatchString(c.ReservedIntentID) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type ProtectedPublicationPlanningStore interface {
	ProtectedPublicationStore
	ProtectedCascadeStore
	NextPayloadCandidate(context.Context) (PublicationCandidate, error)
	NextCascadeCandidate(context.Context) (PublicationCandidate, error)
	NextApplicationCandidate(context.Context, ProtectedPublisherIdentity) (PublicationCandidate, error)
}

// ProtectedBindingResolver is a trusted production projection boundary. Its
// implementation must derive the snapshot from the active, provider-observed
// platform/environment bindings. API payloads must never implement this seam.
type ProtectedBindingResolver interface {
	ResolveProtectedBinding(context.Context, ReleaseTarget) (ProtectedBindingSnapshot, error)
}

type PublicationPlanResult struct {
	Kind     PublicationCandidateKind
	IntentID string
	Replay   bool
}

type PublicationPlanner struct {
	Store       ProtectedPublicationPlanningStore
	Bindings    ProtectedBindingResolver
	Publisher   ProtectedPublisherIdentity
	Application ProtectedApplicationRuntime
	NewID       func() string
	Now         func() time.Time
}

func (p PublicationPlanner) Validate() error {
	if p.Store == nil || p.Bindings == nil || p.Publisher.Validate() != nil ||
		p.Application.Validate() != nil || p.NewID == nil || p.Now == nil {
		return ErrInvalid
	}
	return nil
}

// ProcessOne plans at most one durable intent. Verified payloads are promoted
// before new payload work so a completed phase one cannot be stranded behind
// a large render queue. The protected store independently rechecks that the
// selected release is still the current head in the same serializable write.
func (p PublicationPlanner) ProcessOne(ctx context.Context) (PublicationPlanResult, error) {
	if p.Validate() != nil || ctx == nil {
		return PublicationPlanResult{}, ErrInvalid
	}
	now := p.Now().UTC()
	if now.IsZero() {
		return PublicationPlanResult{}, ErrInvalid
	}
	candidate, err := p.Store.NextApplicationCandidate(ctx, p.Publisher)
	if err == nil {
		if candidate.Validate() != nil || candidate.Kind != PublicationApplication {
			return PublicationPlanResult{}, ErrConflict
		}
		intentID := candidate.ReservedIntentID
		if intentID == "" {
			intentID = p.NewID()
		}
		intent, replay, createErr := p.Store.CreateApplicationForPayload(ctx, intentID,
			candidate.PayloadIntentID, p.Application, p.Publisher, now)
		if createErr != nil {
			return PublicationPlanResult{}, createErr
		}
		if intent.ID == "" || intent.ReleaseRevisionID != candidate.ReleaseRevisionID || intent.Target != candidate.Target {
			return PublicationPlanResult{}, ErrConflict
		}
		return PublicationPlanResult{Kind: PublicationApplication, IntentID: intent.ID, Replay: replay}, nil
	}
	if err != nil && err != ErrNotFound {
		return PublicationPlanResult{}, err
	}
	candidate, err = p.Store.NextCascadeCandidate(ctx)
	if err == nil {
		if candidate.Validate() != nil || candidate.Kind != PublicationCascade {
			return PublicationPlanResult{}, ErrConflict
		}
		preflightID, deleteIntentID := p.NewID(), p.NewID()
		preflight, replay, createErr := p.Store.CreateCascadePreflightForPayload(ctx,
			preflightID, deleteIntentID, candidate.PayloadIntentID, p.Application, p.Publisher, now)
		if createErr != nil {
			return PublicationPlanResult{}, createErr
		}
		if preflight.ID == "" || preflight.ReleaseRevisionID != candidate.ReleaseRevisionID ||
			preflight.Target != candidate.Target {
			return PublicationPlanResult{}, ErrConflict
		}
		return PublicationPlanResult{Kind: PublicationCascade, IntentID: preflight.ID, Replay: replay}, nil
	}
	if err != nil && err != ErrNotFound {
		return PublicationPlanResult{}, err
	}
	candidate, err = p.Store.NextPayloadCandidate(ctx)
	if err != nil {
		return PublicationPlanResult{}, err
	}
	if candidate.Validate() != nil || candidate.Kind != PublicationPayload {
		return PublicationPlanResult{}, ErrConflict
	}
	binding, err := p.Bindings.ResolveProtectedBinding(ctx, candidate.Target)
	if err != nil {
		return PublicationPlanResult{}, err
	}
	if binding.Validate() != nil {
		return PublicationPlanResult{}, ErrConflict
	}
	intentID := p.NewID()
	intent, replay, err := p.Store.CreatePayloadForHead(ctx, intentID, candidate.Target,
		binding, p.Publisher, now)
	if err != nil {
		return PublicationPlanResult{}, err
	}
	if intent.ID == "" || intent.ReleaseRevisionID != candidate.ReleaseRevisionID || intent.Target != candidate.Target ||
		intent.Binding != binding {
		return PublicationPlanResult{}, ErrConflict
	}
	return PublicationPlanResult{Kind: PublicationPayload, IntentID: intent.ID, Replay: replay}, nil
}
