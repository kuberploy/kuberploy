// Package externaldns validates and manages safe ExternalDNS integration
// desired state. It materializes only closed server-rendered bundles and never
// handles provider credential values or arbitrary provider payloads.
package externaldns

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/store"
)

var (
	ErrInvalid              = errors.New("external-dns integration input is invalid")
	ErrIntegrationReference = errors.New("external-dns integration reference is not authorized")
	ErrHostnameNotAllowed   = errors.New("hostname is outside the integration's allowed domain suffixes")
)

const (
	ModeManaged         = "managed"
	ModeAdopted         = "adopted"
	SyncPolicyUpsert    = "upsert-only"
	SyncPolicySync      = "sync"
	ReadinessUnobserved = "unobserved"
	ReadinessReady      = "ready"
)

var (
	slugRE     = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	txtOwnerRE = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9._]{0,126}[a-z0-9])?$`)
	refRE      = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$`)
	labelRE    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	uuidRE     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type Store interface {
	ListExternalDNSIntegrationsForActor(context.Context, string) ([]domain.ExternalDNSIntegration, error)
	CreateExternalDNSIntegrationForActor(context.Context, string, string, string, string, domain.ExternalDNSIntegration) (store.Result[domain.ExternalDNSIntegration], error)
	UpdateExternalDNSIntegrationForActor(context.Context, string, string, string, string, domain.ExternalDNSIntegration) (store.Result[domain.ExternalDNSIntegration], error)
	ExternalDNSIntegrationsForEnvironmentActor(context.Context, string, string) ([]domain.ExternalDNSIntegration, error)
	ExternalDNSIntegrationsForApplicationActor(context.Context, string, string, string) ([]domain.ExternalDNSIntegration, error)
}

type Management struct {
	store Store
	newID func() string
	now   func() time.Time
}

type Option func(*Management)

func WithIDGenerator(newID func() string) Option { return func(m *Management) { m.newID = newID } }
func WithClock(now func() time.Time) Option      { return func(m *Management) { m.now = now } }

func NewManagement(repository Store, options ...Option) *Management {
	m := &Management{store: repository, newID: id.New, now: func() time.Time { return time.Now().UTC() }}
	for _, option := range options {
		option(m)
	}
	return m
}

type IntegrationInput struct {
	Slug                     string
	Name                     string
	Mode                     string
	ProviderKind             string
	TXTOwnerID               string
	AllowedDomainSuffixes    []string
	SyncPolicy               string
	DestructiveSyncConfirmed bool
	CredentialSecretRef      string
	ProviderConfigRef        string
	EgressConfigRef          string
	OperatorProfileRef       string
	EnvironmentIDs           []string
}

func (m *Management) Integrations(ctx context.Context, actor string) ([]domain.ExternalDNSIntegration, error) {
	if m == nil || m.store == nil {
		return nil, ErrInvalid
	}
	return m.store.ListExternalDNSIntegrationsForActor(ctx, actor)
}

func (m *Management) Create(ctx context.Context, actor, key, fingerprint, requestID string, input IntegrationInput) (store.Result[domain.ExternalDNSIntegration], error) {
	if m == nil || m.store == nil || m.newID == nil || m.now == nil {
		return store.Result[domain.ExternalDNSIntegration]{}, ErrInvalid
	}
	integration := integrationFromInput(m.newID(), actor, input)
	if err := Validate(integration); err != nil {
		return store.Result[domain.ExternalDNSIntegration]{}, err
	}
	return m.store.CreateExternalDNSIntegrationForActor(ctx, actor, key, fingerprint, requestID, integration)
}

func (m *Management) Update(ctx context.Context, actor, key, fingerprint, requestID, integrationID string, input IntegrationInput) (store.Result[domain.ExternalDNSIntegration], error) {
	if m == nil || m.store == nil || m.now == nil {
		return store.Result[domain.ExternalDNSIntegration]{}, ErrInvalid
	}
	integration := integrationFromInput(strings.TrimSpace(integrationID), actor, input)
	if err := Validate(integration); err != nil {
		return store.Result[domain.ExternalDNSIntegration]{}, err
	}
	return m.store.UpdateExternalDNSIntegrationForActor(ctx, actor, key, fingerprint, requestID, integration)
}

func (m *Management) Deactivate(ctx context.Context, actor, key, fingerprint, requestID, integrationID string) (store.Result[domain.ExternalDNSIntegration], error) {
	if m == nil || m.store == nil || !uuidRE.MatchString(strings.TrimSpace(integrationID)) {
		return store.Result[domain.ExternalDNSIntegration]{}, ErrInvalid
	}
	deactivator, ok := m.store.(interface {
		DeactivateExternalDNSIntegrationForActor(context.Context, string, string, string, string, string) (store.Result[domain.ExternalDNSIntegration], error)
	})
	if !ok {
		return store.Result[domain.ExternalDNSIntegration]{}, ErrInvalid
	}
	return deactivator.DeactivateExternalDNSIntegrationForActor(ctx, actor, key, fingerprint, requestID, strings.TrimSpace(integrationID))
}

func (m *Management) EnvironmentCatalog(ctx context.Context, actor, environmentID string) ([]domain.ExternalDNSCatalogItem, error) {
	if m == nil || m.store == nil || !uuidRE.MatchString(strings.TrimSpace(environmentID)) {
		return nil, ErrInvalid
	}
	items, err := m.store.ExternalDNSIntegrationsForEnvironmentActor(ctx, actor, strings.TrimSpace(environmentID))
	if err != nil {
		return nil, err
	}
	return catalog(items), nil
}

func (m *Management) ApplicationCatalog(ctx context.Context, actor, applicationID, environmentID string) ([]domain.ExternalDNSCatalogItem, error) {
	if m == nil || m.store == nil || !uuidRE.MatchString(strings.TrimSpace(applicationID)) || !uuidRE.MatchString(strings.TrimSpace(environmentID)) {
		return nil, ErrInvalid
	}
	items, err := m.store.ExternalDNSIntegrationsForApplicationActor(ctx, actor, strings.TrimSpace(applicationID), strings.TrimSpace(environmentID))
	if err != nil {
		return nil, err
	}
	return catalog(items), nil
}

func (m *Management) ValidateApplicationRoute(ctx context.Context, actor, applicationID, environmentID, integrationRef, hostname string) error {
	items, err := m.ApplicationCatalog(ctx, actor, applicationID, environmentID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Slug != strings.TrimSpace(integrationRef) {
			continue
		}
		if !HostnameAllowed(hostname, item.AllowedDomainSuffixes) {
			return ErrHostnameNotAllowed
		}
		return nil
	}
	return ErrIntegrationReference
}

func catalog(items []domain.ExternalDNSIntegration) []domain.ExternalDNSCatalogItem {
	out := make([]domain.ExternalDNSCatalogItem, 0, len(items))
	for _, item := range items {
		if item.Lifecycle != "" && item.Lifecycle != "active" {
			continue
		}
		out = append(out, domain.ExternalDNSCatalogItem{ID: item.ID, Slug: item.Slug, Name: item.Name, Mode: item.Mode, ProviderKind: item.ProviderKind, AllowedDomainSuffixes: append([]string(nil), item.AllowedDomainSuffixes...), RuntimeRevision: item.RuntimeRevision})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Slug < out[j].Slug
	})
	return out
}

func integrationFromInput(integrationID, actor string, input IntegrationInput) domain.ExternalDNSIntegration {
	syncPolicy := strings.TrimSpace(input.SyncPolicy)
	if syncPolicy == "" {
		syncPolicy = SyncPolicyUpsert
	}
	return domain.ExternalDNSIntegration{
		ID: strings.TrimSpace(integrationID), Slug: strings.TrimSpace(input.Slug), Name: strings.TrimSpace(input.Name),
		Mode: strings.TrimSpace(input.Mode), ProviderKind: strings.TrimSpace(input.ProviderKind), TXTOwnerID: strings.TrimSpace(input.TXTOwnerID),
		AllowedDomainSuffixes: normalizeSuffixes(input.AllowedDomainSuffixes), SyncPolicy: syncPolicy,
		DestructiveSyncConfirmed: input.DestructiveSyncConfirmed, CredentialSecretRef: strings.TrimSpace(input.CredentialSecretRef),
		ProviderConfigRef: strings.TrimSpace(input.ProviderConfigRef), EgressConfigRef: strings.TrimSpace(input.EgressConfigRef),
		OperatorProfileRef: strings.TrimSpace(input.OperatorProfileRef), EnvironmentIDs: normalizeIDs(input.EnvironmentIDs), CreatedBy: actor,
		RuntimeRevision: 1, Lifecycle: "active",
	}
}

func Validate(integration domain.ExternalDNSIntegration) error {
	if !uuidRE.MatchString(integration.ID) || !slugRE.MatchString(integration.Slug) || len(integration.Name) < 1 || len(integration.Name) > 100 || strings.TrimSpace(integration.Name) != integration.Name || hasControl(integration.Name) || !txtOwnerRE.MatchString(integration.TXTOwnerID) {
		return ErrInvalid
	}
	switch integration.ProviderKind {
	case "aws", "azure", "cloudflare", "google", "rfc2136":
	default:
		return ErrInvalid
	}
	if len(integration.AllowedDomainSuffixes) < 1 || len(integration.AllowedDomainSuffixes) > 64 || len(integration.EnvironmentIDs) < 1 || len(integration.EnvironmentIDs) > 256 {
		return ErrInvalid
	}
	seenSuffixes := map[string]bool{}
	for _, suffix := range integration.AllowedDomainSuffixes {
		if !validDNSName(suffix) || suffix != strings.ToLower(suffix) || seenSuffixes[suffix] {
			return ErrInvalid
		}
		seenSuffixes[suffix] = true
	}
	seenIDs := map[string]bool{}
	for _, environmentID := range integration.EnvironmentIDs {
		if !uuidRE.MatchString(environmentID) || seenIDs[environmentID] {
			return ErrInvalid
		}
		seenIDs[environmentID] = true
	}
	if (integration.SyncPolicy == SyncPolicyUpsert && integration.DestructiveSyncConfirmed) || (integration.SyncPolicy == SyncPolicySync && !integration.DestructiveSyncConfirmed) || (integration.SyncPolicy != SyncPolicyUpsert && integration.SyncPolicy != SyncPolicySync) {
		return ErrInvalid
	}
	switch integration.Mode {
	case ModeManaged:
		if !refRE.MatchString(integration.CredentialSecretRef) || !refRE.MatchString(integration.ProviderConfigRef) || !refRE.MatchString(integration.EgressConfigRef) || integration.OperatorProfileRef != "" {
			return ErrInvalid
		}
	case ModeAdopted:
		if !refRE.MatchString(integration.OperatorProfileRef) || integration.CredentialSecretRef != "" || integration.ProviderConfigRef != "" || integration.EgressConfigRef != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func HostnameAllowed(host string, suffixes []string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if !validDNSName(host) {
		return false
	}
	for _, suffix := range suffixes {
		suffix = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(suffix), "."))
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func validDNSName(name string) bool {
	if len(name) < 1 || len(name) > 253 || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || !strings.Contains(name, ".") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !labelRE.MatchString(label) {
			return false
		}
	}
	return true
}

func normalizeSuffixes(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), ".")))
	}
	sort.Strings(out)
	return out
}

func normalizeIDs(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, strings.TrimSpace(value))
	}
	sort.Strings(out)
	return out
}

func hasControl(value string) bool {
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return true
		}
	}
	return false
}
