package builds

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitssh"
)

type WorkloadState string

const (
	WorkloadPending   WorkloadState = "pending"
	WorkloadRunning   WorkloadState = "running"
	WorkloadSucceeded WorkloadState = "succeeded"
	WorkloadFailed    WorkloadState = "failed"
)

type BuildWorkload struct {
	Attempt          BuildAttempt
	Plan             builder.JobPlan
	CheckoutRequest  builder.CheckoutRequest
	InputDigest      string
	SourceUsername   string
	SourceCredential githubapp.Credential
	SSHPrivateKey    []byte
	SSHKnownHosts    []byte
}

// WorkloadObservation must include the live Job and NetworkPolicy for exact
// adoption. InputDigest binds the request ConfigMap and projected Secret names
// to the immutable persisted input without returning any Secret data.
type WorkloadObservation struct {
	State         WorkloadState
	Job           map[string]any
	NetworkPolicy map[string]any
	InputDigest   string
	Result        []byte
	LogReference  string
	FailureCode   string
}

type KubernetesBuildAPI interface {
	Ensure(context.Context, BuildWorkload) (WorkloadObservation, error)
	Cancel(context.Context, BuildAttempt) error
	PromoteCache(context.Context, BuildAttempt, string) (bool, error)
}

type BuildController struct {
	Store    Store
	Provider GitHubProvider
	GitSSH   interface {
		Active(context.Context, gitssh.Scope, string) (gitssh.KeyMetadata, error)
		PrivateKey(context.Context, gitssh.Scope, string) ([]byte, error)
	}
	Kubernetes    KubernetesBuildAPI
	Owner         string
	LeaseDuration time.Duration
	Now           func() time.Time
}

type ReconcileResult struct {
	AttemptID string
	State     AttemptState
	RetryAt   time.Time
}

func (c *BuildController) ReconcileNext(ctx context.Context) (ReconcileResult, error) {
	if c == nil || c.Store == nil || c.Kubernetes == nil || !validOwnerLease(c.Owner, c.LeaseDuration) {
		return ReconcileResult{}, ErrInvalid
	}
	now := c.now()
	attempt, err := c.Store.ClaimNextAttempt(ctx, c.Owner, now, c.LeaseDuration)
	if err != nil {
		return ReconcileResult{}, err
	}
	result := ReconcileResult{AttemptID: attempt.ID, State: attempt.State}
	if attempt.State == AttemptCancelling {
		if cancelErr := c.Kubernetes.Cancel(ctx, attempt); cancelErr != nil {
			return c.retryInfrastructure(ctx, attempt, "kubernetes-cancel-failed", cancelErr)
		}
		if err = c.Store.HeartbeatAttempt(ctx, attempt.ID, c.Owner, c.now(), c.LeaseDuration); err != nil {
			return result, err
		}
		if err = c.Store.CompleteCancellation(ctx, attempt.ID, c.Owner, c.now()); err != nil {
			return result, err
		}
		result.State = AttemptCancelled
		return result, nil
	}
	definition, err := c.Store.Definition(ctx, attempt.DefinitionID)
	if err != nil || !definition.Enabled || definition.DefinitionDigest != attempt.DefinitionDigest {
		_ = c.Store.FailAttempt(ctx, attempt.ID, c.Owner, "source-authorization-revoked", c.now())
		return result, ErrUnauthorized
	}
	if err = c.Store.HeartbeatAttempt(ctx, attempt.ID, c.Owner, c.now(), c.LeaseDuration); err != nil {
		return result, err
	}
	plan, err := builder.PlanJob(attempt.PlanRequest)
	if err != nil {
		_ = c.Store.FailAttempt(ctx, attempt.ID, c.Owner, "persisted-plan-invalid", c.now())
		return result, err
	}
	workload := BuildWorkload{Attempt: attempt, Plan: plan, CheckoutRequest: attempt.CheckoutRequest, InputDigest: attempt.InputDigest}
	if definition.SourceKind == SourceGitHub {
		if c.Provider == nil {
			_ = c.Store.FailAttempt(ctx, attempt.ID, c.Owner, "github-provider-unavailable", c.now())
			return result, ErrInfrastructure
		}
		installation, repository, authorizationErr := c.Store.AttemptAuthorization(ctx, attempt.ID)
		if authorizationErr != nil {
			_ = c.Store.FailAttempt(ctx, attempt.ID, c.Owner, "github-authorization-revoked", c.now())
			return result, authorizationErr
		}
		required := githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}
		if _, err = c.Provider.VerifyInstallation(ctx, installation.GitHubInstallationID, installation.Account, required); err != nil {
			if _, retryable := providerRetryAt(err, c.now()); retryable {
				return c.deferInfrastructure(ctx, attempt, "github-provider-retry", ErrProviderRetry)
			}
			_ = c.Store.FailAttempt(ctx, attempt.ID, c.Owner, "github-authorization-revoked", c.now())
			return result, err
		}
		if err = c.Store.HeartbeatAttempt(ctx, attempt.ID, c.Owner, c.now(), c.LeaseDuration); err != nil {
			return result, err
		}
		token, tokenErr := c.Provider.MintInstallationToken(ctx, githubapp.TokenRequest{
			InstallationID: installation.GitHubInstallationID, Account: installation.Account,
			Repositories: []githubapp.RepositoryIdentity{repository.Identity}, Permissions: required,
		})
		if tokenErr != nil {
			if _, retryable := providerRetryAt(tokenErr, c.now()); retryable {
				return c.deferInfrastructure(ctx, attempt, "github-provider-retry", ErrProviderRetry)
			}
			_ = c.Store.FailAttempt(ctx, attempt.ID, c.Owner, "github-authorization-revoked", c.now())
			return result, tokenErr
		}
		workload.SourceUsername, workload.SourceCredential = "x-access-token", token.Authorization()
	} else {
		if c.GitSSH == nil || definition.GitSSH == nil {
			_ = c.Store.FailAttempt(ctx, attempt.ID, c.Owner, "git-ssh-credential-unavailable", c.now())
			return result, ErrInfrastructure
		}
		scope := gitssh.Scope(definition.GitSSH.KeyScope)
		metadata, metadataErr := c.GitSSH.Active(ctx, scope, definition.GitSSH.KeyOwnerID)
		if metadataErr != nil || metadata.Revision != definition.GitSSH.KeyRevision || metadata.Status != gitssh.StatusActive {
			_ = c.Store.FailAttempt(ctx, attempt.ID, c.Owner, "git-ssh-key-revoked", c.now())
			return result, ErrUnauthorized
		}
		workload.SSHPrivateKey, err = c.GitSSH.PrivateKey(ctx, scope, definition.GitSSH.KeyOwnerID)
		if err != nil {
			return c.retryInfrastructure(ctx, attempt, "git-ssh-key-decrypt-failed", err)
		}
		defer clear(workload.SSHPrivateKey)
		workload.SSHKnownHosts = []byte(definition.GitSSH.KnownHosts)
	}
	if err = c.Store.HeartbeatAttempt(ctx, attempt.ID, c.Owner, c.now(), c.LeaseDuration); err != nil {
		return result, err
	}
	observation, err := c.Kubernetes.Ensure(ctx, workload)
	if err != nil {
		return c.retryInfrastructure(ctx, attempt, "kubernetes-ensure-failed", err)
	}
	// Ensure may perform a Kubernetes round trip. A stale worker must stop
	// before adopting or publishing anything if that round trip outlived its
	// durable lease.
	if err = c.Store.HeartbeatAttempt(ctx, attempt.ID, c.Owner, c.now(), c.LeaseDuration); err != nil {
		return result, err
	}
	if observation.InputDigest != attempt.InputDigest || !builder.CanAdoptJob(observation.Job, attempt.PlanRequest) || !builder.CanAdoptNetworkPolicy(observation.NetworkPolicy, attempt.PlanRequest) {
		_ = c.Store.FailAttempt(ctx, attempt.ID, c.Owner, "kubernetes-adoption-mismatch", c.now())
		return result, ErrInfrastructure
	}
	if err = c.Store.MarkAttemptRunning(ctx, attempt.ID, c.Owner, c.now()); err != nil {
		return result, err
	}
	result.State = AttemptRunning
	switch observation.State {
	case WorkloadPending, WorkloadRunning:
		return result, nil
	case WorkloadFailed:
		code := observation.FailureCode
		if validateFailureCode(code) != nil {
			code = "builder-job-failed"
		}
		return c.retryInfrastructure(ctx, attempt, code, ErrInfrastructure)
	case WorkloadSucceeded:
		buildResult, decodeErr := decodeBuildResult(observation.Result)
		if decodeErr != nil || validateResultForAttempt(buildResult, attempt) != nil || !logRefRE.MatchString(observation.LogReference) {
			_ = c.Store.FailAttempt(ctx, attempt.ID, c.Owner, "builder-result-invalid", c.now())
			if decodeErr != nil {
				return result, decodeErr
			}
			return result, ErrInfrastructure
		}
		finalCache := cacheReference(attempt)
		if err = c.Store.HeartbeatAttempt(ctx, attempt.ID, c.Owner, c.now(), c.LeaseDuration); err != nil {
			return result, err
		}
		promoted, promoteErr := c.Kubernetes.PromoteCache(ctx, attempt, finalCache)
		if promoteErr != nil || !promoted {
			finalCache = ""
			buildResult.Warnings = addResultWarning(buildResult.Warnings, builder.WarningCacheDegraded)
		}
		if err = c.Store.HeartbeatAttempt(ctx, attempt.ID, c.Owner, c.now(), c.LeaseDuration); err != nil {
			return result, err
		}
		if err = c.Store.CompleteAttempt(ctx, attempt.ID, c.Owner, BuildCompletion{
			Result: buildResult, CacheReference: finalCache, LogReference: observation.LogReference,
		}, c.now()); err != nil {
			return result, err
		}
		result.State = AttemptSucceeded
		return result, nil
	default:
		_ = c.Store.FailAttempt(ctx, attempt.ID, c.Owner, "kubernetes-state-invalid", c.now())
		return result, ErrInfrastructure
	}
}

func (c *BuildController) deferInfrastructure(ctx context.Context, attempt BuildAttempt, code string, cause error) (ReconcileResult, error) {
	retryAt := c.now().Add(retryDelay(attempt.ExecutionAttempts))
	if err := c.Store.DeferAttempt(ctx, attempt.ID, c.Owner, code, c.now(), retryAt); err != nil {
		return ReconcileResult{AttemptID: attempt.ID, State: attempt.State}, err
	}
	result := ReconcileResult{AttemptID: attempt.ID, State: attempt.State, RetryAt: retryAt}
	if cause == ErrProviderRetry {
		return result, ErrProviderRetry
	}
	return result, fmt.Errorf("%w: %s: %v", ErrInfrastructure, code, cause)
}

func (c *BuildController) retryInfrastructure(ctx context.Context, attempt BuildAttempt, code string, cause error) (ReconcileResult, error) {
	retryAt := c.now().Add(retryDelay(attempt.ExecutionAttempts))
	retry, err := c.Store.ScheduleAttemptRetry(ctx, attempt.ID, c.Owner, code, c.now(), retryAt)
	result := ReconcileResult{AttemptID: attempt.ID, State: AttemptFailed}
	if err != nil {
		return result, err
	}
	if retry {
		persisted, getErr := c.Store.Attempt(ctx, attempt.ID)
		if getErr != nil {
			return result, getErr
		}
		result.State, result.RetryAt = persisted.State, persisted.AvailableAt
	}
	if cause == ErrProviderRetry {
		return result, ErrProviderRetry
	}
	if cause != nil {
		return result, fmt.Errorf("%w: %s: %v", ErrInfrastructure, code, cause)
	}
	return result, ErrInfrastructure
}

func retryDelay(executionAttempts int) time.Duration {
	if executionAttempts < 1 {
		executionAttempts = 1
	}
	delay := 15 * time.Second
	for index := 1; index < executionAttempts && delay < 5*time.Minute; index++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func decodeBuildResult(encoded []byte) (builder.BuildResult, error) {
	if len(encoded) == 0 || len(encoded) > builder.MaxResultBytes {
		return builder.BuildResult{}, ErrInvalid
	}
	decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(encoded), builder.MaxResultBytes+1))
	decoder.DisallowUnknownFields()
	var result builder.BuildResult
	if err := decoder.Decode(&result); err != nil {
		return builder.BuildResult{}, ErrInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return builder.BuildResult{}, ErrInvalid
	}
	return result, nil
}

func validateResultForAttempt(result builder.BuildResult, attempt BuildAttempt) error {
	if result.OperationID != attempt.ID || result.Generation != attempt.Generation || result.Image.Reference != attempt.PlanRequest.Build.Destination.Repository+"@"+result.Image.Digest ||
		!slices.Equal(result.Image.Platforms, attempt.PlanRequest.Build.Platforms) {
		return ErrInvalid
	}
	if result.Cache != nil && result.Cache.Reference != cacheReference(attempt) {
		return ErrInvalid
	}
	return validateBuildResult(result, "", "")
}

func validateBuildResult(result builder.BuildResult, cacheRef, logRef string) error {
	if result.APIVersion != builder.ProtocolVersion || result.Status != "Succeeded" || !uuidRE.MatchString(result.OperationID) || result.Generation < 1 ||
		!digestRE.MatchString(result.Image.Digest) || !strings.HasSuffix(result.Image.Reference, "@"+result.Image.Digest) ||
		len(result.Image.Platforms) < 1 || len(result.Image.Platforms) > 2 || !slices.IsSorted(result.Image.Platforms) ||
		result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) || result.CompletedAt.IsZero() {
		return ErrInvalid
	}
	switch result.CacheReuse {
	case builder.CacheReuseNotRequested, builder.CacheReuseUnavailable, builder.CacheReuseHit, builder.CacheReuseMiss, builder.CacheReuseUnknown:
	default:
		return ErrInvalid
	}
	seenPlatforms := map[string]struct{}{}
	for _, platform := range result.Image.Platforms {
		if platform != "linux/amd64" && platform != "linux/arm64" {
			return ErrInvalid
		}
		if _, duplicate := seenPlatforms[platform]; duplicate {
			return ErrInvalid
		}
		seenPlatforms[platform] = struct{}{}
	}
	seenWarnings := map[builder.Warning]struct{}{}
	for _, warning := range result.Warnings {
		if warning != builder.WarningColdBuild && warning != builder.WarningCacheDegraded && warning != builder.WarningSensitiveBuildArg {
			return ErrInvalid
		}
		if _, duplicate := seenWarnings[warning]; duplicate {
			return ErrInvalid
		}
		seenWarnings[warning] = struct{}{}
	}
	if cacheRef != "" {
		if !strings.Contains(cacheRef, ":generation-") || strings.Contains(cacheRef, "@") {
			return ErrInvalid
		}
	}
	if result.Cache != nil {
		if result.Cache.Reference == "" || strings.Contains(result.Cache.Reference, "@") || !strings.Contains(result.Cache.Reference, ":generation-") || !digestRE.MatchString(result.Cache.Digest) {
			return ErrInvalid
		}
	}
	if logRef != "" && !logRefRE.MatchString(logRef) {
		return ErrInvalid
	}
	return nil
}

// normalizeLegacyCacheReuse keeps pre-signal PostgreSQL results readable after
// an upgrade without fabricating a hit or miss. New completion writes still
// require one explicit closed value through validateBuildResult.
func normalizeLegacyCacheReuse(result *builder.BuildResult) {
	if result != nil && result.CacheReuse == "" {
		result.CacheReuse = builder.CacheReuseUnknown
	}
}

func addResultWarning(warnings []builder.Warning, warning builder.Warning) []builder.Warning {
	if !slices.Contains(warnings, warning) {
		return append(warnings, warning)
	}
	return warnings
}

func (c *BuildController) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now().UTC()
}
