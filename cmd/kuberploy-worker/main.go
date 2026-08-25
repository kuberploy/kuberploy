package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/builds"
	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/certissuers"
	"github.com/kuberploy/kuberploy/internal/config"
	"github.com/kuberploy/kuberploy/internal/edge"
	"github.com/kuberploy/kuberploy/internal/environmentfoundation"
	"github.com/kuberploy/kuberploy/internal/externaldns"
	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/imagepull"
	"github.com/kuberploy/kuberploy/internal/queue"
	"github.com/kuberploy/kuberploy/internal/registry"
	"github.com/kuberploy/kuberploy/internal/secrets"
	"github.com/kuberploy/kuberploy/internal/store/postgres"
	"github.com/kuberploy/kuberploy/internal/valkeystartup"
	"github.com/kuberploy/kuberploy/internal/worker"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "registry-maintenance-helper" {
		if err := registry.RunMaintenanceHelper(context.Background()); err != nil {
			slog.Error("registry maintenance helper failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "outbox-relay-once" {
		if err := runOutboxRelayOnce(context.Background(), os.Stdout); err != nil {
			slog.Error("outbox relay failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := config.ValidateControlPlaneEgressFromEnvironment(); err != nil {
		return err
	}
	databaseURL, err := config.Required("KUBERPLOY_DATABASE_URL")
	if err != nil {
		return err
	}
	gitRoot, err := config.Required("KUBERPLOY_GIT_WORKTREE")
	if err != nil {
		return err
	}
	sourceBuildConfig, err := builds.WorkerRuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	gitProjectionConfig, err := gitprojection.RuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	runtimeSecretConfig, err := secrets.RuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	certificateObservationConfig, err := certificates.ObservationConfigFromEnvironment(runtimeSecretConfig)
	if err != nil {
		return err
	}
	if certificateObservationConfig.Enabled && !gitProjectionConfig.Enabled {
		return gitprojection.ErrInvalid
	}
	runtimeRegistryPullConfig, err := imagepull.RuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	edgeRuntimeConfig, err := edge.RuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	externalDNSOperationalConfig, err := externaldns.OperationalConfigFromEnvironment()
	if err != nil {
		return err
	}
	certificateIssuerConfig, err := certissuers.ObserverConfigFromEnvironment()
	if err != nil {
		return err
	}
	argoObservationConfig, err := argo.ObservationRuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	argoDesiredStateConfig, err := argo.ProductionRuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	foundationConfig, err := environmentfoundation.RuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	if argoDesiredStateConfig.Enabled && (!foundationConfig.Enabled || !gitProjectionConfig.Enabled || !argoObservationConfig.Enabled ||
		argoDesiredStateConfig.DesiredState.ArgoNamespace != argoObservationConfig.Namespace) {
		return argo.ErrInvalid
	}
	if foundationConfig.Enabled && (!argoDesiredStateConfig.Enabled || !gitProjectionConfig.Enabled ||
		foundationConfig.PlatformBindingID != argoDesiredStateConfig.DesiredState.PlatformBindingID) {
		return environmentfoundation.ErrInvalid
	}
	if certificateIssuerConfig.Enabled && (!gitProjectionConfig.Enabled || !argoDesiredStateConfig.Enabled || !foundationConfig.Enabled ||
		!edgeRuntimeConfig.Enabled || edgeRuntimeConfig.Profiles.CertManager == nil ||
		certificateIssuerConfig.BindingID != argoDesiredStateConfig.DesiredState.PlatformBindingID ||
		certificateIssuerConfig.BindingID != foundationConfig.PlatformBindingID) {
		return certissuers.ErrObservationUnavailable
	}
	if externalDNSOperationalConfig.Enabled && (!gitProjectionConfig.Enabled || !argoDesiredStateConfig.Enabled || !foundationConfig.Enabled || !edgeRuntimeConfig.Enabled ||
		externalDNSOperationalConfig.BindingID != argoDesiredStateConfig.DesiredState.PlatformBindingID ||
		externalDNSOperationalConfig.BindingID != foundationConfig.PlatformBindingID) {
		return externaldns.ErrRuntimeUnavailable
	}
	managedRegistryConfig, err := registry.RuntimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	db, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	valkeyAddresses := config.List("KUBERPLOY_VALKEY_ADDRESSES", "127.0.0.1:6379")
	valkeyUsername := os.Getenv("KUBERPLOY_VALKEY_USERNAME")
	valkeyPassword := os.Getenv("KUBERPLOY_VALKEY_PASSWORD")
	consumerStream, err := valkeystartup.Open(ctx, func() (*queue.ValkeyStream, error) {
		return queue.NewValkeyStream(queue.ValkeyOptions{Addresses: valkeyAddresses,
			Username: config.Get("KUBERPLOY_VALKEY_CONSUMER_USERNAME", valkeyUsername), Password: valkeyCredential("KUBERPLOY_VALKEY_CONSUMER_PASSWORD", valkeyPassword),
			ClientName: "kuberploy-worker-consumer"})
	})
	if err != nil {
		return err
	}
	defer consumerStream.Close()
	publisherStream, err := valkeystartup.Open(ctx, func() (*queue.ValkeyStream, error) {
		return queue.NewValkeyStream(queue.ValkeyOptions{Addresses: valkeyAddresses,
			Username: config.Get("KUBERPLOY_VALKEY_PUBLISHER_USERNAME", valkeyUsername), Password: valkeyCredential("KUBERPLOY_VALKEY_PUBLISHER_PASSWORD", valkeyPassword),
			ClientName: "kuberploy-outbox-publisher"})
	})
	if err != nil {
		return err
	}
	defer publisherStream.Close()
	writer := &gitops.Writer{Root: gitRoot, Remote: stringsTrim(os.Getenv("KUBERPLOY_GIT_REMOTE")), Branch: config.Get("KUBERPLOY_GIT_BRANCH", "main"), DefaultIngressClass: config.Get("KUBERPLOY_INGRESS_CLASS", "traefik")}
	switch authMode := config.Get("KUBERPLOY_GIT_AUTH_MODE", "none"); authMode {
	case "none":
	case "secret":
		writer.UseCredentialFiles = true
	default:
		return fmt.Errorf("unsupported KUBERPLOY_GIT_AUTH_MODE %q", authMode)
	}
	host, _ := os.Hostname()
	sourceBuilds, err := newSourceBuildRuntime(ctx, databaseURL, host, sourceBuildConfig, db)
	if err != nil {
		return err
	}
	certificateObservation, err := newCertificateObservationRuntime(ctx, databaseURL, host, certificateObservationConfig)
	if err != nil {
		if sourceBuilds != nil {
			sourceBuilds.Close()
		}
		return err
	}
	var certificateResolver *certificates.PostgreSQLReferenceResolver
	if certificateObservation != nil {
		certificateResolver = certificateObservation.resolver
	}
	certificateIssuerStore, err := openCertificateIssuerWorkerStore(ctx, databaseURL, certificateIssuerConfig)
	if err != nil {
		if certificateObservation != nil {
			certificateObservation.Close()
		}
		if sourceBuilds != nil {
			sourceBuilds.Close()
		}
		return err
	}
	certificateIssuerRuntimeManaged := false
	defer func() {
		if !certificateIssuerRuntimeManaged && certificateIssuerStore != nil {
			certificateIssuerStore.Close()
		}
	}()
	gitProjection, err := newGitProjectionRuntime(ctx, databaseURL, host, gitProjectionConfig, runtimeSecretConfig, certificateObservationConfig, certificateIssuerConfig, runtimeRegistryPullConfig, edgeRuntimeConfig, externalDNSOperationalConfig, certificateResolver, certificateIssuerStore)
	if err != nil {
		if certificateObservation != nil {
			certificateObservation.Close()
		}
		if sourceBuilds != nil {
			sourceBuilds.Close()
		}
		return err
	}
	certificateIssuers, err := newCertificateIssuerRuntime(certificateIssuerConfig, host, certificateIssuerStore, gitProjection)
	if err != nil {
		if gitProjection != nil {
			gitProjection.Close()
		}
		if certificateObservation != nil {
			certificateObservation.Close()
		}
		if sourceBuilds != nil {
			sourceBuilds.Close()
		}
		return err
	}
	argoObservation, err := newArgoObservationRuntime(ctx, databaseURL, host, argoObservationConfig)
	if err != nil {
		if gitProjection != nil {
			gitProjection.Close()
		}
		if certificateObservation != nil {
			certificateObservation.Close()
		}
		if sourceBuilds != nil {
			sourceBuilds.Close()
		}
		return err
	}
	runtimeSecrets, err := newRuntimeSecretRuntime(ctx, databaseURL, host, runtimeSecretConfig)
	if err != nil {
		if argoObservation != nil {
			argoObservation.Close()
		}
		if gitProjection != nil {
			gitProjection.Close()
		}
		if certificateObservation != nil {
			certificateObservation.Close()
		}
		if sourceBuilds != nil {
			sourceBuilds.Close()
		}
		return err
	}
	runtimeRegistryPulls, err := newRuntimeRegistryPullRuntime(ctx, databaseURL, host, runtimeRegistryPullConfig, gitProjection)
	if err != nil {
		if runtimeSecrets != nil {
			runtimeSecrets.Close()
		}
		if argoObservation != nil {
			argoObservation.Close()
		}
		if gitProjection != nil {
			gitProjection.Close()
		}
		if certificateObservation != nil {
			certificateObservation.Close()
		}
		if sourceBuilds != nil {
			sourceBuilds.Close()
		}
		return err
	}
	managedRegistry, err := newManagedRegistryRuntime(ctx, host, managedRegistryConfig, db)
	if err != nil {
		if runtimeRegistryPulls != nil {
			runtimeRegistryPulls.Close()
		}
		if runtimeSecrets != nil {
			runtimeSecrets.Close()
		}
		if argoObservation != nil {
			argoObservation.Close()
		}
		if gitProjection != nil {
			gitProjection.Close()
		}
		if certificateObservation != nil {
			certificateObservation.Close()
		}
		if sourceBuilds != nil {
			sourceBuilds.Close()
		}
		return err
	}
	var edgeManagedRuntime *edgeRuntime
	var externalDNSOperational *externalDNSOperationalRuntime
	if externalDNSOperationalConfig.Enabled {
		externalDNSOperational, err = newExternalDNSOperationalRuntimeWithDatabase(ctx, databaseURL, host, db, edgeRuntimeConfig, externalDNSOperationalConfig, gitProjection)
	} else {
		edgeManagedRuntime, err = newEdgeRuntime(ctx, databaseURL, host, edgeRuntimeConfig)
	}
	if err != nil {
		if managedRegistry != nil {
			managedRegistry.Close()
		}
		if runtimeRegistryPulls != nil {
			runtimeRegistryPulls.Close()
		}
		if runtimeSecrets != nil {
			runtimeSecrets.Close()
		}
		if argoObservation != nil {
			argoObservation.Close()
		}
		if gitProjection != nil {
			gitProjection.Close()
		}
		if certificateObservation != nil {
			certificateObservation.Close()
		}
		if sourceBuilds != nil {
			sourceBuilds.Close()
		}
		return err
	}
	foundationRuntime, err := newEnvironmentFoundationRuntime(ctx, databaseURL, host, foundationConfig, gitProjection)
	if err != nil {
		if edgeManagedRuntime != nil {
			edgeManagedRuntime.Close()
		}
		if externalDNSOperational != nil {
			externalDNSOperational.Close()
		}
		if managedRegistry != nil {
			managedRegistry.Close()
		}
		if runtimeRegistryPulls != nil {
			runtimeRegistryPulls.Close()
		}
		if runtimeSecrets != nil {
			runtimeSecrets.Close()
		}
		if argoObservation != nil {
			argoObservation.Close()
		}
		if gitProjection != nil {
			gitProjection.Close()
		}
		if certificateObservation != nil {
			certificateObservation.Close()
		}
		if sourceBuilds != nil {
			sourceBuilds.Close()
		}
		return err
	}
	var foundationReadiness argo.FoundationReadinessProbe
	if foundationRuntime != nil {
		foundationReadiness = foundationRuntime.readiness
	}
	argoDesiredState, err := newArgoDesiredStateRuntime(ctx, databaseURL, host, argoDesiredStateConfig, runtimeRegistryPullConfig, gitProjection, foundationReadiness, argoObservation)
	if err != nil {
		if foundationRuntime != nil {
			foundationRuntime.Close()
		}
		if edgeManagedRuntime != nil {
			edgeManagedRuntime.Close()
		}
		if externalDNSOperational != nil {
			externalDNSOperational.Close()
		}
		if managedRegistry != nil {
			managedRegistry.Close()
		}
		if runtimeRegistryPulls != nil {
			runtimeRegistryPulls.Close()
		}
		if runtimeSecrets != nil {
			runtimeSecrets.Close()
		}
		if argoObservation != nil {
			argoObservation.Close()
		}
		if gitProjection != nil {
			gitProjection.Close()
		}
		if certificateObservation != nil {
			certificateObservation.Close()
		}
		if sourceBuilds != nil {
			sourceBuilds.Close()
		}
		return err
	}
	var operationWriter worker.GitWriter = legacyGitOperationWriter{writer: writer}
	if gitProjection != nil {
		operationWriter = gitProjection.writer
	}
	processor := &worker.Processor{Store: db, Queue: consumerStream, Writer: operationWriter, Name: "worker-" + host, Batch: 10}
	relay := &queue.Relay{Store: db, Publisher: publisherStream, Batch: 100}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	type managedRuntime interface {
		Run(context.Context) error
		Close()
	}
	type runtimeResult struct {
		name string
		err  error
	}
	runtimes := make([]managedRuntime, 0, 11)
	if sourceBuilds != nil {
		runtimes = append(runtimes, sourceBuilds)
	}
	if gitProjection != nil {
		runtimes = append(runtimes, gitProjection)
	}
	if certificateIssuers != nil {
		runtimes = append(runtimes, certificateIssuers)
		certificateIssuerRuntimeManaged = true
	}
	if foundationRuntime != nil {
		runtimes = append(runtimes, foundationRuntime)
	}
	if argoDesiredState != nil {
		runtimes = append(runtimes, argoDesiredState)
	}
	if certificateObservation != nil {
		runtimes = append(runtimes, certificateObservation)
	}
	if argoObservation != nil {
		runtimes = append(runtimes, argoObservation)
	}
	if runtimeSecrets != nil {
		runtimes = append(runtimes, runtimeSecrets)
	}
	if runtimeRegistryPulls != nil {
		runtimes = append(runtimes, runtimeRegistryPulls)
	}
	if managedRegistry != nil {
		runtimes = append(runtimes, managedRegistry)
	}
	if edgeManagedRuntime != nil {
		runtimes = append(runtimes, edgeManagedRuntime)
	}
	if externalDNSOperational != nil {
		runtimes = append(runtimes, externalDNSOperational)
	}
	runtimeContext, cancelRuntimes := context.WithCancel(ctx)
	runtimeDone := make(chan runtimeResult, len(runtimes))
	for _, runtime := range runtimes {
		name := "source-builds"
		if _, isProjection := runtime.(*gitProjectionRuntime); isProjection {
			name = "git-projection"
		}
		if _, isCertificateObservation := runtime.(*certificateObservationRuntime); isCertificateObservation {
			name = "certificate-observation"
		}
		if _, isCertificateIssuers := runtime.(*certificateIssuerRuntime); isCertificateIssuers {
			name = "certificate-issuers"
		}
		if _, isArgo := runtime.(*argoObservationRuntime); isArgo {
			name = "argo-observation"
		}
		if _, isArgoDesiredState := runtime.(*argoDesiredStateRuntime); isArgoDesiredState {
			name = "argo-desired-state"
		}
		if _, isFoundation := runtime.(*environmentFoundationRuntime); isFoundation {
			name = "environment-foundation"
		}
		if _, isRegistry := runtime.(*managedRegistryRuntime); isRegistry {
			name = "managed-registry"
		}
		if _, isRuntimeSecrets := runtime.(*runtimeSecretRuntime); isRuntimeSecrets {
			name = "runtime-secrets"
		}
		if _, isRegistryPulls := runtime.(*runtimeRegistryPullRuntime); isRegistryPulls {
			name = "runtime-registry-pulls"
		}
		if _, isEdge := runtime.(*edgeRuntime); isEdge {
			name = "edge-runtime"
		}
		if _, isDynamicExternalDNS := runtime.(*externalDNSOperationalRuntime); isDynamicExternalDNS {
			name = "external-dns-operational"
		}
		go func(name string, runtime managedRuntime) {
			runtimeDone <- runtimeResult{name: name, err: runtime.Run(runtimeContext)}
		}(name, runtime)
	}
	completedRuntimes := 0
	defer func() {
		cancelRuntimes()
		for completedRuntimes < len(runtimes) {
			<-runtimeDone
			completedRuntimes++
		}
		for index := len(runtimes) - 1; index >= 0; index-- {
			runtimes[index].Close()
		}
	}()
	for {
		if n, relayErr := relay.RunOnce(ctx); relayErr != nil {
			slog.Warn("Valkey relay unavailable; PostgreSQL fallback remains active", "error", relayErr)
		} else if n > 0 {
			slog.Info("published outbox work", "count", n)
		}
		if n, workErr := processor.RunOnce(ctx); workErr != nil {
			slog.Warn("worker iteration failed", "error", workErr)
		} else if n > 0 {
			slog.Info("processed Git operations", "count", n)
		}
		select {
		case <-ctx.Done():
			return nil
		case result := <-runtimeDone:
			completedRuntimes++
			if ctx.Err() != nil && (result.err == nil || errors.Is(result.err, context.Canceled)) {
				return nil
			}
			if result.err != nil {
				return fmt.Errorf("%s runtime: %w", result.name, result.err)
			}
			return fmt.Errorf("%s runtime stopped unexpectedly", result.name)
		case <-ticker.C:
		}
	}
}
func stringsTrim(v string) string {
	for len(v) > 0 && (v[0] == ' ' || v[0] == '\t' || v[0] == '\n') {
		v = v[1:]
	}
	for len(v) > 0 && (v[len(v)-1] == ' ' || v[len(v)-1] == '\t' || v[len(v)-1] == '\n') {
		v = v[:len(v)-1]
	}
	return v
}

func valkeyCredential(name, fallback string) string {
	if value, present := os.LookupEnv(name); present && value != "" {
		return value
	}
	return fallback
}
