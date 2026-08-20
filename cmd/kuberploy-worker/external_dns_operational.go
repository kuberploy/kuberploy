package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/externaldns"
)

type externalDNSRuntimeStore interface {
	ListExternalDNSIntegrationsForRuntime(context.Context, int) ([]domain.ExternalDNSIntegration, error)
	RecordExternalDNSPublication(context.Context, string, int64, bool, string, string, time.Time) error
}
type externalDNSOperationalRuntime struct {
	source    externalDNSRuntimeStore
	edgeStore edgeRuntimeStore
	observer  edge.TargetObserver
	publisher *externaldns.ProtectedPublisher
	base      edge.RuntimeConfig
	config    externaldns.OperationalConfig
	workerID  string
	startedAt time.Time
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
	return &externalDNSOperationalRuntime{source: source, edgeStore: store, observer: &edge.KubernetesTargetObserver{Reader: reader, Resolver: net.DefaultResolver}, publisher: publisher, base: base, config: config, workerID: owner, startedAt: started}, nil
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
	runtime := r.base
	runtime.Profiles.ExternalDNS = profiles
	runtime.Enabled = true
	return runtime, runtime.Validate()
}

func externalDNSPublicationNeeded(item domain.ExternalDNSIntegration, template externaldns.ManagedRuntimeTemplate) bool {
	if item.Lifecycle != "active" || item.ProtectedGitState != "materialized" || item.ProtectedGitRevision != item.RuntimeRevision || item.ProtectedGitCommit == "" {
		return true
	}
	contentDigest, err := externaldns.ManagedBundleDigest(item, template)
	return err != nil || contentDigest != item.ProtectedGitContentDigest
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
				err = r.edgeStore.SynchronizeTargets(ctx, digest, targets, now)
				if err == nil {
					err = r.edgeStore.RecordReadiness(ctx, edge.Readiness{WorkerID: r.workerID, WorkerEpoch: 1, Contract: edge.RuntimeContract, ConfigDigest: digest, TargetCount: len(targets), StartedAt: r.startedAt, ObservedAt: now, LeaseUntil: now.Add(runtime.ReadinessMaxAge)})
				}
				controller := &edge.RuntimeController{Store: r.edgeStore, Observer: r.observer, Config: runtime, WorkerID: r.workerID, WorkerEpoch: 1, Now: func() time.Time { return time.Now().UTC() }}
				for i := 0; err == nil && i < len(targets); i++ {
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
func (r *externalDNSOperationalRuntime) Close() {
	if r != nil && r.edgeStore != nil {
		r.edgeStore.Close()
	}
}
