package certissuers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"time"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const (
	protectedIssuerContract = "certificate-issuer-protected-git.v1"
	protectedIssuerLease    = 90 * time.Second
	protectedIssuerCleanup  = 15 * time.Second
)

var ErrMaterializationUnavailable = errors.New("cert-manager issuer materialization is unavailable")

type ProtectedGitConfig struct {
	BindingID              string
	ClusterID              string
	Owner                  string
	ObserverNamespace      string
	ObserverServiceAccount string
}

func (c ProtectedGitConfig) Validate() error {
	if !uuidRE.MatchString(c.BindingID) || !uuidRE.MatchString(c.ClusterID) ||
		len(c.Owner) < 8 || len(c.Owner) > 128 || !utf8.ValidString(c.Owner) ||
		!dnsLabelRE.MatchString(c.ObserverNamespace) || !dnsLabelRE.MatchString(c.ObserverServiceAccount) {
		return ErrInvalid
	}
	for _, value := range []byte(c.Owner) {
		if value == 0 || value == '\r' || value == '\n' {
			return ErrInvalid
		}
	}
	return nil
}

// ProtectedGitConfigForObserver derives publisher authority from the same
// server-owned identity used by the read-only observer, preventing RBAC
// subject/binding drift between the two runtimes.
func ProtectedGitConfigForObserver(owner string, observer ObserverConfig) (ProtectedGitConfig, error) {
	if observer.Validate() != nil || !observer.Enabled {
		return ProtectedGitConfig{}, ErrInvalid
	}
	config := ProtectedGitConfig{BindingID: observer.BindingID, ClusterID: observer.ClusterID, Owner: owner,
		ObserverNamespace: observer.Namespace, ObserverServiceAccount: observer.ServiceAccount}
	return config, config.Validate()
}

// ProtectedGitPublisher is the only Git mutation transport for dynamic
// ClusterIssuer profiles. The constructor accepts the existing projection
// store, provider verifier, and MirrorManager; it never creates a second Git
// client, accepts a remote URL, or accepts caller-supplied YAML or paths.
type ProtectedGitPublisher struct {
	store    gitprojection.Store
	provider gitprojection.HeadVerifier
	manager  *gitprojection.MirrorManager
	config   ProtectedGitConfig
	now      func() time.Time
}

func NewProtectedGitPublisher(store gitprojection.Store, provider gitprojection.HeadVerifier,
	manager *gitprojection.MirrorManager, config ProtectedGitConfig, now func() time.Time) (*ProtectedGitPublisher, error) {
	if store == nil || provider == nil || manager == nil || manager.Validate() != nil || config.Validate() != nil || now == nil {
		return nil, ErrInvalid
	}
	return &ProtectedGitPublisher{store: store, provider: provider, manager: manager, config: config, now: now}, nil
}

type PublicationReceipt struct {
	OperationID, BindingID, ClusterID, TargetRef, Path string
	SpecDigest, ContentDigest                          string
	Revision                                           int64
	Action                                             string
	ParentRevision, CommittedRevision, ProviderHead    string
	ProviderRequest                                    string
	ObservedAt                                         time.Time
	Changed                                            bool
}

func (p *ProtectedGitPublisher) Materialize(ctx context.Context, desired Desired) (PublicationReceipt, error) {
	return p.publish(ctx, desired, gitprojection.MutationUpsert)
}

func (p *ProtectedGitPublisher) publish(ctx context.Context, desired Desired, action gitprojection.MutationAction) (PublicationReceipt, error) {
	if ctx == nil || p == nil || p.store == nil || p.provider == nil || p.manager == nil || p.config.Validate() != nil || p.now == nil ||
		(action != gitprojection.MutationUpsert && action != gitprojection.MutationDelete) {
		return PublicationReceipt{}, ErrInvalid
	}
	clean, solver, digest, err := normalizeSpec(desired.Spec)
	if err != nil || !uuidRE.MatchString(desired.ProfileID) || !dnsLabelRE.MatchString(desired.Name) ||
		desired.Revision < 1 || desired.Solver != solver || desired.SpecDigest != digest {
		return PublicationReceipt{}, ErrInvalid
	}
	desired.Spec = clean
	content, err := RenderProtectedClusterIssuerBundle(desired, ProtectedObserverSubject{
		Namespace: p.config.ObserverNamespace, ServiceAccount: p.config.ObserverServiceAccount,
	})
	if err != nil {
		return PublicationReceipt{}, err
	}
	contentDigest := protectedDigest(content)
	documentPath := protectedIssuerPath(p.config.ClusterID, desired.Name)
	operationID := protectedOperationID(p.config, desired, action, contentDigest)

	binding, err := p.store.Binding(ctx, p.config.BindingID)
	if err != nil {
		return PublicationReceipt{}, classifyProtectedGit(err)
	}
	if binding.Validate() != nil || binding.Kind != gitprojection.BindingPlatform ||
		binding.CredentialMode != gitprojection.CredentialGitHubApp || binding.CredentialSecretName != "" ||
		binding.ID != p.config.BindingID || binding.ClusterID != p.config.ClusterID ||
		binding.Prefix != gitprojection.PlatformPrefix(p.config.ClusterID) || binding.ProjectionGeneration < 1 {
		return PublicationReceipt{}, ErrInvalid
	}

	reservation, reservationErr := p.store.PathReservation(ctx, binding.ID, binding.TargetRef, documentPath)
	if reservationErr == nil {
		if reservation.OperationID != operationID || reservation.Owner != p.config.Owner || reservation.TargetRef != binding.TargetRef {
			return PublicationReceipt{}, errors.Join(ErrConflict, gitprojection.ErrLeaseHeld)
		}
	} else if !errors.Is(reservationErr, gitprojection.ErrNotFound) {
		return PublicationReceipt{}, classifyProtectedGit(reservationErr)
	} else if binding.State != gitprojection.BindingReady || binding.TargetHeadRevision == "" ||
		binding.TargetHeadRevision != binding.IndexedRevision {
		return PublicationReceipt{}, errors.Join(ErrConflict, gitprojection.ErrStale)
	}

	head, err := p.provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return PublicationReceipt{}, classifyProtectedGit(err)
	}
	now := p.now().UTC()
	if head.ValidateFor(binding) != nil || head.Source != gitprojection.ObservationWrite || head.ObservedAt.After(now) {
		return PublicationReceipt{}, ErrInvalid
	}
	if err = p.manager.CleanupOperation(ctx, binding.ID, operationID); err != nil {
		return PublicationReceipt{}, classifyProtectedGit(err)
	}
	prepared, err := p.manager.Prepare(ctx, binding, head, operationID)
	if err != nil {
		return PublicationReceipt{}, classifyProtectedGit(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), protectedIssuerCleanup)
		defer cancel()
		_ = prepared.Close(cleanup)
	}()
	plannedHead := binding.IndexedRevision
	if plannedHead == "" {
		return PublicationReceipt{}, ErrInvalid
	}
	if err = prepared.VerifyAncestor(ctx, plannedHead); err != nil {
		return PublicationReceipt{}, classifyProtectedGit(err)
	}

	if errors.Is(reservationErr, gitprojection.ErrNotFound) {
		if head.Commit != plannedHead {
			return PublicationReceipt{}, errors.Join(ErrConflict, gitprojection.ErrStale)
		}
		present, _, existingDigest, inspectErr := prepared.ProtectedCertificateIssuerPreimage(ctx, documentPath, plannedHead)
		if inspectErr != nil {
			return PublicationReceipt{}, classifyProtectedGit(inspectErr)
		}
		if action == gitprojection.MutationUpsert && present && existingDigest == contentDigest ||
			action == gitprojection.MutationDelete && !present {
			return p.noopReceipt(ctx, binding, head, desired, action, operationID, documentPath, contentDigest)
		}
		if action == gitprojection.MutationDelete && existingDigest != contentDigest {
			return PublicationReceipt{}, errors.Join(ErrConflict, errors.New("ClusterIssuer delete preimage differs from the exact catalog revision"))
		}
		candidateMutation, mutationErr := protectedIssuerMutation(ctx, binding, desired, action, operationID, documentPath, plannedHead, plannedHead, content, contentDigest, prepared)
		if mutationErr != nil {
			return PublicationReceipt{}, classifyProtectedGit(mutationErr)
		}
		if mutationErr = prepared.VerifyProtectedMutationPrecondition(ctx, candidateMutation); mutationErr != nil {
			return PublicationReceipt{}, classifyProtectedGit(mutationErr)
		}
		leaseUntil := now.Add(protectedIssuerLease)
		candidate := gitprojection.PathReservation{BindingID: binding.ID, TargetRef: binding.TargetRef, Path: documentPath,
			OperationID: operationID, Owner: p.config.Owner, BaseRevision: plannedHead, State: gitprojection.ReservationCandidate,
			LeaseUntil: &leaseUntil, CreatedAt: now, UpdatedAt: now}
		reservation, _, err = p.store.AcquirePath(ctx, candidate, now, protectedIssuerLease)
		if err != nil {
			return PublicationReceipt{}, classifyProtectedGit(err)
		}
	}

	if err = prepared.VerifyAncestor(ctx, reservation.BaseRevision); err != nil {
		return PublicationReceipt{}, classifyProtectedGit(err)
	}
	mutation, err := protectedIssuerMutation(ctx, binding, desired, action, operationID, documentPath, reservation.BaseRevision,
		plannedHead, content, contentDigest, prepared)
	if err != nil {
		return PublicationReceipt{}, classifyProtectedGit(err)
	}
	committed, found, err := prepared.FindOperationCommit(ctx, mutation)
	if err != nil {
		return PublicationReceipt{}, classifyProtectedGit(err)
	}
	if found {
		if err = prepared.VerifyProtectedMutationPostimage(ctx, mutation); err != nil {
			return PublicationReceipt{}, classifyProtectedGit(err)
		}
		return p.finalizeReceipt(ctx, binding, head, desired, action, operationID, documentPath, contentDigest, reservation.BaseRevision, committed, true)
	}
	if head.Commit != reservation.BaseRevision {
		return PublicationReceipt{}, errors.Join(ErrConflict, errors.New("verified Git head advanced without the exact certificate-issuer operation"))
	}
	if err = prepared.VerifyProtectedMutationPrecondition(ctx, mutation); err != nil {
		return PublicationReceipt{}, classifyProtectedGit(err)
	}
	committed, err = prepared.Commit(ctx, mutation)
	if err != nil {
		return PublicationReceipt{}, classifyProtectedGit(err)
	}
	return p.finalizeReceipt(ctx, binding, gitprojection.VerifiedHead{}, desired, action, operationID, documentPath,
		contentDigest, reservation.BaseRevision, committed, false)
}

func protectedIssuerMutation(ctx context.Context, binding gitprojection.Binding, desired Desired, action gitprojection.MutationAction,
	operationID, documentPath, base, plannedHead string, content []byte, contentDigest string,
	prepared *gitprojection.PreparedRepository) (gitprojection.Mutation, error) {
	present, etag, existingDigest, err := prepared.ProtectedCertificateIssuerPreimage(ctx, documentPath, base)
	if err != nil {
		return gitprojection.Mutation{}, err
	}
	precondition := gitprojection.MutationCreateIfAbsent
	if present {
		precondition = gitprojection.MutationMatchETag
	}
	if action == gitprojection.MutationDelete {
		if !present || existingDigest != contentDigest {
			return gitprojection.Mutation{}, errors.Join(ErrConflict, errors.New("ClusterIssuer delete requires the exact rendered catalog preimage"))
		}
		content, contentDigest = nil, ""
	}
	return gitprojection.Mutation{BindingID: binding.ID, OperationID: operationID, Path: documentPath, BaseRevision: base,
		Precondition: precondition, ExpectedETag: etag, Content: append([]byte(nil), content...), ContentSHA256: contentDigest,
		Message: "materialize certificate issuer " + desired.Name, Action: action,
		Authority:     gitprojection.MutationAuthorityCertificateIssuer,
		CommitTrailer: "Kuberploy-Certificate-Issuer-Intent: " + operationID, RequiredAncestor: plannedHead}, nil
}

func (p *ProtectedGitPublisher) noopReceipt(ctx context.Context, binding gitprojection.Binding, head gitprojection.VerifiedHead,
	desired Desired, action gitprojection.MutationAction, operationID, documentPath, contentDigest string) (PublicationReceipt, error) {
	verified, err := p.provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return PublicationReceipt{}, classifyProtectedGit(err)
	}
	if verified.ValidateFor(binding) != nil || verified.Commit != head.Commit || verified.Source != gitprojection.ObservationWrite || verified.ObservedAt.After(p.now().UTC()) {
		return PublicationReceipt{}, errors.Join(ErrConflict, gitprojection.ErrProviderMismatch)
	}
	return protectedReceipt(binding, desired, action, operationID, documentPath, contentDigest,
		head.Commit, head.Commit, verified, false), nil
}

func (p *ProtectedGitPublisher) finalizeReceipt(ctx context.Context, binding gitprojection.Binding, before gitprojection.VerifiedHead,
	desired Desired, action gitprojection.MutationAction, operationID, documentPath, contentDigest, parent, committed string,
	recovery bool) (PublicationReceipt, error) {
	verified, err := p.provider.VerifyTargetHead(ctx, binding, gitprojection.ObservationWrite)
	if err != nil {
		return PublicationReceipt{}, classifyProtectedGit(err)
	}
	if verified.ValidateFor(binding) != nil || verified.Source != gitprojection.ObservationWrite || verified.ObservedAt.After(p.now().UTC()) ||
		(!recovery && verified.Commit != committed) || (recovery && verified.Commit != before.Commit) {
		return PublicationReceipt{}, errors.Join(ErrConflict, gitprojection.ErrProviderMismatch)
	}
	if _, err = p.store.FinalizePath(ctx, binding.ID, binding.TargetRef, documentPath, operationID, committed, p.notBefore(verified.ObservedAt)); err != nil {
		return PublicationReceipt{}, classifyProtectedGit(err)
	}
	return protectedReceipt(binding, desired, action, operationID, documentPath, contentDigest,
		parent, committed, verified, true), nil
}

func protectedReceipt(binding gitprojection.Binding, desired Desired, action gitprojection.MutationAction,
	operationID, documentPath, contentDigest, parent, committed string, verified gitprojection.VerifiedHead, changed bool) PublicationReceipt {
	return PublicationReceipt{OperationID: operationID, BindingID: binding.ID, ClusterID: binding.ClusterID,
		TargetRef: binding.TargetRef, Path: documentPath, SpecDigest: desired.SpecDigest, ContentDigest: contentDigest,
		Revision: desired.Revision, Action: string(action), ParentRevision: parent, CommittedRevision: committed,
		ProviderHead: verified.Commit, ProviderRequest: verified.ProviderRequest, ObservedAt: verified.ObservedAt.UTC(), Changed: changed}
}

func (p *ProtectedGitPublisher) notBefore(values ...time.Time) time.Time {
	now := p.now().UTC()
	for _, value := range values {
		if now.Before(value) {
			now = value.UTC()
		}
	}
	return now
}

func protectedIssuerPath(clusterID, name string) string {
	return path.Join(gitprojection.PlatformPrefix(clusterID), "argocd", "platform", "certificate-issuers", name+".yaml")
}

func protectedDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func protectedOperationID(config ProtectedGitConfig, desired Desired, action gitprojection.MutationAction, contentDigest string) string {
	raw, _ := json.Marshal([]any{protectedIssuerContract, config.BindingID, config.ClusterID, config.ObserverNamespace,
		config.ObserverServiceAccount, desired.ProfileID, desired.Name,
		desired.Revision, desired.SpecDigest, action, contentDigest})
	sum := sha256.Sum256(raw)
	value := append([]byte(nil), sum[:16]...)
	value[6] = value[6]&0x0f | 0x50
	value[8] = value[8]&0x3f | 0x80
	hexValue := hex.EncodeToString(value)
	return hexValue[:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:]
}

func classifyProtectedGit(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gitprojection.ErrInvalid):
		return errors.Join(ErrInvalid, err)
	case errors.Is(err, gitprojection.ErrConflict), errors.Is(err, gitprojection.ErrStale),
		errors.Is(err, gitprojection.ErrLeaseHeld), errors.Is(err, gitprojection.ErrLeaseLost),
		errors.Is(err, gitprojection.ErrProviderMismatch):
		return errors.Join(ErrConflict, err)
	default:
		return errors.Join(ErrMaterializationUnavailable, err)
	}
}

// RenderClusterIssuer deterministically renders one closed cert-manager object.
// JSON-quoted scalar tokens are valid YAML and avoid a generic YAML input or
// serializer accepting aliases, tags, duplicate keys, or arbitrary fields.
func RenderClusterIssuer(desired Desired) ([]byte, error) {
	clean, solver, digest, err := normalizeSpec(desired.Spec)
	if err != nil || !dnsLabelRE.MatchString(desired.Name) || desired.Revision < 1 || desired.Solver != solver || desired.SpecDigest != digest {
		return nil, ErrInvalid
	}
	q := func(value string) string { raw, _ := json.Marshal(value); return string(raw) }
	var out bytes.Buffer
	fmt.Fprintf(&out, "apiVersion: cert-manager.io/v1\nkind: ClusterIssuer\nmetadata:\n  name: %s\n", q(desired.Name))
	fmt.Fprintf(&out, "  labels:\n    app.kubernetes.io/managed-by: %s\n    kuberploy.io/certificate-issuer-profile: %s\n", q("kuberploy"), q(desired.Name))
	fmt.Fprintf(&out, "  annotations:\n    kuberploy.io/certificate-issuer-spec-digest: %s\n    kuberploy.io/certificate-issuer-revision: %s\n", q(desired.SpecDigest), q(fmt.Sprintf("%d", desired.Revision)))
	fmt.Fprintf(&out, "spec:\n  acme:\n    email: %s\n    server: %s\n    privateKeySecretRef:\n      name: %s\n    solvers:\n", q(clean.ACME.Email), q(clean.ACME.Server), q(clean.ACME.AccountPrivateKeySecretName))
	if solver == HTTP01 {
		fmt.Fprintf(&out, "      - http01:\n          ingress:\n            ingressClassName: %s\n            ingressTemplate:\n              metadata:\n                annotations:\n                  external-dns.alpha.kubernetes.io/ingress-hostname-source: %s\n", q("traefik"), q("annotation-only"))
	} else {
		fmt.Fprint(&out, "      - selector:\n          dnsZones:\n")
		for _, zone := range clean.Cloudflare.DNSZones {
			fmt.Fprintf(&out, "            - %s\n", q(zone))
		}
		fmt.Fprintf(&out, "        dns01:\n          cloudflare:\n            apiTokenSecretRef:\n              name: %s\n              key: %s\n", q(clean.Cloudflare.APITokenSecretName), q(clean.Cloudflare.APITokenSecretKey))
	}
	return out.Bytes(), nil
}

// ProtectedObserverSubject is server configuration, never catalog or tenant input.
// Each dynamic issuer receives an individual get-only ClusterRole so the
// observer never needs list/watch, Secret access, or mutation privileges.
type ProtectedObserverSubject struct {
	Namespace, ServiceAccount string
}

func (i ProtectedObserverSubject) Validate() error {
	if !dnsLabelRE.MatchString(i.Namespace) || !dnsLabelRE.MatchString(i.ServiceAccount) {
		return ErrInvalid
	}
	return nil
}

// RenderProtectedClusterIssuerBundle renders the ClusterIssuer and its exact
// least-privilege read authority as one CAS/delete unit.
func RenderProtectedClusterIssuerBundle(desired Desired, observer ProtectedObserverSubject) ([]byte, error) {
	issuer, err := RenderClusterIssuer(desired)
	if err != nil || observer.Validate() != nil || !uuidRE.MatchString(desired.ProfileID) {
		return nil, ErrInvalid
	}
	q := func(value string) string { raw, _ := json.Marshal(value); return string(raw) }
	name := protectedObserverRBACName(desired)
	var out bytes.Buffer
	out.Write(issuer)
	fmt.Fprintf(&out, "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: %s\n", q(name))
	fmt.Fprintf(&out, "  labels:\n    app.kubernetes.io/managed-by: %s\n    kuberploy.io/certificate-issuer-profile: %s\nrules:\n", q("kuberploy"), q(desired.Name))
	fmt.Fprintf(&out, "  - apiGroups:\n      - %s\n    resources:\n      - %s\n    resourceNames:\n      - %s\n    verbs:\n      - %s\n", q("cert-manager.io"), q("clusterissuers"), q(desired.Name), q("get"))
	fmt.Fprintf(&out, "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: %s\n", q(name))
	fmt.Fprintf(&out, "  labels:\n    app.kubernetes.io/managed-by: %s\n    kuberploy.io/certificate-issuer-profile: %s\nroleRef:\n", q("kuberploy"), q(desired.Name))
	fmt.Fprintf(&out, "  apiGroup: %s\n  kind: ClusterRole\n  name: %s\nsubjects:\n  - kind: ServiceAccount\n    name: %s\n    namespace: %s\n", q("rbac.authorization.k8s.io"), q(name), q(observer.ServiceAccount), q(observer.Namespace))
	return out.Bytes(), nil
}

func protectedObserverRBACName(desired Desired) string {
	raw, _ := json.Marshal([]string{protectedIssuerContract, desired.ProfileID, desired.Name})
	sum := sha256.Sum256(raw)
	return "kuberploy-ci-observer-" + hex.EncodeToString(sum[:10])
}
