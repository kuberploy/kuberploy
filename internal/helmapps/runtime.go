package helmapps

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/id"
)

const (
	RuntimeConfigContract                   = "external-helm-production-runtime.v2"
	ProtectedPublisherRuntimeConfigContract = "helm-protected-publisher-runtime.v2"
)

type RuntimeConfig struct {
	Enabled                bool
	Renderer               KubernetesRendererConfig
	WorkPollInterval       time.Duration
	RenderLeaseDuration    time.Duration
	PublishLeaseDuration   time.Duration
	ReadinessLeaseDuration time.Duration
	OCIRequestTimeout      time.Duration
	OCIRegistryHosts       []string
	OCIAuthHosts           []string
	OCIRedirectHosts       []string
	OCICredentialProfiles  []OCIRegistryCredentialProfile
	PackageCacheBytes      int
	Application            ProtectedApplicationRuntime
	Publisher              ProtectedPublisherIdentity
}

// Validate deliberately treats disabled configuration as dormant. This lets
// a chart retain values while guaranteeing that no dependency, credential, or
// Kubernetes client is constructed until Enabled becomes true. Every field is
// revalidated as one closed contract when enabled.
func (c RuntimeConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Renderer.Validate() != nil ||
		c.WorkPollInterval < 100*time.Millisecond || c.WorkPollInterval > 30*time.Second ||
		c.RenderLeaseDuration < RenderTimeout+5*time.Second || c.RenderLeaseDuration > 15*time.Minute ||
		!validProtectedLeaseDuration(c.PublishLeaseDuration) ||
		c.ReadinessLeaseDuration < 5*time.Second || c.ReadinessLeaseDuration > 5*time.Minute ||
		c.WorkPollInterval*2 > c.ReadinessLeaseDuration ||
		c.OCIRequestTimeout < time.Second || c.OCIRequestTimeout > time.Minute ||
		c.PackageCacheBytes < MaximumChartSize || c.PackageCacheBytes > 1<<30 ||
		c.Application.Validate() != nil || c.Publisher.Validate() != nil ||
		validateOCIHostList(c.OCIRegistryHosts, true) != nil ||
		validateOCIHostList(c.OCIAuthHosts, false) != nil ||
		validateOCIHostList(c.OCIRedirectHosts, false) != nil ||
		validateOCICredentialProfiles(c.OCICredentialProfiles, c.OCIRegistryHosts, c.OCIAuthHosts) != nil {
		return ErrInvalid
	}
	return nil
}

func validateOCIHostList(hosts []string, required bool) error {
	if required && len(hosts) == 0 || len(hosts) > maximumOCIHostCount || !sort.StringsAreSorted(hosts) {
		return ErrInvalid
	}
	for index, host := range hosts {
		if !validOCIHost(host) || index > 0 && host == hosts[index-1] {
			return ErrInvalid
		}
	}
	return nil
}

func (c RuntimeConfig) IdentityDigest() (string, error) {
	if c.Validate() != nil || !c.Enabled {
		return "", ErrInvalid
	}
	return digestJSON(struct {
		Contract               string                         `json:"contract"`
		Runtime                RuntimeIdentity                `json:"runtime"`
		RendererNamespace      string                         `json:"rendererNamespace"`
		RendererServiceAccount string                         `json:"rendererServiceAccount"`
		WorkPollMillis         int64                          `json:"workPollMillis"`
		RenderLeaseSeconds     int64                          `json:"renderLeaseSeconds"`
		PublishLeaseSeconds    int64                          `json:"publishLeaseSeconds"`
		ReadinessLeaseSeconds  int64                          `json:"readinessLeaseSeconds"`
		OCIRequestSeconds      int64                          `json:"ociRequestSeconds"`
		OCIRegistryHosts       []string                       `json:"ociRegistryHosts"`
		OCIAuthHosts           []string                       `json:"ociAuthHosts"`
		OCIRedirectHosts       []string                       `json:"ociRedirectHosts"`
		OCICredentialProfiles  []OCIRegistryCredentialProfile `json:"ociCredentialProfiles"`
		PackageCacheBytes      int                            `json:"packageCacheBytes"`
		Application            ProtectedApplicationRuntime    `json:"application"`
		Publisher              ProtectedPublisherIdentity     `json:"publisher"`
	}{RuntimeConfigContract, ExpectedRuntimeIdentity(), c.Renderer.Namespace,
		c.Renderer.ServiceAccount, c.WorkPollInterval.Milliseconds(),
		int64(c.RenderLeaseDuration.Seconds()), int64(c.PublishLeaseDuration.Seconds()), int64(c.ReadinessLeaseDuration.Seconds()),
		int64(c.OCIRequestTimeout.Seconds()), append([]string(nil), c.OCIRegistryHosts...),
		append([]string(nil), c.OCIAuthHosts...), append([]string(nil), c.OCIRedirectHosts...),
		append([]OCIRegistryCredentialProfile(nil), c.OCICredentialProfiles...),
		c.PackageCacheBytes, c.Application, c.Publisher})
}

// ProtectedPublisherIdentityForRuntime binds publisher/readiness receipts to
// both the base hardened Git projection runtime and the complete enabled Helm
// runtime policy. Publisher is intentionally excluded from the canonical
// payload because ConfigDigest is the value being derived.
func ProtectedPublisherIdentityForRuntime(baseGitProjection gitprojection.RuntimeIdentity,
	config RuntimeConfig) (ProtectedPublisherIdentity, error) {
	if baseGitProjection.Validate() != nil || config.Validate() != nil || !config.Enabled {
		return ProtectedPublisherIdentity{}, ErrInvalid
	}
	digest, err := digestJSON(struct {
		Contract               string                         `json:"contract"`
		Enabled                bool                           `json:"enabled"`
		BaseGitProjection      gitprojection.RuntimeIdentity  `json:"baseGitProjection"`
		Runtime                RuntimeIdentity                `json:"runtime"`
		RendererNamespace      string                         `json:"rendererNamespace"`
		RendererServiceAccount string                         `json:"rendererServiceAccount"`
		RendererPollMillis     int64                          `json:"rendererPollMillis"`
		WorkPollMillis         int64                          `json:"workPollMillis"`
		RenderLeaseSeconds     int64                          `json:"renderLeaseSeconds"`
		PublishLeaseSeconds    int64                          `json:"publishLeaseSeconds"`
		ReadinessLeaseSeconds  int64                          `json:"readinessLeaseSeconds"`
		OCIRequestSeconds      int64                          `json:"ociRequestSeconds"`
		OCIRegistryHosts       []string                       `json:"ociRegistryHosts"`
		OCIAuthHosts           []string                       `json:"ociAuthHosts"`
		OCIRedirectHosts       []string                       `json:"ociRedirectHosts"`
		OCICredentialProfiles  []OCIRegistryCredentialProfile `json:"ociCredentialProfiles"`
		PackageCacheBytes      int                            `json:"packageCacheBytes"`
		Application            ProtectedApplicationRuntime    `json:"application"`
	}{ProtectedPublisherRuntimeConfigContract, true, baseGitProjection, ExpectedRuntimeIdentity(),
		config.Renderer.Namespace, config.Renderer.ServiceAccount, config.Renderer.PollInterval.Milliseconds(),
		config.WorkPollInterval.Milliseconds(), int64(config.RenderLeaseDuration.Seconds()),
		int64(config.PublishLeaseDuration.Seconds()), int64(config.ReadinessLeaseDuration.Seconds()),
		int64(config.OCIRequestTimeout.Seconds()), append([]string(nil), config.OCIRegistryHosts...),
		append([]string(nil), config.OCIAuthHosts...), append([]string(nil), config.OCIRedirectHosts...),
		append([]OCIRegistryCredentialProfile(nil), config.OCICredentialProfiles...),
		config.PackageCacheBytes, config.Application})
	if err != nil {
		return ProtectedPublisherIdentity{}, err
	}
	identity := ProtectedPublisherIdentity{Contract: ProtectedPublisherContract,
		PolicyVersion: ProtectedGitPolicy, ConfigDigest: digest}
	return identity, identity.Validate()
}

type ProtectedArgoReadiness interface {
	ProtectedHelmApplicationsReady(context.Context, ProtectedPublisherIdentity, time.Time) (bool, error)
}

type rendererReadiness interface {
	RuntimeReady(context.Context, time.Time) (bool, error)
}

type publisherReadiness interface {
	PublisherReady(context.Context, ProtectedPublisherIdentity, time.Time) (bool, error)
}

type Capabilities struct {
	HelmDeployments bool
	HelmRollbacks   bool
}

type CapabilityGate struct {
	Enabled     bool
	Renderer    rendererReadiness
	Publisher   publisherReadiness
	Argo        ProtectedArgoReadiness
	PublisherID ProtectedPublisherIdentity
}

func (g CapabilityGate) Evaluate(ctx context.Context, now time.Time) (Capabilities, error) {
	if !g.Enabled {
		return Capabilities{}, nil
	}
	if ctx == nil || now.IsZero() || g.Renderer == nil || g.Publisher == nil || g.Argo == nil ||
		g.PublisherID.Validate() != nil {
		return Capabilities{}, ErrInvalid
	}
	rendererReady, rendererErr := g.Renderer.RuntimeReady(ctx, now.UTC())
	publisherReady, publisherErr := g.Publisher.PublisherReady(ctx, g.PublisherID, now.UTC())
	argoReady, argoErr := g.Argo.ProtectedHelmApplicationsReady(ctx, g.PublisherID, now.UTC())
	for _, err := range []error{rendererErr, publisherErr, argoErr} {
		if err != nil {
			return Capabilities{}, err
		}
	}
	ready := rendererReady && publisherReady && argoReady
	return Capabilities{HelmDeployments: ready, HelmRollbacks: ready}, nil
}

type RuntimeDependencies struct {
	Pool                *pgxpool.Pool
	OCIClient           *http.Client
	Credentials         OCIRegistryCredentialProvider
	RendererAPI         RendererKubernetesAPI
	Bindings            ProtectedBindingResolver
	ArgoMaterialization ArgoMaterializationAuthority
	GitBindings         ProtectedGitBindingStore
	GitProvider         gitprojection.HeadVerifier
	GitManager          *gitprojection.MirrorManager
	WorkerID            string
	WorkerEpoch         int64
	StartedAt           time.Time
	Now                 func() time.Time
	NewID               func() string
	ReportError         func(string, error)
}

type ociCredentialReadiness interface {
	Probe(context.Context) error
}

type Runtime struct {
	Enabled      bool
	Config       RuntimeConfig
	Store        Store
	Releases     ReleaseService
	Values       ReleaseValuesService
	Publications ProtectedPublicationPlanningStore
	Worker       Worker
	Planner      PublicationPlanner
	Publisher    protectedPublisherProcessor
	workerID     string
	workerEpoch  int64
	startedAt    time.Time
	reportError  func(string, error)
	credentials  ociCredentialReadiness
}

func NewRuntime(config RuntimeConfig, dependencies RuntimeDependencies) (*Runtime, error) {
	if config.Validate() != nil {
		return nil, ErrInvalid
	}
	if !config.Enabled {
		return &Runtime{Config: config}, nil
	}
	if dependencies.Pool == nil || dependencies.OCIClient == nil || dependencies.RendererAPI == nil ||
		dependencies.Bindings == nil || dependencies.ArgoMaterialization.Validate() != nil ||
		dependencies.GitBindings == nil || dependencies.GitProvider == nil ||
		dependencies.GitManager == nil || dependencies.GitManager.Validate() != nil ||
		dependencies.Now == nil || dependencies.StartedAt.IsZero() ||
		!workerIDRE.MatchString(dependencies.WorkerID) || dependencies.WorkerEpoch < 1 ||
		dependencies.ReportError == nil {
		return nil, ErrInvalid
	}
	if dependencies.NewID == nil {
		dependencies.NewID = id.New
	}
	var credentialReadiness ociCredentialReadiness
	if len(config.OCICredentialProfiles) != 0 {
		var ok bool
		credentialReadiness, ok = dependencies.Credentials.(ociCredentialReadiness)
		if !ok || credentialReadiness == nil {
			return nil, ErrInvalid
		}
	}
	store, err := NewPostgresStore(dependencies.Pool, config.Publisher.ConfigDigest)
	if err != nil {
		return nil, err
	}
	releases, err := NewPostgresReleaseService(dependencies.Pool, config.Publisher.ConfigDigest)
	if err != nil {
		return nil, err
	}
	publications, err := NewPostgresProtectedPublicationStore(dependencies.Pool,
		dependencies.ArgoMaterialization)
	if err != nil {
		return nil, err
	}
	client := *dependencies.OCIClient
	client.Timeout = config.OCIRequestTimeout
	packages := &CachedChartPackageSource{Upstream: OCIHTTPPackageSource{Client: &client,
		AllowedRegistryHosts: append([]string(nil), config.OCIRegistryHosts...),
		AllowedAuthHosts:     append([]string(nil), config.OCIAuthHosts...),
		AllowedRedirectHosts: append([]string(nil), config.OCIRedirectHosts...), Credentials: dependencies.Credentials},
		MaxBytes: config.PackageCacheBytes}
	worker := Worker{Store: store, Packages: packages,
		Renderer:      KubernetesRenderExecutor{API: dependencies.RendererAPI, Config: config.Renderer},
		LeaseDuration: config.RenderLeaseDuration, Now: dependencies.Now,
		OperatorConfigDigest: config.Publisher.ConfigDigest}
	if worker.Validate() != nil {
		return nil, ErrInvalid
	}
	planner := PublicationPlanner{Store: publications, Bindings: dependencies.Bindings,
		Publisher: config.Publisher, Application: config.Application,
		NewID: dependencies.NewID, Now: dependencies.Now}
	if planner.Validate() != nil {
		return nil, ErrInvalid
	}
	publisher := &ProtectedGitPublisher{Store: publications, Bindings: dependencies.GitBindings,
		Provider: dependencies.GitProvider, Manager: dependencies.GitManager,
		Publisher: config.Publisher, WorkerID: dependencies.WorkerID,
		LeaseDuration: config.PublishLeaseDuration, Now: dependencies.Now}
	if publisher.Validate() != nil {
		return nil, ErrInvalid
	}
	return &Runtime{Enabled: true, Config: config, Store: store, Releases: releases,
		Values: releases, Publications: publications, Worker: worker, Planner: planner, Publisher: publisher,
		workerID: dependencies.WorkerID, workerEpoch: dependencies.WorkerEpoch,
		startedAt: dependencies.StartedAt.UTC(), reportError: dependencies.ReportError,
		credentials: credentialReadiness}, nil
}

type protectedPublisherProcessor interface {
	Validate() error
	ProcessPayloadOne(context.Context) (ProtectedPayloadIntent, error)
	ProcessApplicationOne(context.Context) (ProtectedApplicationIntent, error)
}

func (r *Runtime) ObserveRendererReadiness(ctx context.Context) error {
	if r == nil || !r.Enabled || r.Store == nil || r.Config.Validate() != nil || ctx == nil {
		return ErrInvalid
	}
	if len(r.Config.OCICredentialProfiles) != 0 {
		if r.credentials == nil || r.credentials.Probe(ctx) != nil {
			return ErrOCICredentialUnavailable
		}
	}
	now := r.Worker.Now().UTC()
	readiness, err := r.Worker.Readiness(r.workerID, r.workerEpoch,
		r.startedAt, now, r.Config.ReadinessLeaseDuration)
	if err != nil {
		return err
	}
	return r.Store.PutReadiness(ctx, readiness)
}

func (r *Runtime) ObservePublisherReadiness(ctx context.Context) error {
	if r == nil || !r.Enabled || r.Publications == nil || r.Publisher == nil ||
		r.Publisher.Validate() != nil || r.Config.Validate() != nil || ctx == nil {
		return ErrInvalid
	}
	now := r.Worker.Now().UTC()
	readiness := ProtectedPublisherReadiness{WorkerID: r.workerID, WorkerEpoch: r.workerEpoch,
		Publisher: r.Config.Publisher, StartedAt: r.startedAt, ObservedAt: now,
		LeaseUntil: now.Add(r.Config.ReadinessLeaseDuration)}
	if readiness.Validate() != nil {
		return ErrInvalid
	}
	return r.Publications.PutPublisherReadiness(ctx, readiness)
}

func (r *Runtime) ProcessRenderOne(ctx context.Context) (RenderResult, error) {
	if r == nil || !r.Enabled {
		return RenderResult{}, ErrInvalid
	}
	return r.Worker.ProcessOne(ctx, r.workerID)
}

func (r *Runtime) ProcessPublicationOne(ctx context.Context) (PublicationPlanResult, error) {
	if r == nil || !r.Enabled {
		return PublicationPlanResult{}, ErrInvalid
	}
	return r.Planner.ProcessOne(ctx)
}

func (r *Runtime) ProcessProtectedPayloadOne(ctx context.Context) (ProtectedPayloadIntent, error) {
	if r == nil || !r.Enabled || r.Publisher == nil {
		return ProtectedPayloadIntent{}, ErrInvalid
	}
	return r.Publisher.ProcessPayloadOne(ctx)
}

func (r *Runtime) ProcessProtectedApplicationOne(ctx context.Context) (ProtectedApplicationIntent, error) {
	if r == nil || !r.Enabled || r.Publisher == nil {
		return ProtectedApplicationIntent{}, ErrInvalid
	}
	return r.Publisher.ProcessApplicationOne(ctx)
}

// Run keeps both exact readiness leases, rendering, planning, and the two
// protected publication phases in independent loops. Long bounded Git/Helm
// work therefore cannot let readiness expire. Iteration failures are reported
// and retried after the fixed poll interval; durable stores retain their own
// retry, recovery, and fencing semantics.
func (r *Runtime) Run(ctx context.Context) error {
	if r == nil || !r.Enabled || r.Config.Validate() != nil || r.Publisher == nil ||
		r.Publisher.Validate() != nil || r.reportError == nil || ctx == nil {
		return ErrInvalid
	}
	var wait sync.WaitGroup
	run := func(name string, interval time.Duration, operation func(context.Context) error) {
		defer wait.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			err := operation(ctx)
			if err != nil && !errors.Is(err, ErrNotFound) && ctx.Err() == nil {
				r.reportError(name, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}
	wait.Add(6)
	go run("renderer-readiness", r.Config.ReadinessLeaseDuration/3, r.ObserveRendererReadiness)
	go run("publisher-readiness", r.Config.ReadinessLeaseDuration/3, r.ObservePublisherReadiness)
	go run("render", r.Config.WorkPollInterval, func(loopContext context.Context) error {
		_, err := r.ProcessRenderOne(loopContext)
		return err
	})
	go run("publication-planner", r.Config.WorkPollInterval, func(loopContext context.Context) error {
		_, err := r.ProcessPublicationOne(loopContext)
		return err
	})
	go run("protected-application-publisher", r.Config.WorkPollInterval, func(loopContext context.Context) error {
		_, err := r.ProcessProtectedApplicationOne(loopContext)
		return err
	})
	go run("protected-payload-publisher", r.Config.WorkPollInterval, func(loopContext context.Context) error {
		_, err := r.ProcessProtectedPayloadOne(loopContext)
		return err
	})
	<-ctx.Done()
	wait.Wait()
	return nil
}

type APIRuntimeDependencies struct {
	Pool *pgxpool.Pool
	Argo ProtectedArgoReadiness
	Now  func() time.Time
}

type APIRuntime struct {
	Enabled  bool
	Config   RuntimeConfig
	Releases ReleaseService
	Values   ReleaseValuesService
	Gate     CapabilityGate
	now      func() time.Time
}

// NewAPIRuntime constructs only read/write services and readiness probes. It
// cannot fetch charts, create renderer Jobs, or plan Git mutations.
func NewAPIRuntime(config RuntimeConfig, dependencies APIRuntimeDependencies) (*APIRuntime, error) {
	if config.Validate() != nil {
		return nil, ErrInvalid
	}
	if !config.Enabled {
		return &APIRuntime{Config: config}, nil
	}
	if dependencies.Pool == nil || dependencies.Argo == nil || dependencies.Now == nil {
		return nil, ErrInvalid
	}
	store, err := NewPostgresStore(dependencies.Pool, config.Publisher.ConfigDigest)
	if err != nil {
		return nil, err
	}
	releases, err := NewPostgresReleaseService(dependencies.Pool, config.Publisher.ConfigDigest)
	if err != nil {
		return nil, err
	}
	publications, err := NewPostgresProtectedPublicationStore(dependencies.Pool)
	if err != nil {
		return nil, err
	}
	gate := CapabilityGate{Enabled: true, Renderer: store, Publisher: publications,
		Argo: dependencies.Argo, PublisherID: config.Publisher}
	return &APIRuntime{Enabled: true, Config: config, Releases: releases, Values: releases,
		Gate: gate, now: dependencies.Now}, nil
}

func (r *APIRuntime) Capabilities(ctx context.Context) (Capabilities, error) {
	if r == nil {
		return Capabilities{}, ErrInvalid
	}
	if !r.Enabled {
		return Capabilities{}, nil
	}
	return r.Gate.Evaluate(ctx, r.now().UTC())
}

func (r *APIRuntime) ApprovalCatalog(ctx context.Context, limit int) ([]ApprovalDocument, error) {
	if r == nil || !r.Enabled || r.Releases == nil {
		return nil, ErrUnavailable
	}
	return r.Releases.ApprovalCatalog(ctx, limit)
}

func (r *APIRuntime) ApprovalDocument(ctx context.Context, key ApprovalKey) (ApprovalDocument, error) {
	if r == nil || !r.Enabled || r.Values == nil {
		return ApprovalDocument{}, ErrUnavailable
	}
	return r.Values.ApprovalDocument(ctx, key)
}

func (r *APIRuntime) PreviewValues(ctx context.Context, target ReleaseTarget, key ApprovalKey, values []byte) (ValuesPreview, error) {
	if r == nil || !r.Enabled || r.Values == nil {
		return ValuesPreview{}, ErrUnavailable
	}
	return r.Values.PreviewValues(ctx, target, key, values)
}

func (r *APIRuntime) Upsert(ctx context.Context, request UpsertReleaseRequest, now time.Time) (ReleaseRevision, bool, error) {
	if r == nil || !r.Enabled || r.Releases == nil {
		return ReleaseRevision{}, false, ErrUnavailable
	}
	return r.Releases.Upsert(ctx, request, now)
}

func (r *APIRuntime) Retry(ctx context.Context, request RetryReleaseRequest, now time.Time) (ReleaseRevision, bool, error) {
	if r == nil || !r.Enabled || r.Releases == nil {
		return ReleaseRevision{}, false, ErrUnavailable
	}
	return r.Releases.Retry(ctx, request, now)
}

func (r *APIRuntime) Disable(ctx context.Context, request DisableReleaseRequest, now time.Time) (ReleaseRevision, bool, error) {
	if r == nil || !r.Enabled || r.Releases == nil {
		return ReleaseRevision{}, false, ErrUnavailable
	}
	return r.Releases.Disable(ctx, request, now)
}

func (r *APIRuntime) Rollback(ctx context.Context, request RollbackReleaseRequest, now time.Time) (ReleaseRevision, bool, error) {
	if r == nil || !r.Enabled || r.Releases == nil {
		return ReleaseRevision{}, false, ErrUnavailable
	}
	return r.Releases.Rollback(ctx, request, now)
}

func (r *APIRuntime) Head(ctx context.Context, target ReleaseTarget) (ReleaseStatus, error) {
	if r == nil || !r.Enabled || r.Releases == nil {
		return ReleaseStatus{}, ErrUnavailable
	}
	return r.Releases.Head(ctx, target)
}

func (r *APIRuntime) History(ctx context.Context, target ReleaseTarget, limit int) ([]ReleaseStatus, error) {
	if r == nil || !r.Enabled || r.Releases == nil {
		return nil, ErrUnavailable
	}
	return r.Releases.History(ctx, target, limit)
}

var _ interface {
	ReleaseService
	ReleaseValuesService
} = (*APIRuntime)(nil)
