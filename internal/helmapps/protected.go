package helmapps

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"go.yaml.in/yaml/v3"
)

const (
	ProtectedPublisherContract    = "helm-protected-publisher.v1"
	ProtectedGitPolicy            = "helm-protected-git.v1"
	protectedPrerequisiteContract = "helm-publication-prerequisite.v1"
	ArgoApplicationAPIVersion     = "argoproj.io/v1alpha1"
	ArgoApplicationKind           = "Application"
	ArgoInClusterServer           = "https://kubernetes.default.svc"
	MaximumProtectedAttempts      = 30
)

var (
	gitCommitRE        = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	gitRefRE           = regexp.MustCompile(`^refs/heads/[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	githubOwnerRE      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9])?$`)
	githubRepositoryRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
)

type ProtectedPublisherIdentity struct {
	Contract, PolicyVersion, ConfigDigest string
}

func (i ProtectedPublisherIdentity) Validate() error {
	if i.Contract != ProtectedPublisherContract || i.PolicyVersion != ProtectedGitPolicy ||
		!validDigest(i.ConfigDigest) {
		return ErrInvalid
	}
	return nil
}

type ProtectedBindingSnapshot struct {
	PlatformBindingID, EnvironmentBindingID, ClusterID string
	PlatformTargetRef, EnvironmentTargetRef            string
	EnvironmentRevision                                string
	EnvironmentGeneration                              int64
	CatalogDigest, PlannedBaseRevision                 string
}

// ArgoMaterializationAuthority is the worker-owned identity against which an
// environment materialization receipt is admitted. It is not persisted in a
// Helm intent: admission always uses the currently running worker authority.
type ArgoMaterializationAuthority struct {
	PolicyDigest      string
	Runtime           argo.RuntimeLock
	DigestEnforcement argo.ChartDigestEnforcement
}

func (a ArgoMaterializationAuthority) Validate() error {
	if !validDigest(a.PolicyDigest) || a.Runtime.Validate() != nil ||
		a.DigestEnforcement != argo.ChartDigestNativeOCI {
		return ErrInvalid
	}
	return nil
}

// ProtectedPublicationPrerequisiteReceipt is immutable proof that one release
// was planned against the exact current environment projection and foundation,
// plus the latest verified Argo desired-state command which owns its AppProject.
// The command may precede unrelated branch-only projection advances when the
// rendered desired state did not change. The protected Git publisher revalidates
// its immutable terminal identities, then proves both commits are ancestors of
// its claim-time write base.
type ProtectedPublicationPrerequisiteReceipt struct {
	ReleaseRevisionID, ProjectID, EnvironmentID, ApplicationID string
	PlatformBindingID, EnvironmentBindingID, ClusterID         string
	EnvironmentRevision                                        string
	EnvironmentGeneration                                      int64
	FoundationIntentID, FoundationRevision                     string
	DesiredStateCommandID, DesiredStateRevision                string
	PlannedBaseRevision                                        string
	CreatedAt                                                  time.Time
}

func (r ProtectedPublicationPrerequisiteReceipt) Validate() error {
	if !uuidRE.MatchString(r.ReleaseRevisionID) || !uuidRE.MatchString(r.ProjectID) ||
		!uuidRE.MatchString(r.EnvironmentID) || !uuidRE.MatchString(r.ApplicationID) ||
		!uuidRE.MatchString(r.PlatformBindingID) || !uuidRE.MatchString(r.EnvironmentBindingID) ||
		!uuidRE.MatchString(r.ClusterID) || !gitCommitRE.MatchString(r.EnvironmentRevision) ||
		r.EnvironmentGeneration < 1 || !uuidRE.MatchString(r.FoundationIntentID) ||
		!gitCommitRE.MatchString(r.FoundationRevision) || !uuidRE.MatchString(r.DesiredStateCommandID) ||
		!gitCommitRE.MatchString(r.DesiredStateRevision) ||
		!gitCommitRE.MatchString(r.PlannedBaseRevision) || r.CreatedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

func (r ProtectedPublicationPrerequisiteReceipt) ValidateFor(releaseID string, target ReleaseTarget,
	binding ProtectedBindingSnapshot) error {
	if r.Validate() != nil || target.Validate() != nil || binding.Validate() != nil ||
		r.ReleaseRevisionID != releaseID || r.ProjectID != target.ProjectID ||
		r.EnvironmentID != target.EnvironmentID || r.ApplicationID != target.ApplicationID ||
		r.PlatformBindingID != binding.PlatformBindingID ||
		r.EnvironmentBindingID != binding.EnvironmentBindingID || r.ClusterID != binding.ClusterID ||
		r.EnvironmentRevision != binding.EnvironmentRevision ||
		r.EnvironmentGeneration != binding.EnvironmentGeneration {
		return ErrConflict
	}
	return nil
}

func (s ProtectedBindingSnapshot) Validate() error {
	if !uuidRE.MatchString(s.PlatformBindingID) || !uuidRE.MatchString(s.EnvironmentBindingID) ||
		!uuidRE.MatchString(s.ClusterID) || !validProtectedGitRef(s.PlatformTargetRef) ||
		!validProtectedGitRef(s.EnvironmentTargetRef) || !gitCommitRE.MatchString(s.EnvironmentRevision) ||
		s.EnvironmentGeneration < 1 || !validDigest(s.CatalogDigest) ||
		!gitCommitRE.MatchString(s.PlannedBaseRevision) {
		return ErrInvalid
	}
	return nil
}

type ProtectedIntentState string

const (
	ProtectedPending      ProtectedIntentState = "pending"
	ProtectedClaimed      ProtectedIntentState = "claimed"
	ProtectedGitCommitted ProtectedIntentState = "git-committed"
	ProtectedVerified     ProtectedIntentState = "verified"
	ProtectedFailed       ProtectedIntentState = "failed"
	ProtectedSuperseded   ProtectedIntentState = "superseded"
)

type ProtectedPayloadAction string

const (
	ProtectedPayloadPublish ProtectedPayloadAction = "publish"
	ProtectedPayloadDisable ProtectedPayloadAction = "disable-receipt"
)

type ProtectedPayloadIntent struct {
	ID, ReleaseRevisionID                      string
	ReleaseGeneration                          int64
	Target                                     ReleaseTarget
	Action                                     ProtectedPayloadAction
	Binding                                    ProtectedBindingSnapshot
	Path, ContentDigest                        string
	Content                                    []byte
	InventoryDigest                            string
	ResourceCount                              int
	IntentDigest                               string
	CommitTrailer                              string
	Publisher                                  ProtectedPublisherIdentity
	Message                                    string
	State                                      ProtectedIntentState
	NextAttemptAt                              time.Time
	Attempts, ConsecutiveFailures              int
	LastFailureCode, LeaseOwner                string
	LeaseEpoch                                 int64
	LeaseUntil                                 *time.Time
	WriteBaseRevision                          string
	WriteBaseObservedAt                        *time.Time
	CommittedRevision, CommittedParentRevision string
	CommittedAt, VerifiedAt                    *time.Time
	VerifiedPathDigest, ProviderRequest        string
	CreatedAt, UpdatedAt                       time.Time
	CompletedAt                                *time.Time
}

func (i ProtectedPayloadIntent) Validate() error {
	if !uuidRE.MatchString(i.ID) || !uuidRE.MatchString(i.ReleaseRevisionID) ||
		i.ReleaseGeneration < 1 || i.Target.Validate() != nil || i.Binding.Validate() != nil ||
		len(i.Content) < 1 || len(i.Content) > MaximumOutputSize ||
		!validDigest(i.ContentDigest) || digestBytes(i.Content) != i.ContentDigest ||
		!validDigest(i.IntentDigest) || i.CommitTrailer != "Kuberploy-Helm-Payload-Intent: "+i.ID ||
		i.Publisher.Validate() != nil || len(i.Message) < 1 || len(i.Message) > 512 ||
		containsControl(i.Message) || i.CreatedAt.IsZero() || i.UpdatedAt.Before(i.CreatedAt) ||
		i.NextAttemptAt.Before(i.CreatedAt) || i.Attempts < 0 || i.Attempts > MaximumProtectedAttempts ||
		i.ConsecutiveFailures < 0 || i.ConsecutiveFailures > MaximumProtectedAttempts ||
		(i.LastFailureCode == "") != (i.ConsecutiveFailures == 0) ||
		(i.LastFailureCode != "" && !failureCodeRE.MatchString(i.LastFailureCode)) ||
		(i.LeaseUntil != nil && !i.LeaseUntil.After(i.UpdatedAt)) ||
		(i.WriteBaseObservedAt != nil && (i.WriteBaseObservedAt.Before(i.CreatedAt) || i.WriteBaseObservedAt.After(i.UpdatedAt))) ||
		(i.CommittedAt != nil && (i.CommittedAt.Before(i.CreatedAt) || i.CommittedAt.After(i.UpdatedAt))) ||
		(i.VerifiedAt != nil && (i.VerifiedAt.Before(i.CreatedAt) || i.VerifiedAt.After(i.UpdatedAt))) ||
		(i.CompletedAt != nil && (i.CompletedAt.Before(i.CreatedAt) || i.CompletedAt.After(i.UpdatedAt))) || len(i.ProviderRequest) > 256 ||
		(i.ProviderRequest != "" && containsControl(i.ProviderRequest)) {
		return ErrInvalid
	}
	expectedPath := protectedPayloadPath(i.Binding.ClusterID, i.Target.EnvironmentID,
		i.Target.ApplicationID, i.ReleaseRevisionID, i.Action == ProtectedPayloadDisable)
	if i.Path != expectedPath {
		return ErrInvalid
	}
	switch i.Action {
	case ProtectedPayloadPublish:
		if !validDigest(i.InventoryDigest) || i.ResourceCount < 1 || i.ResourceCount > MaximumResources {
			return ErrInvalid
		}
	case ProtectedPayloadDisable:
		if i.InventoryDigest != "" || i.ResourceCount != 0 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return validateProtectedRuntimeShape(i.State, i.ContentDigest, i.Attempts,
		i.LeaseOwner, i.LeaseEpoch, i.LeaseUntil, i.WriteBaseRevision,
		i.WriteBaseObservedAt, i.CommittedRevision, i.CommittedParentRevision,
		i.CommittedAt, i.VerifiedAt, i.VerifiedPathDigest, i.ProviderRequest, i.CompletedAt)
}

type ProtectedApplicationAction string

const (
	ProtectedApplicationPublish ProtectedApplicationAction = "publish"
	ProtectedApplicationDelete  ProtectedApplicationAction = "delete"
)

type ProtectedApplicationIntent struct {
	ID, ReleaseRevisionID, PayloadIntentID                 string
	ReleaseGeneration                                      int64
	Target                                                 ReleaseTarget
	Action                                                 ProtectedApplicationAction
	Binding                                                ProtectedBindingSnapshot
	PayloadRevision, PayloadPath, SourceDirectory          string
	ApplicationPath, Operation, Precondition, ExpectedETag string
	Content                                                []byte
	ContentDigest, IntentDigest, CommitTrailer             string
	Publisher                                              ProtectedPublisherIdentity
	Message                                                string
	State                                                  ProtectedIntentState
	NextAttemptAt                                          time.Time
	Attempts, ConsecutiveFailures                          int
	LastFailureCode, LeaseOwner                            string
	LeaseEpoch                                             int64
	LeaseUntil                                             *time.Time
	WriteBaseRevision                                      string
	WriteBaseObservedAt                                    *time.Time
	CommittedRevision, CommittedParentRevision             string
	CommittedAt, VerifiedAt                                *time.Time
	VerifiedPathDigest, ProviderRequest                    string
	CreatedAt, UpdatedAt                                   time.Time
	CompletedAt                                            *time.Time
}

func (i ProtectedApplicationIntent) Validate() error {
	if !uuidRE.MatchString(i.ID) || !uuidRE.MatchString(i.ReleaseRevisionID) ||
		!uuidRE.MatchString(i.PayloadIntentID) || i.ReleaseGeneration < 1 ||
		i.Target.Validate() != nil || i.Binding.Validate() != nil ||
		!gitCommitRE.MatchString(i.PayloadRevision) ||
		i.PayloadPath != protectedPayloadPath(i.Binding.ClusterID, i.Target.EnvironmentID,
			i.Target.ApplicationID, i.ReleaseRevisionID, i.Action == ProtectedApplicationDelete) ||
		i.ApplicationPath != protectedApplicationPath(i.Binding.ClusterID,
			i.Target.EnvironmentID, i.Target.ApplicationID) ||
		!validDigest(i.IntentDigest) ||
		i.CommitTrailer != "Kuberploy-Helm-Application-Intent: "+i.ID ||
		i.Publisher.Validate() != nil || len(i.Message) < 1 || len(i.Message) > 512 ||
		containsControl(i.Message) || i.CreatedAt.IsZero() || i.UpdatedAt.Before(i.CreatedAt) ||
		i.NextAttemptAt.Before(i.CreatedAt) || i.Attempts < 0 || i.Attempts > MaximumProtectedAttempts ||
		i.ConsecutiveFailures < 0 || i.ConsecutiveFailures > MaximumProtectedAttempts ||
		(i.LastFailureCode == "") != (i.ConsecutiveFailures == 0) ||
		(i.LastFailureCode != "" && !failureCodeRE.MatchString(i.LastFailureCode)) ||
		(i.LeaseUntil != nil && !i.LeaseUntil.After(i.UpdatedAt)) ||
		(i.WriteBaseObservedAt != nil && (i.WriteBaseObservedAt.Before(i.CreatedAt) || i.WriteBaseObservedAt.After(i.UpdatedAt))) ||
		(i.CommittedAt != nil && (i.CommittedAt.Before(i.CreatedAt) || i.CommittedAt.After(i.UpdatedAt))) ||
		(i.VerifiedAt != nil && (i.VerifiedAt.Before(i.CreatedAt) || i.VerifiedAt.After(i.UpdatedAt))) ||
		(i.CompletedAt != nil && (i.CompletedAt.Before(i.CreatedAt) || i.CompletedAt.After(i.UpdatedAt))) || len(i.ProviderRequest) > 256 ||
		(i.ProviderRequest != "" && containsControl(i.ProviderRequest)) {
		return ErrInvalid
	}
	switch i.Action {
	case ProtectedApplicationPublish:
		if (i.Operation != "create" && i.Operation != "update") ||
			i.SourceDirectory != protectedSourceDirectory(i.Binding.ClusterID,
				i.Target.EnvironmentID, i.Target.ApplicationID, i.ReleaseRevisionID) ||
			len(i.Content) < 1 || len(i.Content) > MaximumDescriptorSize ||
			!validDigest(i.ContentDigest) || digestBytes(i.Content) != i.ContentDigest {
			return ErrInvalid
		}
	case ProtectedApplicationDelete:
		if i.Operation != "delete" || i.SourceDirectory != "" || len(i.Content) != 0 || i.ContentDigest != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if (i.Operation == "create" && (i.Precondition != "create-if-absent" || i.ExpectedETag != "")) ||
		((i.Operation == "update" || i.Operation == "delete") &&
			(i.Precondition != "match-etag" || !validProtectedETag(i.ExpectedETag))) {
		return ErrInvalid
	}
	return validateProtectedRuntimeShape(i.State, i.ContentDigest, i.Attempts,
		i.LeaseOwner, i.LeaseEpoch, i.LeaseUntil, i.WriteBaseRevision,
		i.WriteBaseObservedAt, i.CommittedRevision, i.CommittedParentRevision,
		i.CommittedAt, i.VerifiedAt, i.VerifiedPathDigest, i.ProviderRequest, i.CompletedAt)
}

func validateProtectedRuntimeShape(state ProtectedIntentState, contentDigest string, attempts int,
	leaseOwner string, leaseEpoch int64, leaseUntil *time.Time, writeBase string,
	writeBaseAt *time.Time, committed, parent string, committedAt, verifiedAt *time.Time,
	verifiedDigest, providerRequest string, completedAt *time.Time) error {
	leasePieces := leaseOwner != "" || leaseUntil != nil
	lease := workerIDRE.MatchString(leaseOwner) && leaseEpoch > 0 && leaseUntil != nil
	writePieces := writeBase != "" || writeBaseAt != nil
	writeReceipt := gitCommitRE.MatchString(writeBase) && writeBaseAt != nil
	commitPieces := committed != "" || parent != "" || committedAt != nil
	commitReceipt := gitCommitRE.MatchString(committed) && parent == writeBase && committedAt != nil
	verificationPieces := verifiedAt != nil || verifiedDigest != "" || providerRequest != ""
	if leasePieces != lease || writePieces != writeReceipt || commitPieces != commitReceipt ||
		(verifiedAt != nil && (providerRequest == "" || verifiedDigest != contentDigest)) ||
		(verifiedAt == nil && verificationPieces) ||
		(writeBaseAt != nil && committedAt != nil && committedAt.Before(*writeBaseAt)) ||
		(committedAt != nil && verifiedAt != nil && verifiedAt.Before(*committedAt)) {
		return ErrInvalid
	}
	switch state {
	case ProtectedPending:
		if lease || commitReceipt || verificationPieces || completedAt != nil {
			return ErrInvalid
		}
	case ProtectedClaimed:
		if !lease || attempts < 1 || commitReceipt || verificationPieces || completedAt != nil {
			return ErrInvalid
		}
	case ProtectedGitCommitted:
		if !lease || attempts < 1 || !writeReceipt || !commitReceipt || verifiedAt != nil || completedAt != nil {
			return ErrInvalid
		}
	case ProtectedVerified:
		if lease || attempts < 1 || !writeReceipt || !commitReceipt || verifiedAt == nil || completedAt == nil ||
			!completedAt.Equal(*verifiedAt) {
			return ErrInvalid
		}
	case ProtectedFailed, ProtectedSuperseded:
		if lease || commitReceipt || verificationPieces || completedAt == nil {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type ProtectedIntentLease struct {
	IntentID, Owner string
	Epoch           int64
	Until           time.Time
	Publisher       ProtectedPublisherIdentity
}

func (l ProtectedIntentLease) Validate() error {
	if !uuidRE.MatchString(l.IntentID) || !workerIDRE.MatchString(l.Owner) ||
		l.Epoch < 1 || l.Until.IsZero() || l.Publisher.Validate() != nil {
		return ErrInvalid
	}
	return nil
}

type ProtectedMutationAction string

const (
	ProtectedMutationUpsert ProtectedMutationAction = "upsert"
	ProtectedMutationDelete ProtectedMutationAction = "delete"
)

// ProtectedMutation is the typed handoff to the single hardened Git writer.
// RequiredAncestor is nonempty only for phase two and must be proven against
// the claim-time provider head before binding WriteBaseRevision.
type ProtectedMutation struct {
	IntentID, BindingID, TargetRef, Path     string
	Action                                   ProtectedMutationAction
	BaseRevision, Precondition, ExpectedETag string
	Content                                  []byte
	ContentDigest, Message, CommitTrailer    string
	RequiredAncestor                         string
}

func (m ProtectedMutation) Validate() error {
	if !uuidRE.MatchString(m.IntentID) || !uuidRE.MatchString(m.BindingID) ||
		!validProtectedGitRef(m.TargetRef) || !gitCommitRE.MatchString(m.BaseRevision) ||
		len(m.Path) < 1 || len(m.Path) > 1024 || strings.ContainsAny(m.Path, "\\\x00\r\n") ||
		len(m.Message) < 1 || len(m.Message) > 512 || containsControl(m.Message) ||
		m.CommitTrailer == "" {
		return ErrInvalid
	}
	if m.RequiredAncestor != "" && !gitCommitRE.MatchString(m.RequiredAncestor) {
		return ErrInvalid
	}
	payloadPath, applicationPath := validProtectedPayloadPath(m.Path), validProtectedApplicationPath(m.Path)
	if !payloadPath && !applicationPath {
		return ErrInvalid
	}
	if payloadPath && (m.Action != ProtectedMutationUpsert || m.RequiredAncestor != "" ||
		m.Precondition != "create-if-absent" || m.ExpectedETag != "" ||
		m.CommitTrailer != "Kuberploy-Helm-Payload-Intent: "+m.IntentID) {
		return ErrInvalid
	}
	if applicationPath && (m.RequiredAncestor == "" ||
		m.CommitTrailer != "Kuberploy-Helm-Application-Intent: "+m.IntentID) {
		return ErrInvalid
	}
	switch m.Action {
	case ProtectedMutationUpsert:
		if len(m.Content) < 1 || len(m.Content) > MaximumOutputSize ||
			!validDigest(m.ContentDigest) || digestBytes(m.Content) != m.ContentDigest ||
			!((m.Precondition == "create-if-absent" && m.ExpectedETag == "") ||
				(m.Precondition == "match-etag" && validProtectedETag(m.ExpectedETag))) {
			return ErrInvalid
		}
	case ProtectedMutationDelete:
		if len(m.Content) != 0 || m.ContentDigest != "" || m.Precondition != "match-etag" ||
			!validProtectedETag(m.ExpectedETag) || m.RequiredAncestor == "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func (i ProtectedPayloadIntent) Mutation() (ProtectedMutation, error) {
	baseRevision := i.WriteBaseRevision
	if baseRevision == "" {
		baseRevision = i.Binding.PlannedBaseRevision
	}
	mutation := ProtectedMutation{IntentID: i.ID, BindingID: i.Binding.PlatformBindingID,
		TargetRef: i.Binding.PlatformTargetRef, Path: i.Path, Action: ProtectedMutationUpsert,
		BaseRevision: baseRevision, Precondition: "create-if-absent",
		Content: append([]byte(nil), i.Content...), ContentDigest: i.ContentDigest,
		Message: i.Message, CommitTrailer: i.CommitTrailer}
	return mutation, mutation.Validate()
}

func (i ProtectedApplicationIntent) Mutation() (ProtectedMutation, error) {
	action := ProtectedMutationUpsert
	if i.Action == ProtectedApplicationDelete {
		action = ProtectedMutationDelete
	}
	baseRevision := i.WriteBaseRevision
	if baseRevision == "" {
		baseRevision = i.Binding.PlannedBaseRevision
	}
	mutation := ProtectedMutation{IntentID: i.ID, BindingID: i.Binding.PlatformBindingID,
		TargetRef: i.Binding.PlatformTargetRef, Path: i.ApplicationPath, Action: action,
		BaseRevision: baseRevision, Precondition: i.Precondition,
		ExpectedETag: i.ExpectedETag, Content: append([]byte(nil), i.Content...),
		ContentDigest: i.ContentDigest, Message: i.Message, CommitTrailer: i.CommitTrailer,
		RequiredAncestor: i.PayloadRevision}
	return mutation, mutation.Validate()
}

// gitMutation is the deliberately narrow adapter into gitprojection's one
// hardened mutation transport. The transport independently validates the
// authority, exact protected path family, digest, action, CAS precondition,
// required ancestor, and both recovery trailers.
func (m ProtectedMutation) gitMutation() (gitprojection.Mutation, error) {
	if m.Validate() != nil {
		return gitprojection.Mutation{}, ErrInvalid
	}
	action := gitprojection.MutationUpsert
	if m.Action == ProtectedMutationDelete {
		action = gitprojection.MutationDelete
	}
	authority := gitprojection.MutationAuthorityHelmApplication
	if validProtectedPayloadPath(m.Path) {
		authority = gitprojection.MutationAuthorityHelmPayload
	}
	mutation := gitprojection.Mutation{
		BindingID: m.BindingID, OperationID: m.IntentID, Path: m.Path,
		BaseRevision: m.BaseRevision, Precondition: gitprojection.MutationPrecondition(m.Precondition),
		ExpectedETag: m.ExpectedETag, Content: append([]byte(nil), m.Content...),
		ContentSHA256: m.ContentDigest, Message: m.Message, Action: action,
		Authority: authority, CommitTrailer: m.CommitTrailer, RequiredAncestor: m.RequiredAncestor,
	}
	return mutation, nil
}

type ProtectedPublicationStore interface {
	CreatePayloadForHead(context.Context, string, ReleaseTarget, ProtectedBindingSnapshot, ProtectedPublisherIdentity, time.Time) (ProtectedPayloadIntent, bool, error)
	Payload(context.Context, string) (ProtectedPayloadIntent, error)
	ClaimPayload(context.Context, string, ProtectedPublisherIdentity, time.Time, time.Duration) (ProtectedPayloadIntent, ProtectedIntentLease, error)
	HeartbeatPayload(context.Context, ProtectedIntentLease, time.Time, time.Duration) (ProtectedIntentLease, error)
	BindPayloadWriteBase(context.Context, ProtectedIntentLease, string, time.Time, time.Time) (ProtectedPayloadIntent, error)
	RebindPayloadWriteBase(context.Context, ProtectedIntentLease, string, string, time.Time, time.Time) (ProtectedPayloadIntent, error)
	MarkPayloadCommitted(context.Context, ProtectedIntentLease, string, string, time.Time) (ProtectedPayloadIntent, error)
	VerifyPayload(context.Context, ProtectedIntentLease, string, string, string, time.Time) (ProtectedPayloadIntent, error)
	RetryPayload(context.Context, ProtectedIntentLease, string, time.Time, time.Time) (ProtectedPayloadIntent, error)
	FailPayload(context.Context, ProtectedIntentLease, string, time.Time) (ProtectedPayloadIntent, error)
	PublicationPrerequisite(context.Context, string) (ProtectedPublicationPrerequisiteReceipt, error)

	CreateApplicationForPayload(context.Context, string, string, ProtectedApplicationRuntime, ProtectedPublisherIdentity, time.Time) (ProtectedApplicationIntent, bool, error)
	Application(context.Context, string) (ProtectedApplicationIntent, error)
	ClaimApplication(context.Context, string, ProtectedPublisherIdentity, time.Time, time.Duration) (ProtectedApplicationIntent, ProtectedIntentLease, error)
	HeartbeatApplication(context.Context, ProtectedIntentLease, time.Time, time.Duration) (ProtectedIntentLease, error)
	BindApplicationWriteBase(context.Context, ProtectedIntentLease, string, time.Time, time.Time) (ProtectedApplicationIntent, error)
	RebindApplicationWriteBase(context.Context, ProtectedIntentLease, string, string, time.Time, time.Time) (ProtectedApplicationIntent, error)
	MarkApplicationCommitted(context.Context, ProtectedIntentLease, string, string, time.Time) (ProtectedApplicationIntent, error)
	VerifyApplication(context.Context, ProtectedIntentLease, string, string, string, time.Time) (ProtectedApplicationIntent, error)
	RetryApplication(context.Context, ProtectedIntentLease, string, time.Time, time.Time) (ProtectedApplicationIntent, error)
	FailApplication(context.Context, ProtectedIntentLease, string, time.Time) (ProtectedApplicationIntent, error)
	PutPublisherReadiness(context.Context, ProtectedPublisherReadiness) error
	PublisherReady(context.Context, ProtectedPublisherIdentity, time.Time) (bool, error)
}

type ProtectedApplicationRuntime struct {
	ArgoNamespace string
}

func (r ProtectedApplicationRuntime) Validate() error {
	if !dnsLabelRE.MatchString(r.ArgoNamespace) {
		return ErrInvalid
	}
	return nil
}

type ProtectedPublisherReadiness struct {
	WorkerID                          string
	WorkerEpoch                       int64
	Publisher                         ProtectedPublisherIdentity
	StartedAt, ObservedAt, LeaseUntil time.Time
}

func (r ProtectedPublisherReadiness) Validate() error {
	if !workerIDRE.MatchString(r.WorkerID) || r.WorkerEpoch < 1 ||
		r.Publisher.Validate() != nil || r.StartedAt.IsZero() ||
		r.ObservedAt.Before(r.StartedAt) || !r.LeaseUntil.After(r.ObservedAt) {
		return ErrInvalid
	}
	return nil
}

type protectedArgoMetadata struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

type protectedArgoSource struct {
	RepoURL        string `yaml:"repoURL"`
	TargetRevision string `yaml:"targetRevision"`
	Path           string `yaml:"path"`
	Directory      struct {
		Recurse bool   `yaml:"recurse"`
		Include string `yaml:"include"`
	} `yaml:"directory"`
}

type protectedArgoDestination struct {
	Server    string `yaml:"server"`
	Namespace string `yaml:"namespace"`
}

type protectedArgoSyncPolicy struct {
	Automated struct {
		Prune      bool `yaml:"prune"`
		SelfHeal   bool `yaml:"selfHeal"`
		AllowEmpty bool `yaml:"allowEmpty"`
	} `yaml:"automated"`
	SyncOptions []string `yaml:"syncOptions"`
}

type protectedArgoApplication struct {
	APIVersion string                `yaml:"apiVersion"`
	Kind       string                `yaml:"kind"`
	Metadata   protectedArgoMetadata `yaml:"metadata"`
	Spec       struct {
		Project     string                   `yaml:"project"`
		Source      protectedArgoSource      `yaml:"source"`
		Destination protectedArgoDestination `yaml:"destination"`
		SyncPolicy  protectedArgoSyncPolicy  `yaml:"syncPolicy"`
	} `yaml:"spec"`
}

func renderProtectedArgoApplication(intentID string, release ReleaseRevision,
	payload ProtectedPayloadIntent, runtime ProtectedApplicationRuntime,
	repositoryOwner, repositoryName, destinationNamespace, argoProject string) ([]byte, error) {
	if !uuidRE.MatchString(intentID) || release.Validate() != nil || payload.Validate() != nil ||
		payload.State != ProtectedVerified || payload.Action != ProtectedPayloadPublish ||
		payload.ReleaseRevisionID != release.ID || runtime.Validate() != nil ||
		!githubOwnerRE.MatchString(repositoryOwner) || !githubRepositoryRE.MatchString(repositoryName) ||
		repositoryName == "." || repositoryName == ".." || !dnsLabelRE.MatchString(destinationNamespace) ||
		!dnsLabelRE.MatchString(argoProject) {
		return nil, ErrInvalid
	}
	var manifest protectedArgoApplication
	manifest.APIVersion, manifest.Kind = ArgoApplicationAPIVersion, ArgoApplicationKind
	manifest.Metadata.Name = "kp-h-" + strings.ReplaceAll(release.Target.ApplicationID, "-", "")
	manifest.Metadata.Namespace = runtime.ArgoNamespace
	manifest.Metadata.Labels = map[string]string{
		"app.kubernetes.io/managed-by": "kuberploy",
		"app.kubernetes.io/component":  "approved-helm-application",
		"kuberploy.io/project-id":      release.Target.ProjectID,
		"kuberploy.io/environment-id":  release.Target.EnvironmentID,
		"kuberploy.io/application-id":  release.Target.ApplicationID,
	}
	manifest.Metadata.Annotations = map[string]string{
		"kuberploy.io/helm-release-revision":         release.ID,
		"kuberploy.io/helm-payload-revision":         payload.CommittedRevision,
		"kuberploy.io/helm-payload-digest":           payload.ContentDigest,
		"argocd.argoproj.io/manifest-generate-paths": payload.Path,
	}
	manifest.Spec.Project = argoProject
	manifest.Spec.Source.RepoURL = "https://github.com/" + repositoryOwner + "/" + repositoryName + ".git"
	manifest.Spec.Source.TargetRevision = payload.CommittedRevision
	manifest.Spec.Source.Path = protectedSourceDirectory(payload.Binding.ClusterID,
		release.Target.EnvironmentID, release.Target.ApplicationID, release.ID)
	manifest.Spec.Source.Directory.Recurse = false
	manifest.Spec.Source.Directory.Include = "release.yaml"
	manifest.Spec.Destination.Server = ArgoInClusterServer
	manifest.Spec.Destination.Namespace = destinationNamespace
	manifest.Spec.SyncPolicy.Automated.Prune = true
	manifest.Spec.SyncPolicy.Automated.SelfHeal = true
	manifest.Spec.SyncPolicy.Automated.AllowEmpty = false
	manifest.Spec.SyncPolicy.SyncOptions = []string{"CreateNamespace=false", "ServerSideApply=true"}
	content, err := yaml.Marshal(manifest)
	if err != nil || len(content) < 1 || len(content) > MaximumDescriptorSize {
		return nil, ErrInvalid
	}
	return content, nil
}

func protectedPayloadPath(clusterID, environmentID, applicationID, releaseID string, disabled bool) string {
	name := "release.yaml"
	if disabled {
		name = "disabled.json"
	}
	return protectedSourceDirectory(clusterID, environmentID, applicationID, releaseID) + "/" + name
}

func protectedSourceDirectory(clusterID, environmentID, applicationID, releaseID string) string {
	return "clusters/" + clusterID + "/helm-manifests/environments/" + environmentID +
		"/applications/" + applicationID + "/revisions/" + releaseID
}

func protectedApplicationPath(clusterID, environmentID, applicationID string) string {
	return "clusters/" + clusterID + "/argocd/helm-applications/" + environmentID + "/" + applicationID + ".yaml"
}

func validProtectedPayloadPath(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 10 || parts[0] != "clusters" || !uuidRE.MatchString(parts[1]) ||
		parts[2] != "helm-manifests" || parts[3] != "environments" || !uuidRE.MatchString(parts[4]) ||
		parts[5] != "applications" || !uuidRE.MatchString(parts[6]) || parts[7] != "revisions" ||
		!uuidRE.MatchString(parts[8]) {
		return false
	}
	return parts[9] == "release.yaml" || parts[9] == "disabled.json"
}

func validProtectedApplicationPath(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 6 || parts[0] != "clusters" || !uuidRE.MatchString(parts[1]) ||
		parts[2] != "argocd" || parts[3] != "helm-applications" || !uuidRE.MatchString(parts[4]) ||
		!strings.HasSuffix(parts[5], ".yaml") {
		return false
	}
	return uuidRE.MatchString(strings.TrimSuffix(parts[5], ".yaml"))
}

func validProtectedETag(value string) bool {
	return len(value) == len(`"sha256:`)+64+1 && strings.HasPrefix(value, `"sha256:`) &&
		strings.HasSuffix(value, `"`) && validDigest(strings.Trim(value, `"`))
}

func validProtectedGitRef(value string) bool {
	return gitRefRE.MatchString(value) && !strings.Contains(value, "..") &&
		!strings.Contains(value, "//") && !strings.HasSuffix(value, "/")
}

func protectedIntentDigest(value any) (string, error) { return digestJSON(value) }
