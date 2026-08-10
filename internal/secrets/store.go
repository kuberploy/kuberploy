package secrets

import (
	"context"
	"time"
)

type Idempotency struct {
	ActorID            string
	Operation          string
	ApplicationID      string
	Key                string
	RequestFingerprint [32]byte
	BindingID          string
	VersionID          string
	CreatedAt          time.Time
}

func (i Idempotency) validate() error {
	if !uuidRE.MatchString(i.ActorID) || (i.Operation != "create" && i.Operation != "rotate") ||
		!uuidRE.MatchString(i.ApplicationID) || !idempotencyRE.MatchString(i.Key) ||
		!uuidRE.MatchString(i.BindingID) || !uuidRE.MatchString(i.VersionID) || i.CreatedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

type BeginCreate struct {
	Binding     Binding
	Version     Version
	Idempotency Idempotency
	Event       Event
}

type BeginRotation struct {
	BindingID             string
	ExpectedActiveVersion int64
	Version               Version
	Idempotency           Idempotency
	Event                 Event
}

// Store mutations are atomic with their safe outbox event. Begin operations
// return the existing immutable version on a matching idempotency replay and
// ErrConflict when the key was bound to different input.
type Store interface {
	BeginCreate(context.Context, BeginCreate) (Binding, Version, bool, error)
	BeginRotation(context.Context, BeginRotation) (Binding, Version, bool, error)
	CompleteStage(context.Context, string, Artifact, Event, time.Time) (Version, error)
	FailVersion(context.Context, string, string, Event, time.Time) (Version, error)
	ActivateVersion(context.Context, string, time.Time, Event) (Binding, Version, error)

	Binding(context.Context, string) (Binding, error)
	ListBindings(context.Context, string, string) ([]Binding, error)
	Version(context.Context, string) (Version, error)
	Versions(context.Context, string) ([]Version, error)

	AddReference(context.Context, Reference, Event) error
	RemoveReference(context.Context, string, ReferenceKind, string, Event) error
	References(context.Context, string) ([]Reference, error)

	PrepareDelete(context.Context, string, string, Event, time.Time) (Binding, []Version, error)
	CompleteDelete(context.Context, string, Event, time.Time) (Binding, error)

	PendingEvents(context.Context, int) ([]Event, error)
	MarkEventPublished(context.Context, string, time.Time) error
}
