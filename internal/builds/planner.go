package builds

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
)

func generatedBuildRequest(operationID string, generation int64, projectID, serviceID, commit string, spec DefinitionSpec, imports []string, definitionDigest string) builder.BuildRequest {
	compactOperation := strings.ReplaceAll(operationID, "-", "")
	platforms := slices.Clone(spec.Platforms)
	slices.Sort(platforms)
	platformScope := make([]string, len(platforms))
	for index, platform := range platforms {
		platformScope[index] = strings.TrimPrefix(platform, "linux/")
	}
	cacheRepository := spec.Registry.Server + "/" + spec.Registry.RepositoryPrefix + "/projects/" + projectID + "/services/" + serviceID + "/cache/v1/" + spec.CacheTrustLane + "/" + strings.Join(platformScope, "-") + "/" + strings.TrimPrefix(definitionDigest, "sha256:")
	return builder.BuildRequest{
		APIVersion: builder.ProtocolVersion, OperationID: operationID, Generation: generation,
		ProjectID: projectID, ServiceID: serviceID, Commit: commit,
		ContextPath: spec.ContextPath, DockerfilePath: spec.DockerfilePath, Platforms: platforms,
		Destination: builder.Destination{
			Repository: spec.Registry.Server + "/" + spec.Registry.RepositoryPrefix + "/projects/" + projectID + "/services/" + serviceID + "/image",
			Reference:  fmt.Sprintf("candidate-%s-g%d-%s", compactOperation, generation, commit[:12]),
		},
		Registry: builder.RegistryCredentials{
			Server: spec.Registry.Server, RepositoryPrefix: spec.Registry.RepositoryPrefix,
			UsernameFile: builder.RegistryPushSecretRoot + "/username", PasswordFile: builder.RegistryPushSecretRoot + "/password",
		},
		BuildArgs: slices.Clone(spec.BuildArgs), SecretFiles: slices.Clone(spec.SecretFiles), SSHFiles: slices.Clone(spec.SSHFiles),
		Cache: builder.CachePolicy{
			Schema: "v1", TrustLane: spec.CacheTrustLane, BuildDefinition: definitionDigest,
			Imports: slices.Clone(imports), CandidateExport: fmt.Sprintf("%s:candidate-%s-g%d", cacheRepository, compactOperation, generation),
			UsernameFile: builder.RegistryCacheSecretRoot + "/username", PasswordFile: builder.RegistryCacheSecretRoot + "/password",
		},
		Profile: spec.Profile,
	}
}

func jobPlanRequest(request builder.BuildRequest, spec DefinitionSpec, operationID string) builder.JobPlanRequest {
	compact := strings.ReplaceAll(operationID, "-", "")
	return builder.JobPlanRequest{
		Build:     request,
		Namespace: spec.Execution.Namespace, PodServiceAccount: spec.Execution.PodServiceAccount,
		RequestConfigMap: "build-request-" + compact[:24], SourceCredentialSecret: "source-credentials-" + compact[:20],
		RegistryPushCredentialSecret:  spec.Registry.PushCredentialSecret,
		RegistryCacheCredentialSecret: spec.Registry.CacheCredentialSecret,
		BuildSecret:                   spec.Execution.BuildSecret, SSHSecret: spec.Execution.SSHSecret,
		CheckoutImage: spec.Execution.BuilderAgentImage, AgentImage: spec.Execution.BuilderAgentImage,
		NodeSelector: cloneStringMap(spec.Execution.NodeSelector), Toleration: spec.Execution.Toleration,
		CheckoutResources: spec.Execution.CheckoutResources, DinDResources: spec.Execution.DinDResources, AgentResources: spec.Execution.AgentResources,
		WorkspaceSizeLimit: spec.Execution.WorkspaceSizeLimit, SocketSizeLimit: spec.Execution.SocketSizeLimit,
		ResultSizeLimit: spec.Execution.ResultSizeLimit, DockerDataSizeLimit: spec.Execution.DockerDataSizeLimit,
		ActiveDeadlineSeconds: spec.Execution.ActiveDeadlineSeconds, TTLSecondsAfterFinished: spec.Execution.TTLSecondsAfterFinished,
		Egress: slices.Clone(spec.Execution.Egress),
	}
}

func checkoutRequest(operationID string, generation int64, commit string, repository Repository) builder.CheckoutRequest {
	return builder.CheckoutRequest{
		APIVersion: builder.ProtocolVersion, OperationID: operationID, Generation: generation,
		RepositoryURL: "https://github.com/" + repository.Identity.OwnerLogin + "/" + repository.Identity.Name + ".git",
		ApprovedHost:  "github.com", Commit: commit,
		UsernameFile: builder.SourceCredentialRoot + "/username", AccessTokenFile: builder.SourceCredentialRoot + "/token",
	}
}

func newAttempt(definition BuildDefinition, repository Repository, delivery EnqueuePush, generation int64, imports []string, now time.Time) (BuildAttempt, error) {
	operationID := deterministicUUID("build-attempt-v1", delivery.ClaimKey, definition.ID)
	request := generatedBuildRequest(operationID, generation, definition.ProjectID, definition.ServiceID, delivery.CommitSHA, definition.Spec, imports, definition.DefinitionDigest)
	planRequest := jobPlanRequest(request, definition.Spec, operationID)
	checkout := checkoutRequest(operationID, generation, delivery.CommitSHA, repository)
	if err := request.Validate(); err != nil {
		return BuildAttempt{}, fmt.Errorf("%w: generated build request: %v", ErrInvalid, err)
	}
	plan, err := builder.PlanJob(planRequest)
	if err != nil {
		return BuildAttempt{}, fmt.Errorf("%w: generated Job plan: %v", ErrInvalid, err)
	}
	if err := checkout.Validate(); err != nil {
		return BuildAttempt{}, fmt.Errorf("%w: generated checkout request: %v", ErrInvalid, err)
	}
	inputDigest, err := attemptInputDigest(planRequest, checkout)
	if err != nil {
		return BuildAttempt{}, err
	}
	metadata := plan.Job["metadata"].(map[string]any)
	return BuildAttempt{
		ID: operationID, DefinitionID: definition.ID, DeliveryClaimKey: delivery.ClaimKey,
		ProjectID: definition.ProjectID, ServiceID: definition.ServiceID, CommitSHA: delivery.CommitSHA, GitRef: delivery.GitRef,
		Generation: generation, DefinitionDigest: definition.DefinitionDigest,
		PlanRequest: planRequest, CheckoutRequest: checkout, InputDigest: inputDigest, RegistryMode: definition.Spec.Registry.Mode,
		State: AttemptQueued, MaxAttempts: definition.Spec.MaxAttempts, AvailableAt: now.UTC(),
		JobNamespace: definition.Spec.Execution.Namespace, JobName: metadata["name"].(string), CacheCandidate: request.Cache.CandidateExport,
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}, nil
}

func newAttemptWithExecution(definition BuildDefinition, execution ExecutionSettings, repository Repository, delivery EnqueuePush, generation int64, imports []string, now time.Time) (BuildAttempt, error) {
	definition.Spec.Execution = execution
	return newAttempt(definition, repository, delivery, generation, imports, now)
}

func attemptInputDigest(plan builder.JobPlanRequest, checkout builder.CheckoutRequest) (string, error) {
	encoded, err := json.Marshal(struct {
		Plan     builder.JobPlanRequest  `json:"plan"`
		Checkout builder.CheckoutRequest `json:"checkout"`
	}{plan, checkout})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func deterministicUUID(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	value := hash.Sum(nil)[:16]
	value[6] = (value[6] & 0x0f) | 0x80
	value[8] = (value[8] & 0x3f) | 0x80
	text := hex.EncodeToString(value)
	return text[:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:]
}

func cacheReference(attempt BuildAttempt) string {
	return strings.Split(attempt.CacheCandidate, ":candidate-")[0] + fmt.Sprintf(":generation-%d", attempt.Generation)
}

func cloneStringMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
