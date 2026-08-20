package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/externaldns"
)

type externalDNSRuntimeStore interface {
	ListExternalDNSIntegrationsForRuntime(context.Context, int) ([]domain.ExternalDNSIntegration, error)
	AdvanceExternalDNSRuntimeRevision(context.Context, string, int64, string, time.Time) error
	RecordExternalDNSPublication(context.Context, string, int64, bool, string, string, time.Time) error
}
type externalDNSOperationalRuntime struct {
	source                externalDNSRuntimeStore
	edgeStore             edgeRuntimeStore
	observer              edge.TargetObserver
	publisher             *externaldns.ProtectedPublisher
	base                  edge.RuntimeConfig
	config                externaldns.OperationalConfig
	workerID              string
	startedAt             time.Time
	workerEpoch           int64
	readinessConfigDigest string
}

func newExternalDNSOperationalRuntimeWithDatabase(ctx context.Context, databaseURL, host string, source externalDNSRuntimeStore, base edge.RuntimeConfig, config externaldns.OperationalConfig, git *gitProjectionRuntime) (*externalDNSOperationalRuntime, error) {
	if !config.Enabled {
		return nil, nil
	}
	if config.Validate() != nil || source == nil || git == nil || git.store == nil || git.headVerifier.Client == nil || git.writeManager == nil {
		return nil, externaldns.ErrRuntimeUnavailable
	}
	store, err := edge.OpenPostgreSQLStore(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	reader, err := edge.NewInClusterKubernetesReader()
	if err != nil {
		store.Close()
		return nil, err
	}
	started := time.Now().UTC()
	owner, err := edgeWorkerIdentity(host, 1, started)
	if err != nil {
		store.Close()
		return nil, err
	}
	publisher, err := externaldns.NewProtectedPublisher(git.store, git.headVerifier, git.writeManager, externaldns.ProtectedGitConfig{BindingID: config.BindingID, ClusterID: config.ClusterID, Owner: owner, Template: config.Template}, func() time.Time { return time.Now().UTC() })
	if err != nil {
		store.Close()
		return nil, err
	}
	return &externalDNSOperationalRuntime{source: source, edgeStore: store, observer: &edge.KubernetesTargetObserver{Reader: reader, Resolver: net.DefaultResolver}, publisher: publisher, base: base, config: config, workerID: owner, startedAt: started, workerEpoch: 1}, nil
}

func (r *externalDNSOperationalRuntime) desired(ctx context.Context) (edge.RuntimeConfig, error) {
	items, err := r.source.ListExternalDNSIntegrationsForRuntime(ctx, edge.MaximumExternalDNSProfiles)
	if err != nil {
		return edge.RuntimeConfig{}, err
	}
	profiles := make([]edge.ExternalDNSProfile, 0, len(items))
	static := map[string]edge.ExternalDNSProfile{}
	for _, profile := range r.base.Profiles.ExternalDNS {
		static[profile.IntegrationID] = profile
	}
	for _, item := range items {
		if item.Mode == externaldns.ModeManaged {
			if externalDNSRuntimeRevisionAdvanceNeeded(item, r.config.Template) {
				if err = r.source.AdvanceExternalDNSRuntimeRevision(ctx, item.ID, item.RuntimeRevision, item.ProtectedGitContentDigest, time.Now().UTC()); err != nil {
					return edge.RuntimeConfig{}, err
				}
				item.RuntimeRevision++
				item.ProtectedGitState, item.ProtectedGitRevision, item.ProtectedGitContentDigest, item.ProtectedGitCommit, item.ProtectedGitObservedAt = "pending", 0, "", "", nil
			}
			var profile edge.ExternalDNSProfile
			if item.Lifecycle == "active" {
				profile, err = externaldns.ManagedProfile(item, r.config.Template)
				if err != nil {
					return edge.RuntimeConfig{}, err
				}
			}
			if externalDNSPublicationNeeded(item, r.config.Template) {
				receipt, publishErr := r.publisher.Reconcile(ctx, item)
				if publishErr != nil {
					err = publishErr
					return edge.RuntimeConfig{}, err
				}
				if err = r.source.RecordExternalDNSPublication(ctx, item.ID, item.RuntimeRevision, receipt.Deleted, receipt.ContentDigest, receipt.CommittedRevision, time.Now().UTC()); err != nil {
					return edge.RuntimeConfig{}, err
				}
				item.ProtectedGitState = "materialized"
				if receipt.Deleted {
					item.ProtectedGitState = "dematerialized"
				}
			}
			if item.Lifecycle == "active" {
				profiles = append(profiles, profile)
			}
		} else if item.Lifecycle == "active" {
			profile, ok := static[item.ID]
			if !ok || profile.Revision != item.RuntimeRevision {
				return edge.RuntimeConfig{}, externaldns.ErrRuntimeUnavailable
			}
			profiles = append(profiles, profile)
		}
	}
	sortExternalDNSProfiles(profiles)
	runtime := r.base
	runtime.Profiles.ExternalDNS = profiles
	runtime.Enabled = true
	return runtime, runtime.Validate()
}

func sortExternalDNSProfiles(profiles []edge.ExternalDNSProfile) {
	slices.SortFunc(profiles, func(left, right edge.ExternalDNSProfile) int {
		return strings.Compare(left.IntegrationID, right.IntegrationID)
	})
}

func externalDNSPublicationNeeded(item domain.ExternalDNSIntegration, template externaldns.ManagedRuntimeTemplate) bool {
	if item.Lifecycle != "active" || item.ProtectedGitState != "materialized" || item.ProtectedGitRevision != item.RuntimeRevision || item.ProtectedGitCommit == "" {
		return true
	}
	contentDigest, err := externaldns.ManagedBundleDigest(item, template)
	return err != nil || contentDigest != item.ProtectedGitContentDigest
}

func externalDNSRuntimeRevisionAdvanceNeeded(item domain.ExternalDNSIntegration, template externaldns.ManagedRuntimeTemplate) bool {
	if item.Mode != externaldns.ModeManaged || item.Lifecycle != "active" || item.ProtectedGitState != "materialized" ||
		item.ProtectedGitRevision != item.RuntimeRevision || item.ProtectedGitCommit == "" || item.ProtectedGitContentDigest == "" {
		return false
	}
	contentDigest, err := externaldns.ManagedBundleDigest(item, template)
	return err == nil && contentDigest != item.ProtectedGitContentDigest
}

func (r *externalDNSOperationalRuntime) Run(ctx context.Context) error {
	if r == nil {
		return externaldns.ErrRuntimeUnavailable
	}
	ticker := time.NewTicker(r.config.PollInterval)
	defer ticker.Stop()
	for {
		runtime, err := r.desired(ctx)
		if err == nil {
			digest, digestErr := runtime.Digest()
			targets, targetErr := runtime.DesiredTargets()
			now := time.Now().UTC()
			if digestErr == nil && targetErr == nil {
				revisionAdvanced := false
				readinessEpoch := r.nextReadinessEpoch(digest)
				err = r.edgeStore.SynchronizeTargets(ctx, digest, targets, now)
				if errors.Is(err, edge.ErrConflict) {
					var advanceErr error
					revisionAdvanced, advanceErr = r.advanceConflictingExternalDNSRuntimeRevisions(ctx, targets)
					if advanceErr != nil {
						err = advanceErr
					} else if revisionAdvanced {
						err = nil
					}
				}
				if err == nil && !revisionAdvanced {
					err = r.edgeStore.RecordReadiness(ctx, edge.Readiness{WorkerID: r.workerID, WorkerEpoch: readinessEpoch, Contract: edge.RuntimeContract, ConfigDigest: digest, TargetCount: len(targets), StartedAt: r.startedAt, ObservedAt: now, LeaseUntil: now.Add(runtime.ReadinessMaxAge)})
					if err == nil {
						r.workerEpoch, r.readinessConfigDigest = readinessEpoch, digest
					}
				}
				controller := &edge.RuntimeController{Store: r.edgeStore, Observer: r.observer, Config: runtime, WorkerID: r.workerID, WorkerEpoch: readinessEpoch, Now: func() time.Time { return time.Now().UTC() }}
				for i := 0; err == nil && !revisionAdvanced && i < len(targets); i++ {
					_, err = controller.Reconcile(ctx, digest)
				}
			}
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("dynamic external-dns reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *externalDNSOperationalRuntime) nextReadinessEpoch(configDigest string) int64 {
	epoch := r.workerEpoch
	if epoch < 1 {
		epoch = 1
	}
	if r.readinessConfigDigest != "" && r.readinessConfigDigest != configDigest {
		epoch++
	}
	return epoch
}

func (r *externalDNSOperationalRuntime) advanceConflictingExternalDNSRuntimeRevisions(ctx context.Context, targets []edge.DesiredTarget) (bool, error) {
	items, err := r.source.ListExternalDNSIntegrationsForRuntime(ctx, edge.MaximumExternalDNSProfiles)
	if err != nil {
		return false, err
	}
	byID := make(map[string]domain.ExternalDNSIntegration, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	advanced := false
	for _, desired := range targets {
		if desired.Kind != edge.KindExternalDNS {
			continue
		}
		current, targetErr := r.edgeStore.Target(ctx, desired.Key, desired.Revision)
		if errors.Is(targetErr, edge.ErrNotFound) {
			continue
		}
		if targetErr != nil {
			return false, targetErr
		}
		item, ok := byID[desired.IntegrationID]
		if !ok || !externalDNSTargetRevisionAdvanceNeeded(item, current.DesiredTarget, desired) {
			continue
		}
		if err = r.source.AdvanceExternalDNSRuntimeRevision(ctx, item.ID, item.RuntimeRevision, item.ProtectedGitContentDigest, time.Now().UTC()); err != nil {
			return false, err
		}
		advanced = true
	}
	return advanced, nil
}

func externalDNSTargetRevisionAdvanceNeeded(item domain.ExternalDNSIntegration, current, desired edge.DesiredTarget) bool {
	if item.Mode != externaldns.ModeManaged || item.Lifecycle != "active" || item.ProtectedGitState != "materialized" ||
		item.ProtectedGitRevision != item.RuntimeRevision || item.ProtectedGitCommit == "" || item.ProtectedGitContentDigest == "" ||
		desired.Kind != edge.KindExternalDNS || desired.IntegrationID != item.ID || desired.Revision != item.RuntimeRevision ||
		current.Kind != edge.KindExternalDNS || current.IntegrationID != item.ID || current.Revision != item.RuntimeRevision {
		return false
	}
	current.RuntimeConfigDigest = desired.RuntimeConfigDigest
	return current != desired
}

func (r *externalDNSOperationalRuntime) Close() {
	if r != nil && r.edgeStore != nil {
		r.edgeStore.Close()
	}
}
