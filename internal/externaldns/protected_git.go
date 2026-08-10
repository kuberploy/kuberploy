package externaldns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const protectedContract = "external-dns-protected-git.v1"

type ProtectedGitConfig struct {
	BindingID, ClusterID, Owner string
	Template                    ManagedRuntimeTemplate
}

func (c ProtectedGitConfig) Validate() error {
	if !uuidRE.MatchString(c.BindingID) || !uuidRE.MatchString(c.ClusterID) || len(c.Owner) < 8 || len(c.Owner) > 128 || c.Template.Validate() != nil {
		return ErrRuntimeUnavailable
	}
	return nil
}

type ProtectedPublisher struct {
	store    gitprojection.Store
	provider gitprojection.HeadVerifier
	manager  *gitprojection.MirrorManager
	config   ProtectedGitConfig
	now      func() time.Time
}

func NewProtectedPublisher(store gitprojection.Store, provider gitprojection.HeadVerifier, manager *gitprojection.MirrorManager, config ProtectedGitConfig, now func() time.Time) (*ProtectedPublisher, error) {
	if store == nil || provider == nil || manager == nil || manager.Validate() != nil || config.Validate() != nil || now == nil {
		return nil, ErrRuntimeUnavailable
	}
	return &ProtectedPublisher{store, provider, manager, config, now}, nil
}

type PublicationReceipt struct {
	IntegrationID, Path, OperationID, ContentDigest, CommittedRevision string
	RuntimeRevision                                                    int64
	Deleted, Changed                                                   bool
}

func (p *ProtectedPublisher) Reconcile(ctx context.Context, item domain.ExternalDNSIntegration) (PublicationReceipt, error) {
	if p == nil || p.config.Validate() != nil || !uuidRE.MatchString(item.ID) || item.RuntimeRevision < 1 || item.Mode != ModeManaged {
		return PublicationReceipt{}, ErrRuntimeUnavailable
	}
	content, _, err := RenderManagedBundle(itemForRendering(item), p.config.Template)
	if err != nil {
		return PublicationReceipt{}, err
	}
	action := gitprojection.MutationUpsert
	if item.Lifecycle == "deactivated" {
		action = gitprojection.MutationDelete
	} else if item.Lifecycle != "active" {
		return PublicationReceipt{}, ErrRuntimeUnavailable
	}
	return p.publish(ctx, item, content, action)
}

func itemForRendering(item domain.ExternalDNSIntegration) domain.ExternalDNSIntegration {
	item.Lifecycle = "active"
	return item
}

func (p *ProtectedPublisher) publish(ctx context.Context, item domain.ExternalDNSIntegration, content []byte, action gitprojection.MutationAction) (PublicationReceipt, error) {
	docPath := path.Join(gitprojection.PlatformPrefix(p.config.ClusterID), "argocd", "platform", "external-dns", item.ID+".yaml")
	contentDigest := digest(content)
	operationID := externalDNSOperationID(p.config, item, action, contentDigest)
	binding, err := p.store.Binding(ctx, p.config.BindingID)
	if err != nil {
		return PublicationReceipt{}, err
	}
	if binding.Validate() != nil || binding.Kind != gitprojection.BindingPlatform || binding.CredentialMode != gitprojection.CredentialGitHubApp || binding.ID != p.config.BindingID || binding.ClusterID != p.config.ClusterID || binding.Prefix != gitprojection.PlatformPrefix(p.config.ClusterID) {
		return PublicationReceipt{}, ErrRuntimeUnavailable
	}
	reservation, reservationErr := p.store.PathReservation(ctx, binding.ID, binding.TargetRef, docPath)
	if reservationErr == nil {
		if reservation.OperationID != operationID || reservation.Owner != p.config.Owner {
			return PublicationReceipt{}, gitprojection.ErrLeaseHeld
		}
	} else if !errors.Is(reservationErr, gitprojection.ErrNotFound) {
		return PublicationReceipt{}, reservationErr
	} else if binding.State != gitprojection.BindingReady || binding.TargetHeadRevision == "" || binding.TargetHeadRevision != binding.IndexedRevision {
		return PublicationReceipt{}, gitprojection.ErrStale
	}
	head, err := p.provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return PublicationReceipt{}, err
	}
	if head.ValidateFor(binding) != nil || head.Source != gitprojection.ObservationWrite {
		return PublicationReceipt{}, gitprojection.ErrProviderMismatch
	}
	if err = p.manager.CleanupOperation(ctx, binding.ID, operationID); err != nil {
		return PublicationReceipt{}, err
	}
	prepared, err := p.manager.Prepare(ctx, binding, head, operationID)
	if err != nil {
		return PublicationReceipt{}, err
	}
	defer prepared.Close(context.WithoutCancel(ctx)) //nolint:errcheck
	planned := binding.IndexedRevision
	if err = prepared.VerifyAncestor(ctx, planned); err != nil {
		return PublicationReceipt{}, err
	}
	if errors.Is(reservationErr, gitprojection.ErrNotFound) {
		if head.Commit != planned {
			return PublicationReceipt{}, gitprojection.ErrStale
		}
		present, _, existing, inspectErr := prepared.ProtectedExternalDNSPreimage(ctx, docPath, planned)
		if inspectErr != nil {
			return PublicationReceipt{}, inspectErr
		}
		if action == gitprojection.MutationUpsert && present && existing == contentDigest || action == gitprojection.MutationDelete && !present {
			verified, verifyErr := p.provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
			if verifyErr != nil {
				return PublicationReceipt{}, verifyErr
			}
			if verified.ValidateFor(binding) != nil || verified.Source != gitprojection.ObservationWrite || verified.Commit != head.Commit {
				return PublicationReceipt{}, gitprojection.ErrProviderMismatch
			}
			return PublicationReceipt{IntegrationID: item.ID, Path: docPath, OperationID: operationID, ContentDigest: contentDigest, CommittedRevision: planned, RuntimeRevision: item.RuntimeRevision, Deleted: action == gitprojection.MutationDelete}, nil
		}
		if action == gitprojection.MutationDelete && existing != contentDigest {
			return PublicationReceipt{}, gitprojection.ErrConflict
		}
		mutation, mutationErr := externalDNSMutation(ctx, binding, item, action, operationID, docPath, planned, planned, content, contentDigest, prepared)
		if mutationErr != nil {
			return PublicationReceipt{}, mutationErr
		}
		if mutationErr = prepared.VerifyProtectedMutationPrecondition(ctx, mutation); mutationErr != nil {
			return PublicationReceipt{}, mutationErr
		}
		now := p.now().UTC()
		until := now.Add(90 * time.Second)
		candidate := gitprojection.PathReservation{BindingID: binding.ID, TargetRef: binding.TargetRef, Path: docPath, OperationID: operationID, Owner: p.config.Owner, BaseRevision: planned, State: gitprojection.ReservationCandidate, LeaseUntil: &until, CreatedAt: now, UpdatedAt: now}
		reservation, _, err = p.store.AcquirePath(ctx, candidate, now, 90*time.Second)
		if err != nil {
			return PublicationReceipt{}, err
		}
	}
	mutation, err := externalDNSMutation(ctx, binding, item, action, operationID, docPath, reservation.BaseRevision, planned, content, contentDigest, prepared)
	if err != nil {
		return PublicationReceipt{}, err
	}
	committed, found, err := prepared.FindOperationCommit(ctx, mutation)
	if err != nil {
		return PublicationReceipt{}, err
	}
	if !found {
		if head.Commit != reservation.BaseRevision {
			return PublicationReceipt{}, gitprojection.ErrStale
		}
		if err = prepared.VerifyProtectedMutationPrecondition(ctx, mutation); err != nil {
			return PublicationReceipt{}, err
		}
		committed, err = prepared.Commit(ctx, mutation)
		if err != nil {
			return PublicationReceipt{}, err
		}
	}
	verified, err := p.provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return PublicationReceipt{}, err
	}
	if verified.ValidateFor(binding) != nil || verified.Commit != committed {
		return PublicationReceipt{}, gitprojection.ErrProviderMismatch
	}
	if _, err = p.store.FinalizePath(ctx, binding.ID, binding.TargetRef, docPath, operationID, committed, p.now().UTC()); err != nil {
		return PublicationReceipt{}, err
	}
	return PublicationReceipt{IntegrationID: item.ID, Path: docPath, OperationID: operationID, ContentDigest: contentDigest, CommittedRevision: committed, RuntimeRevision: item.RuntimeRevision, Deleted: action == gitprojection.MutationDelete, Changed: true}, nil
}

func externalDNSMutation(ctx context.Context, binding gitprojection.Binding, item domain.ExternalDNSIntegration, action gitprojection.MutationAction, operationID, docPath, base, planned string, content []byte, contentDigest string, prepared *gitprojection.PreparedRepository) (gitprojection.Mutation, error) {
	present, etag, existing, err := prepared.ProtectedExternalDNSPreimage(ctx, docPath, base)
	if err != nil {
		return gitprojection.Mutation{}, err
	}
	precondition := gitprojection.MutationCreateIfAbsent
	if present {
		precondition = gitprojection.MutationMatchETag
	}
	if action == gitprojection.MutationDelete {
		if !present || existing != contentDigest {
			return gitprojection.Mutation{}, gitprojection.ErrConflict
		}
		content = nil
		contentDigest = ""
	}
	return gitprojection.Mutation{BindingID: binding.ID, OperationID: operationID, Path: docPath, BaseRevision: base, Precondition: precondition, ExpectedETag: etag, Content: content, ContentSHA256: contentDigest, Message: "materialize external-dns integration " + item.Slug, Action: action, Authority: gitprojection.MutationAuthorityExternalDNS, CommitTrailer: "Kuberploy-External-DNS-Intent: " + operationID, RequiredAncestor: planned}, nil
}

func externalDNSOperationID(config ProtectedGitConfig, item domain.ExternalDNSIntegration, action gitprojection.MutationAction, contentDigest string) string {
	raw, _ := json.Marshal([]any{protectedContract, config.BindingID, config.ClusterID, item.ID, item.RuntimeRevision, action, contentDigest})
	sum := sha256.Sum256(raw)
	value := append([]byte(nil), sum[:16]...)
	value[6] = value[6]&0x0f | 0x50
	value[8] = value[8]&0x3f | 0x80
	h := hex.EncodeToString(value)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
