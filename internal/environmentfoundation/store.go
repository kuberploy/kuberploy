package environmentfoundation

import (
	"context"
	"time"
)

type Store interface {
	EnsureIntent(context.Context, EnsureRequest) (Intent, error)
	Intent(context.Context, string) (Intent, error)
	ClaimIntent(context.Context, string, string, string, time.Time, time.Duration) (Lease, bool, error)
	HeartbeatIntent(context.Context, Lease, time.Time, time.Duration) (Lease, error)
	BindWriteBase(context.Context, Lease, string, time.Time, time.Time) (Intent, error)
	RecordReady(context.Context, Lease, PublicationReceipt, time.Time) (Intent, error)
	RecordRetry(context.Context, Lease, string, bool, time.Time, time.Time) (Intent, error)
	RecordReadiness(context.Context, Readiness) error
	ExactReady(context.Context, string, string, int, time.Time) error
}

type PublisherIdentity struct {
	Contract, Policy, ConfigDigest string
}

func (i PublisherIdentity) Validate() error {
	if i.Contract != PublisherContract || i.Policy != ProtectedGitPolicy || !digestRE.MatchString(i.ConfigDigest) {
		return ErrInvalid
	}
	return nil
}

// ProtectedPublisher is intentionally the only mutation boundary exposed by
// this package. A future adapter must enforce this exact contract and policy;
// there is no generic Git client, path, YAML, or Kubernetes mutation escape.
type ProtectedPublisher interface {
	Identity() PublisherIdentity
	Publish(context.Context, Lease, PublicationRequest) (PublicationReceipt, error)
}

type PublicationRequest struct {
	IntentID, BindingID, ClusterID, EnvironmentID, TargetRef, PlannedHead string
	BindingGeneration                                                     int64
	Path, ContentDigest, IntentDigest, CommitTrailer                      string
	Content                                                               []byte
}

func publicationFor(intent Intent) PublicationRequest {
	return PublicationRequest{IntentID: intent.ID, BindingID: intent.Authority.BindingID, ClusterID: intent.Authority.ClusterID, EnvironmentID: intent.EnvironmentID,
		TargetRef: intent.Authority.TargetRef, PlannedHead: intent.Authority.PlannedHead, BindingGeneration: intent.Authority.Generation,
		Path: intent.Path, ContentDigest: intent.ManifestDigest, IntentDigest: intent.IntentDigest,
		CommitTrailer: intent.CommitTrailer, Content: append([]byte(nil), intent.Manifest...)}
}

func (r PublicationRequest) Validate(intent Intent, publisher PublisherIdentity) error {
	if intent.Validate() != nil || publisher.Validate() != nil || publisher.ConfigDigest != intent.PublisherConfigDigest ||
		r.IntentID != intent.ID || r.BindingID != intent.Authority.BindingID || r.ClusterID != intent.Authority.ClusterID || r.EnvironmentID != intent.EnvironmentID ||
		r.TargetRef != intent.Authority.TargetRef || r.PlannedHead != intent.Authority.PlannedHead ||
		r.BindingGeneration != intent.Authority.Generation || r.Path != intent.Path || r.ContentDigest != intent.ManifestDigest ||
		r.IntentDigest != intent.IntentDigest || r.CommitTrailer != intent.CommitTrailer || digest(r.Content) != intent.ManifestDigest {
		return ErrInvalid
	}
	return nil
}

type PublicationReceipt struct {
	IntentID, BindingID, TargetRef, Path, ContentDigest string
	ParentRevision, CommittedRevision, ProviderRequest  string
	ObservedAt                                          time.Time
}

func (r PublicationReceipt) Validate(intent Intent) error {
	if r.IntentID != intent.ID || r.BindingID != intent.Authority.BindingID || r.TargetRef != intent.Authority.TargetRef ||
		r.Path != intent.Path || r.ContentDigest != intent.ManifestDigest || r.ParentRevision != intent.WriteBaseRevision ||
		!gitCommitRE.MatchString(r.CommittedRevision) || r.CommittedRevision == r.ParentRevision ||
		!requestRE.MatchString(r.ProviderRequest) || r.ObservedAt.Before(intent.CreatedAt) {
		return ErrInvalid
	}
	return nil
}
