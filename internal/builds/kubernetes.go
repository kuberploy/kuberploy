package builds

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/kuberploy/kuberploy/internal/builder"
)

const (
	buildInputDigestAnnotation = "kuberploy.io/build-input-digest"
	credentialOwnerLabel       = "kuberploy.io/credential-owner"
	credentialOwnerCheckout    = "checkout"
	maximumSourceTokenBytes    = 2048
)

var (
	errKubernetesObjectNotFound = errors.New("Kubernetes object not found")
	errKubernetesObjectConflict = errors.New("Kubernetes object already exists")
	buildOperationLabelRE       = regexp.MustCompile(`^[0-9a-f]{32}$`)
	buildGenerationLabelRE      = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
	sourceTokenRE               = regexp.MustCompile(`^[A-Za-z0-9_.=-]+$`)
)

type kubernetesResource string

const (
	resourceConfigMaps      kubernetesResource = "configmaps"
	resourceSecrets         kubernetesResource = "secrets"
	resourcePods            kubernetesResource = "pods"
	resourceJobs            kubernetesResource = "jobs"
	resourceNetworkPolicies kubernetesResource = "networkpolicies"
)

type deletePreconditions struct {
	UID               string
	ResourceVersion   string
	PropagationPolicy string
}

// kubernetesBuildResources is deliberately narrower than a Kubernetes client.
// It cannot read logs, exec into Pods, update resources, or access resources
// outside the deterministic builder protocol.
type kubernetesBuildResources interface {
	Get(context.Context, kubernetesResource, string, string) (map[string]any, error)
	Create(context.Context, kubernetesResource, string, map[string]any) (map[string]any, error)
	Delete(context.Context, kubernetesResource, string, string, deletePreconditions) error
	ListBuildPods(context.Context, string, string, string, int64) ([]map[string]any, error)
	ListBuilderNodes(context.Context, int64) ([]map[string]any, error)
}

// KubernetesAdapter implements the durable build controller's Kubernetes
// boundary. The distinct release-push and cache credential authorities remain
// existing Secret references in the exact Job plan and are never read by this
// adapter.
type KubernetesAdapter struct {
	resources     kubernetesBuildResources
	namespace     string
	nodeIsolation bool
}

// ObservedBuildJob is an opaque, verified handle to the exact Kubernetes Job
// derived from an immutable BuildAttempt. Callers can inspect only the
// identity needed to fence subsequent read-only observations; the live object
// remains private so it cannot be substituted when verifying a Pod owner.
type ObservedBuildJob struct {
	Namespace       string
	Name            string
	UID             string
	OperationLabel  string
	GenerationLabel string
	Deleting        bool
	object          map[string]any
}

// ObservedBuildPod is the safe subset of a Pod controlled by an already
// verified build Job. It deliberately contains no log reference or secret
// material.
type ObservedBuildPod struct {
	Namespace     string
	Name          string
	UID           string
	AgentRestarts int32
	Ready         bool
	Terminating   bool
}

// VerifyObservedBuildJob reconstructs the exact desired Job from the durable
// attempt and applies the same adoption/defaulting checks used by the build
// controller. It is a pure read verifier for tightly scoped observers such as
// build logs; it performs no Kubernetes operation.
func VerifyObservedBuildJob(attempt BuildAttempt, live map[string]any) (ObservedBuildJob, error) {
	if validateStoredAttempt(attempt) != nil || attempt.ID == "" || attempt.JobNamespace == "" || attempt.JobName == "" ||
		!observableBuildAttemptState(attempt.State) ||
		attempt.JobNamespace != attempt.PlanRequest.Namespace || attempt.InputDigest == "" || !digestRE.MatchString(attempt.InputDigest) ||
		attempt.ID != attempt.PlanRequest.Build.OperationID || attempt.Generation != attempt.PlanRequest.Build.Generation ||
		attempt.CheckoutRequest.OperationID != attempt.ID || attempt.CheckoutRequest.Generation != attempt.Generation ||
		attempt.ProjectID != attempt.PlanRequest.Build.ProjectID || attempt.ServiceID != attempt.PlanRequest.Build.ServiceID ||
		attempt.CommitSHA != attempt.PlanRequest.Build.Commit || attempt.CommitSHA != attempt.CheckoutRequest.Commit ||
		attempt.CacheCandidate != attempt.PlanRequest.Build.Cache.CandidateExport {
		return ObservedBuildJob{}, ErrInvalid
	}
	plan, err := builder.PlanJob(attempt.PlanRequest)
	if err != nil || !builder.CanAdoptJob(plan.Job, attempt.PlanRequest) || objectName(plan.Job) != attempt.JobName {
		return ObservedBuildJob{}, ErrInvalid
	}
	desired := withInputDigest(plan.Job, attempt.InputDigest)
	if validationErr := validateLiveJob(live, desired); validationErr != nil {
		return ObservedBuildJob{}, validationErr
	}
	metadata, _ := live["metadata"].(map[string]any)
	uid, _ := metadata["uid"].(string)
	labels, _ := desired["metadata"].(map[string]any)["labels"].(map[string]any)
	operation, _ := labels["kuberploy.io/build-operation"].(string)
	generation, _ := labels["kuberploy.io/build-generation"].(string)
	if !buildOperationLabelRE.MatchString(operation) || !buildGenerationLabelRE.MatchString(generation) ||
		uid == "" || len(uid) > 256 || strings.IndexFunc(uid, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == ':' || r == '-')
	}) >= 0 {
		return ObservedBuildJob{}, ErrInfrastructure
	}
	metadata, _ = live["metadata"].(map[string]any)
	deleting, _ := metadata["deletionTimestamp"].(string)
	return ObservedBuildJob{
		Namespace: attempt.JobNamespace, Name: attempt.JobName, UID: uid, OperationLabel: operation,
		GenerationLabel: generation, Deleting: deleting != "", object: cloneMap(live),
	}, nil
}

func observableBuildAttemptState(state AttemptState) bool {
	switch state {
	case AttemptQueued, AttemptPreparing, AttemptRunning, AttemptCancelling, AttemptSucceeded, AttemptFailed, AttemptCancelled:
		return true
	default:
		return false
	}
}

// VerifyObservedBuildPod proves that a live Pod is the one controller-owned
// child of the verified Job and that the fixed builder-agent container exists.
// The caller must still re-read and compare the Pod UID immediately before
// opening the log subresource to close delete/recreate races.
func VerifyObservedBuildPod(job ObservedBuildJob, live map[string]any) (ObservedBuildPod, error) {
	if job.object == nil || job.Namespace == "" || job.Name == "" || job.UID == "" {
		return ObservedBuildPod{}, ErrInfrastructure
	}
	if validationErr := validateBuildPod(live, job.object); validationErr != nil {
		return ObservedBuildPod{}, validationErr
	}
	metadata, _ := live["metadata"].(map[string]any)
	uid, _ := metadata["uid"].(string)
	if uid == "" || len(uid) > 256 || strings.IndexFunc(uid, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == ':' || r == '-')
	}) >= 0 {
		return ObservedBuildPod{}, ErrInfrastructure
	}
	spec, _ := live["spec"].(map[string]any)
	containers, _ := spec["containers"].([]any)
	agentCount := 0
	for _, raw := range containers {
		container, _ := raw.(map[string]any)
		if container["name"] == "agent" {
			agentCount++
		}
	}
	initContainers, _ := spec["initContainers"].([]any)
	for _, raw := range initContainers {
		container, _ := raw.(map[string]any)
		if container["name"] == "agent" {
			agentCount++
		}
	}
	if agentCount != 1 {
		return ObservedBuildPod{}, ErrInfrastructure
	}
	status, _ := live["status"].(map[string]any)
	containerStatuses, _ := status["containerStatuses"].([]any)
	var restarts int32
	seenStatus := false
	for _, raw := range containerStatuses {
		container, _ := raw.(map[string]any)
		if container["name"] != "agent" {
			continue
		}
		if seenStatus {
			return ObservedBuildPod{}, ErrInfrastructure
		}
		seenStatus = true
		value, ok := integerValue(container["restartCount"])
		if !ok || value < 0 || value > int64(^uint32(0)>>1) {
			return ObservedBuildPod{}, ErrInfrastructure
		}
		restarts = int32(value)
	}
	ready := false
	conditions, _ := status["conditions"].([]any)
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] == "Ready" && condition["status"] == "True" {
			ready = true
		}
	}
	deleting := false
	if value, exists := metadata["deletionTimestamp"]; exists && value != nil && value != "" {
		if _, ok := value.(string); !ok {
			return ObservedBuildPod{}, ErrInfrastructure
		}
		deleting = true
	}
	return ObservedBuildPod{Namespace: job.Namespace, Name: objectName(live), UID: uid, AgentRestarts: restarts, Ready: ready, Terminating: deleting}, nil
}

func newKubernetesAdapter(resources kubernetesBuildResources, namespace string) (*KubernetesAdapter, error) {
	return newKubernetesAdapterWithIsolation(resources, namespace, true)
}

func newKubernetesAdapterWithIsolation(resources kubernetesBuildResources, namespace string, nodeIsolation bool) (*KubernetesAdapter, error) {
	if resources == nil || !kubeNameRE.MatchString(namespace) {
		return nil, ErrInvalid
	}
	return &KubernetesAdapter{resources: resources, namespace: namespace, nodeIsolation: nodeIsolation}, nil
}

var _ KubernetesBuildAPI = (*KubernetesAdapter)(nil)

func (a *KubernetesAdapter) BuilderCapacityReady(ctx context.Context) error {
	return a.BuilderCapacityReadyFor(ctx, a.nodeIsolation)
}

func (a *KubernetesAdapter) BuilderCapacityReadyFor(ctx context.Context, nodeIsolation bool) error {
	if a == nil || a.resources == nil {
		return ErrInfrastructure
	}
	nodes, err := a.resources.ListBuilderNodes(ctx, 100)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		eligible, validationErr := eligibleBuilderNode(node, nodeIsolation)
		if validationErr != nil {
			return validationErr
		}
		if eligible {
			return nil
		}
	}
	return ErrBuilderCapacityUnavailable
}

func eligibleBuilderNode(node map[string]any, nodeIsolation bool) (bool, error) {
	if node["apiVersion"] != "v1" || node["kind"] != "Node" || !kubeNameRE.MatchString(objectName(node)) {
		return false, ErrInfrastructure
	}
	metadata, metadataOK := node["metadata"].(map[string]any)
	labels, labelsOK := metadata["labels"].(map[string]any)
	if !metadataOK || !labelsOK {
		return false, ErrInfrastructure
	}
	if nodeIsolation && labels["kuberploy.io/node-class"] != "dind-builder" {
		return false, nil
	}
	if deleting, exists := metadata["deletionTimestamp"]; exists && deleting != nil && deleting != "" {
		return false, nil
	}
	spec, specOK := node["spec"].(map[string]any)
	status, statusOK := node["status"].(map[string]any)
	if !specOK || !statusOK {
		return false, ErrInfrastructure
	}
	if unschedulable, exists := spec["unschedulable"]; exists {
		value, ok := unschedulable.(bool)
		if !ok {
			return false, ErrInfrastructure
		}
		if value {
			return false, nil
		}
	}
	ready := false
	conditions, conditionsOK := status["conditions"].([]any)
	if !conditionsOK {
		return false, ErrInfrastructure
	}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			return false, ErrInfrastructure
		}
		if condition["type"] == "Ready" && condition["status"] == "True" {
			ready = true
		}
	}
	if !ready {
		return false, nil
	}
	taints, exists := spec["taints"]
	if !exists {
		return !nodeIsolation, nil
	}
	taintList, ok := taints.([]any)
	if !ok {
		return false, ErrInfrastructure
	}
	required := false
	for _, raw := range taintList {
		taint, ok := raw.(map[string]any)
		if !ok {
			return false, ErrInfrastructure
		}
		key, keyOK := taint["key"].(string)
		effect, effectOK := taint["effect"].(string)
		value := ""
		if rawValue, exists := taint["value"]; exists {
			var valueOK bool
			value, valueOK = rawValue.(string)
			if !valueOK {
				return false, ErrInfrastructure
			}
		}
		if !keyOK || !effectOK {
			return false, ErrInfrastructure
		}
		if key == "kuberploy.io/dind-builder" && value == "true" && effect == "NoSchedule" {
			required = true
			continue
		}
		if effect == "NoSchedule" || effect == "NoExecute" {
			return false, nil
		}
	}
	return !nodeIsolation || required, nil
}

func (a *KubernetesAdapter) Ensure(ctx context.Context, workload BuildWorkload) (WorkloadObservation, error) {
	return a.ensure(ctx, workload, workload.SourceCredential.Reveal())
}

func (a *KubernetesAdapter) ensure(ctx context.Context, workload BuildWorkload, rawSourceCredential string) (WorkloadObservation, error) {
	desired, err := a.desiredWorkload(workload, rawSourceCredential)
	if err != nil {
		return WorkloadObservation{}, err
	}
	defer clearSecretData(desired.sourceSecret)

	liveJob, jobFound, err := a.get(ctx, resourceJobs, desired.namespace, desired.jobName)
	if err != nil {
		return WorkloadObservation{}, err
	}
	if jobFound {
		if err = validateLiveJob(liveJob, desired.job); err != nil {
			return WorkloadObservation{}, err
		}
		if jobDeletionInProgress(liveJob) {
			if workload.Attempt.ExecutionAttempts > 1 && isTerminalJob(liveJob) && (workload.Attempt.State == AttemptPreparing || workload.Attempt.State == AttemptRunning) {
				if err = a.deleteSourceSecretIfOwned(ctx, desired.sourceSecret); err != nil {
					return WorkloadObservation{}, err
				}
				return pendingWorkloadObservation(workload), nil
			}
			return WorkloadObservation{}, ErrInfrastructure
		}
		if isTerminalJob(liveJob) && workload.Attempt.State == AttemptPreparing && workload.Attempt.ExecutionAttempts > 1 {
			if err = a.deleteValidated(ctx, resourceJobs, liveJob, desired.job, validateLiveJob, "Foreground"); err != nil {
				return WorkloadObservation{}, err
			}
			if err = a.deleteSourceSecretIfOwned(ctx, desired.sourceSecret); err != nil {
				return WorkloadObservation{}, err
			}
			// Foreground deletion may make the Job GET return NotFound before its
			// controlled Pod has disappeared. Never create the replacement in the
			// same reconciliation; the no-Job path below independently fences on
			// zero remaining Pods for this exact operation and generation.
			return pendingWorkloadObservation(workload), nil
		}
	}

	if !jobFound {
		if workload.Attempt.ExecutionAttempts > 1 {
			labels := metadataLabels(desired.job)
			operation, _ := labels["kuberploy.io/build-operation"].(string)
			generation, _ := labels["kuberploy.io/build-generation"].(string)
			remaining, listErr := a.resources.ListBuildPods(ctx, desired.namespace, operation, generation, 2)
			if listErr != nil {
				return WorkloadObservation{}, listErr
			}
			if len(remaining) != 0 {
				return pendingWorkloadObservation(workload), nil
			}
		}
		if _, err = a.ensureObject(ctx, resourceConfigMaps, desired.configMap, validateExactConfigMap); err != nil {
			return WorkloadObservation{}, err
		}
		if _, err = a.ensureObject(ctx, resourceNetworkPolicies, desired.networkPolicy, validateExactNetworkPolicy); err != nil {
			return WorkloadObservation{}, err
		}
		if err = a.replaceSourceSecret(ctx, desired.sourceSecret); err != nil {
			return WorkloadObservation{}, err
		}
		jobEstablished := false
		defer func() {
			if !jobEstablished {
				_ = a.deleteSourceSecretIfOwned(context.WithoutCancel(ctx), desired.sourceSecret)
			}
		}()
		liveJob, err = a.createOrAdoptJob(ctx, desired.job)
		if err != nil {
			return WorkloadObservation{}, err
		}
		jobEstablished = true
	}

	pods, err := a.buildPods(ctx, desired, liveJob)
	if err != nil {
		return WorkloadObservation{}, fmt.Errorf("build Pod validation: %w", err)
	}
	if err = a.validateAuxiliaries(ctx, desired, liveJob, pods); err != nil {
		return WorkloadObservation{}, fmt.Errorf("build auxiliary validation: %w", err)
	}
	observation := observeBuild(workload.Attempt, liveJob, pods)
	observation.Job = cloneMap(workload.Plan.Job)
	observation.NetworkPolicy = cloneMap(workload.Plan.NetworkPolicy)
	observation.InputDigest = workload.InputDigest
	if observation.State == WorkloadFailed {
		// Keep the terminal Job/Pod as the bounded result/log authority, but do
		// not retain the request or run-scoped egress policy after a failed
		// attempt. Retries recreate both exact auxiliaries from the immutable
		// plan, and terminal failures must not accumulate policy residue.
		if err = a.cleanupTerminalAuxiliaries(ctx, desired); err != nil {
			return WorkloadObservation{}, err
		}
	} else if shouldDeleteSourceSecret(liveJob, pods) {
		if err = a.deleteSourceSecretIfOwned(ctx, desired.sourceSecret); err != nil {
			return WorkloadObservation{}, err
		}
	}
	return observation, nil
}

func pendingWorkloadObservation(workload BuildWorkload) WorkloadObservation {
	return WorkloadObservation{
		State: WorkloadPending, Job: cloneMap(workload.Plan.Job), NetworkPolicy: cloneMap(workload.Plan.NetworkPolicy), InputDigest: workload.InputDigest,
	}
}

func (a *KubernetesAdapter) Cancel(ctx context.Context, attempt BuildAttempt) error {
	desired, err := a.desiredAttempt(attempt)
	if err != nil {
		return err
	}
	if live, found, getErr := a.get(ctx, resourceJobs, desired.namespace, desired.jobName); getErr != nil {
		return getErr
	} else if found {
		if err = a.deleteValidated(ctx, resourceJobs, live, desired.job, validateLiveJob, "Foreground"); err != nil {
			return err
		}
	}
	if err = a.deleteSourceSecretIfOwned(ctx, desired.sourceSecret); err != nil {
		return err
	}
	for _, item := range []struct {
		resource kubernetesResource
		object   map[string]any
		validate func(map[string]any, map[string]any) error
	}{
		{resourceConfigMaps, desired.configMap, validateExactConfigMap},
		{resourceNetworkPolicies, desired.networkPolicy, validateExactNetworkPolicy},
	} {
		live, found, getErr := a.get(ctx, item.resource, desired.namespace, objectName(item.object))
		if getErr != nil {
			return getErr
		}
		if found {
			if err = a.deleteValidated(ctx, item.resource, live, item.object, item.validate, "Background"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *KubernetesAdapter) PromoteCache(ctx context.Context, attempt BuildAttempt, finalReference string) (bool, error) {
	desired, err := a.desiredAttempt(attempt)
	if err != nil || finalReference != cacheReference(attempt) {
		return false, ErrInvalid
	}
	liveJob, found, err := a.get(ctx, resourceJobs, desired.namespace, desired.jobName)
	if err != nil {
		return false, err
	}
	if !found || validateLiveJob(liveJob, desired.job) != nil {
		return false, ErrInfrastructure
	}
	pods, err := a.buildPods(ctx, desired, liveJob)
	if err != nil || len(pods) != 1 {
		return false, ErrInfrastructure
	}
	if !jobSucceeded(liveJob) && !nativeBuildSidecarTerminated(pods[0]) {
		return false, ErrInfrastructure
	}
	encoded, ok := terminationResult(pods[0])
	if !ok {
		return false, ErrInfrastructure
	}
	result, err := decodeBuildResult(encoded)
	clear(encoded)
	if err != nil || validateResultForAttempt(result, attempt) != nil {
		return false, ErrInfrastructure
	}
	promoted := result.Cache != nil && result.Cache.Reference == finalReference && digestRE.MatchString(result.Cache.Digest)
	// Result collection is complete. Keep the TTL-governed Job/Pod as the
	// durable result/log source, but remove every auxiliary object by exact
	// identity. A terminal Job's input-digest annotation makes re-observation
	// safe after this cleanup.
	if cleanupErr := a.cleanupTerminalAuxiliaries(ctx, desired); cleanupErr != nil {
		return false, cleanupErr
	}
	return promoted, nil
}

type desiredKubernetesWorkload struct {
	namespace     string
	jobName       string
	job           map[string]any
	networkPolicy map[string]any
	configMap     map[string]any
	sourceSecret  map[string]any
}

func (a *KubernetesAdapter) desiredWorkload(workload BuildWorkload, rawSourceCredential string) (desiredKubernetesWorkload, error) {
	desired, err := a.desiredAttempt(workload.Attempt)
	if err != nil || workload.InputDigest != workload.Attempt.InputDigest || !reflect.DeepEqual(workload.CheckoutRequest, workload.Attempt.CheckoutRequest) {
		return desiredKubernetesWorkload{}, ErrInvalid
	}
	expectedPlan, err := builder.PlanJob(workload.Attempt.PlanRequest)
	if err != nil || !reflect.DeepEqual(workload.Plan, expectedPlan) {
		return desiredKubernetesWorkload{}, ErrInvalid
	}
	if workload.CheckoutRequest.SSHPrivateKeyFile != "" {
		if workload.SourceUsername != "" || rawSourceCredential != "" || len(workload.SSHPrivateKey) == 0 || len(workload.SSHKnownHosts) == 0 {
			return desiredKubernetesWorkload{}, ErrInvalid
		}
		desired.sourceSecret, err = desiredSSHSourceSecret(workload.Attempt.PlanRequest, workload.InputDigest, workload.SSHPrivateKey, workload.SSHKnownHosts)
	} else {
		if workload.SourceUsername != "x-access-token" || len(workload.SSHPrivateKey) != 0 || len(workload.SSHKnownHosts) != 0 {
			return desiredKubernetesWorkload{}, ErrInvalid
		}
		raw := rawSourceCredential
		if len(raw) < 20 || len(raw) > maximumSourceTokenBytes || !sourceTokenRE.MatchString(raw) {
			return desiredKubernetesWorkload{}, ErrInvalid
		}
		token := []byte(raw)
		defer clear(token)
		desired.sourceSecret, err = desiredSourceSecret(workload.Attempt.PlanRequest, workload.InputDigest, []byte(workload.SourceUsername), token)
	}
	return desired, err
}

func (a *KubernetesAdapter) desiredAttempt(attempt BuildAttempt) (desiredKubernetesWorkload, error) {
	if attempt.JobNamespace != a.namespace || attempt.PlanRequest.Namespace != a.namespace || attempt.InputDigest == "" || !digestRE.MatchString(attempt.InputDigest) ||
		attempt.ID != attempt.PlanRequest.Build.OperationID || attempt.Generation != attempt.PlanRequest.Build.Generation || attempt.CheckoutRequest.OperationID != attempt.ID || attempt.CheckoutRequest.Generation != attempt.Generation ||
		attempt.PlanRequest.SourceCredentialSecret == "" || attempt.CacheCandidate != attempt.PlanRequest.Build.Cache.CandidateExport {
		return desiredKubernetesWorkload{}, ErrInvalid
	}
	plan, err := builder.PlanJob(attempt.PlanRequest)
	if err != nil || !builder.CanAdoptJob(plan.Job, attempt.PlanRequest) || !builder.CanAdoptNetworkPolicy(plan.NetworkPolicy, attempt.PlanRequest) {
		return desiredKubernetesWorkload{}, ErrInvalid
	}
	name := objectName(plan.Job)
	if name == "" || attempt.JobName != name {
		return desiredKubernetesWorkload{}, ErrInvalid
	}
	job := withInputDigest(plan.Job, attempt.InputDigest)
	policy := withInputDigest(plan.NetworkPolicy, attempt.InputDigest)
	configMap, err := desiredRequestConfigMap(attempt.PlanRequest, attempt.CheckoutRequest, attempt.InputDigest)
	if err != nil {
		return desiredKubernetesWorkload{}, err
	}
	return desiredKubernetesWorkload{
		namespace: a.namespace, jobName: name, job: job, networkPolicy: policy, configMap: configMap,
		sourceSecret: sourceSecretSkeleton(attempt.PlanRequest, attempt.InputDigest),
	}, nil
}

func desiredRequestConfigMap(plan builder.JobPlanRequest, checkout builder.CheckoutRequest, inputDigest string) (map[string]any, error) {
	buildJSON, err := json.Marshal(plan.Build)
	if err != nil {
		return nil, ErrInvalid
	}
	checkoutJSON, err := json.Marshal(checkout)
	if err != nil || len(buildJSON)+len(checkoutJSON) > builder.MaxRequestBytes {
		return nil, ErrInvalid
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata":  auxiliaryMetadata(plan, plan.RequestConfigMap, inputDigest, ""),
		"immutable": true,
		"data":      map[string]any{"build.json": string(buildJSON), "checkout.json": string(checkoutJSON)},
	}, nil
}

func sourceSecretSkeleton(plan builder.JobPlanRequest, inputDigest string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":  auxiliaryMetadata(plan, plan.SourceCredentialSecret, inputDigest, credentialOwnerCheckout),
		"immutable": true, "type": "Opaque",
	}
}

func desiredSourceSecret(plan builder.JobPlanRequest, inputDigest string, username, token []byte) (map[string]any, error) {
	if plan.SourceCredentialSecret == "" || string(username) != "x-access-token" || len(token) < 20 || len(token) > maximumSourceTokenBytes || !sourceTokenRE.Match(token) {
		return nil, ErrInvalid
	}
	secret := sourceSecretSkeleton(plan, inputDigest)
	secret["data"] = map[string]any{
		"username": base64.StdEncoding.EncodeToString(username),
		"token":    base64.StdEncoding.EncodeToString(token),
	}
	return secret, nil
}

func desiredSSHSourceSecret(plan builder.JobPlanRequest, inputDigest string, privateKey, knownHosts []byte) (map[string]any, error) {
	if plan.SourceCredentialSecret == "" || len(privateKey) == 0 || len(privateKey) > 64<<10 || len(knownHosts) == 0 || len(knownHosts) > 64<<10 ||
		bytes.IndexByte(privateKey, 0) >= 0 || bytes.IndexByte(knownHosts, 0) >= 0 {
		return nil, ErrInvalid
	}
	secret := sourceSecretSkeleton(plan, inputDigest)
	secret["data"] = map[string]any{
		"ssh-private-key": base64.StdEncoding.EncodeToString(privateKey),
		"known_hosts":     base64.StdEncoding.EncodeToString(knownHosts),
	}
	return secret, nil
}

func auxiliaryMetadata(plan builder.JobPlanRequest, name, inputDigest, owner string) map[string]any {
	planned, _ := builder.PlanJob(plan)
	labels := cloneMap(planned.Job["metadata"].(map[string]any)["labels"].(map[string]any))
	if owner != "" {
		labels[credentialOwnerLabel] = owner
	}
	return map[string]any{
		"name": name, "namespace": plan.Namespace, "labels": labels,
		"annotations": map[string]any{buildInputDigestAnnotation: inputDigest},
	}
}

func withInputDigest(object map[string]any, digest string) map[string]any {
	result := cloneMap(object)
	metadata := result["metadata"].(map[string]any)
	metadata["annotations"] = map[string]any{buildInputDigestAnnotation: digest}
	return result
}

func (a *KubernetesAdapter) get(ctx context.Context, resource kubernetesResource, namespace, name string) (map[string]any, bool, error) {
	object, err := a.resources.Get(ctx, resource, namespace, name)
	if errors.Is(err, errKubernetesObjectNotFound) {
		return nil, false, nil
	}
	return object, err == nil, err
}

func (a *KubernetesAdapter) ensureObject(ctx context.Context, resource kubernetesResource, desired map[string]any, validate func(map[string]any, map[string]any) error) (map[string]any, error) {
	namespace, name := objectNamespace(desired), objectName(desired)
	live, found, err := a.get(ctx, resource, namespace, name)
	if err != nil {
		return nil, err
	}
	if !found {
		live, err = a.resources.Create(ctx, resource, namespace, cloneMap(desired))
		if errors.Is(err, errKubernetesObjectConflict) {
			live, found, err = a.get(ctx, resource, namespace, name)
			if err == nil && !found {
				err = ErrInfrastructure
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: ensure %s: %v", ErrInfrastructure, resource, err)
	}
	if validate(live, desired) != nil {
		return nil, fmt.Errorf("%w: stored %s did not match the planned object", ErrInfrastructure, resource)
	}
	return live, nil
}

func (a *KubernetesAdapter) createOrAdoptJob(ctx context.Context, desired map[string]any) (map[string]any, error) {
	live, err := a.resources.Create(ctx, resourceJobs, objectNamespace(desired), cloneMap(desired))
	if errors.Is(err, errKubernetesObjectConflict) {
		var found bool
		live, found, err = a.get(ctx, resourceJobs, objectNamespace(desired), objectName(desired))
		if err == nil && !found {
			err = ErrInfrastructure
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: create or adopt Job: %v", ErrInfrastructure, err)
	}
	if validationErr := validateLiveJob(live, desired); validationErr != nil {
		return nil, fmt.Errorf("%w: stored Job did not match the canonical planned object: %v", ErrInfrastructure, validationErr)
	}
	return live, nil
}

func (a *KubernetesAdapter) replaceSourceSecret(ctx context.Context, desired map[string]any) error {
	live, found, err := a.get(ctx, resourceSecrets, objectNamespace(desired), objectName(desired))
	if err != nil {
		return err
	}
	if found {
		defer clearSecretData(live)
		if validateSourceSecret(live, desired) != nil {
			return ErrInfrastructure
		}
		if err = a.deleteObject(ctx, resourceSecrets, live, "Background"); err != nil {
			return err
		}
	}
	live, err = a.resources.Create(ctx, resourceSecrets, objectNamespace(desired), cloneMap(desired))
	defer clearSecretData(live)
	if err != nil {
		return fmt.Errorf("%w: create source credential Secret: %v", ErrInfrastructure, err)
	}
	if validateCreatedSourceSecret(live, desired) != nil {
		return fmt.Errorf("%w: stored source credential Secret did not match the planned object", ErrInfrastructure)
	}
	return nil
}

func (a *KubernetesAdapter) validateAuxiliaries(ctx context.Context, desired desiredKubernetesWorkload, liveJob map[string]any, pods []map[string]any) error {
	terminal := isTerminalJob(liveJob)
	for _, item := range []struct {
		resource kubernetesResource
		object   map[string]any
		validate func(map[string]any, map[string]any) error
	}{
		{resourceConfigMaps, desired.configMap, validateExactConfigMap},
		{resourceNetworkPolicies, desired.networkPolicy, validateExactNetworkPolicy},
	} {
		live, found, err := a.get(ctx, item.resource, desired.namespace, objectName(item.object))
		if err != nil {
			return err
		}
		if !found {
			if terminal {
				continue
			}
			return ErrInfrastructure
		}
		if item.validate(live, item.object) != nil {
			return ErrInfrastructure
		}
	}
	liveSecret, found, err := a.get(ctx, resourceSecrets, desired.namespace, objectName(desired.sourceSecret))
	if err != nil {
		return err
	}
	if found {
		defer clearSecretData(liveSecret)
		if validateSourceSecret(liveSecret, desired.sourceSecret) != nil {
			return ErrInfrastructure
		}
	} else if !shouldDeleteSourceSecret(liveJob, pods) {
		liveSecret, err = a.resources.Create(ctx, resourceSecrets, desired.namespace, cloneMap(desired.sourceSecret))
		created := err == nil
		if errors.Is(err, errKubernetesObjectConflict) {
			liveSecret, found, err = a.get(ctx, resourceSecrets, desired.namespace, objectName(desired.sourceSecret))
			if err == nil && !found {
				err = ErrInfrastructure
			}
		}
		defer clearSecretData(liveSecret)
		if err != nil || created && validateCreatedSourceSecret(liveSecret, desired.sourceSecret) != nil || !created && validateSourceSecret(liveSecret, desired.sourceSecret) != nil {
			return ErrInfrastructure
		}
	}
	return nil
}

func (a *KubernetesAdapter) buildPods(ctx context.Context, desired desiredKubernetesWorkload, job map[string]any) ([]map[string]any, error) {
	labels := metadataLabels(desired.job)
	operation, _ := labels["kuberploy.io/build-operation"].(string)
	generation, _ := labels["kuberploy.io/build-generation"].(string)
	pods, err := a.resources.ListBuildPods(ctx, desired.namespace, operation, generation, 2)
	if err != nil {
		return nil, fmt.Errorf("%w: list exact operation Pods: %v", ErrInfrastructure, err)
	}
	if len(pods) > 1 {
		return nil, fmt.Errorf("%w: exact operation selected %d Pods", ErrInfrastructure, len(pods))
	}
	for index, pod := range pods {
		if validationErr := validateBuildPod(pod, job); validationErr != nil {
			return nil, fmt.Errorf("%w: Pod %d identity: %v", ErrInfrastructure, index, validationErr)
		}
	}
	return pods, nil
}

func (a *KubernetesAdapter) deleteSourceSecretIfOwned(ctx context.Context, desired map[string]any) error {
	live, found, err := a.get(ctx, resourceSecrets, objectNamespace(desired), objectName(desired))
	if err != nil || !found {
		return err
	}
	defer clearSecretData(live)
	if validateSourceSecret(live, desired) != nil {
		return ErrInfrastructure
	}
	return a.deleteObject(ctx, resourceSecrets, live, "Background")
}

func (a *KubernetesAdapter) cleanupTerminalAuxiliaries(ctx context.Context, desired desiredKubernetesWorkload) error {
	if err := a.deleteSourceSecretIfOwned(ctx, desired.sourceSecret); err != nil {
		return err
	}
	for _, item := range []struct {
		resource kubernetesResource
		object   map[string]any
		validate func(map[string]any, map[string]any) error
	}{
		{resourceConfigMaps, desired.configMap, validateExactConfigMap},
		{resourceNetworkPolicies, desired.networkPolicy, validateExactNetworkPolicy},
	} {
		live, found, err := a.get(ctx, item.resource, desired.namespace, objectName(item.object))
		if err != nil {
			return err
		}
		if found {
			if err = a.deleteValidated(ctx, item.resource, live, item.object, item.validate, "Background"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *KubernetesAdapter) deleteValidated(ctx context.Context, resource kubernetesResource, live, desired map[string]any, validate func(map[string]any, map[string]any) error, propagation string) error {
	if validate(live, desired) != nil {
		return ErrInfrastructure
	}
	return a.deleteObject(ctx, resource, live, propagation)
}

func (a *KubernetesAdapter) deleteObject(ctx context.Context, resource kubernetesResource, live map[string]any, propagation string) error {
	metadata, _ := live["metadata"].(map[string]any)
	uid, _ := metadata["uid"].(string)
	version, _ := metadata["resourceVersion"].(string)
	if uid == "" || version == "" {
		return ErrInfrastructure
	}
	err := a.resources.Delete(ctx, resource, objectNamespace(live), objectName(live), deletePreconditions{UID: uid, ResourceVersion: version, PropagationPolicy: propagation})
	if errors.Is(err, errKubernetesObjectNotFound) {
		return nil
	}
	return err
}

func validateExactConfigMap(live, desired map[string]any) error {
	if err := validateIdentityMetadata(live, desired); err != nil || live["immutable"] != true || !reflect.DeepEqual(live["data"], desired["data"]) {
		return ErrInfrastructure
	}
	if data, exists := live["binaryData"]; exists && data != nil {
		return ErrInfrastructure
	}
	return nil
}

func validateSourceSecret(live, desired map[string]any) error {
	if err := validateIdentityMetadata(live, desired); err != nil || live["immutable"] != true || live["type"] != "Opaque" {
		return ErrInfrastructure
	}
	data, ok := live["data"].(map[string]any)
	if !ok || len(data) != 2 {
		return ErrInfrastructure
	}
	desiredData, _ := desired["data"].(map[string]any)
	_, wantsSSHPrivateKey := desiredData["ssh-private-key"]
	_, wantsKnownHosts := desiredData["known_hosts"]
	if wantsSSHPrivateKey || wantsKnownHosts {
		if !wantsSSHPrivateKey || !wantsKnownHosts || validateGitSSHSourceSecretData(data) != nil {
			return ErrInfrastructure
		}
	} else if _, hasSSHPrivateKey := data["ssh-private-key"]; hasSSHPrivateKey {
		if validateGitSSHSourceSecretData(data) != nil {
			return ErrInfrastructure
		}
	} else if validateGitHubSourceSecretData(data) != nil {
		return ErrInfrastructure
	}
	if _, extra := live["stringData"]; extra {
		return ErrInfrastructure
	}
	return nil
}

func validateGitHubSourceSecretData(data map[string]any) error {
	username, usernameOK := decodeSecretValue(data["username"], 64)
	token, tokenOK := decodeSecretValue(data["token"], maximumSourceTokenBytes)
	defer clear(username)
	defer clear(token)
	if !usernameOK || !tokenOK || string(username) != "x-access-token" || len(token) < 20 || !sourceTokenRE.Match(token) {
		return ErrInfrastructure
	}
	return nil
}

func validateGitSSHSourceSecretData(data map[string]any) error {
	privateKey, privateKeyOK := decodeSecretValue(data["ssh-private-key"], 64<<10)
	knownHosts, knownHostsOK := decodeSecretValue(data["known_hosts"], 64<<10)
	defer clear(privateKey)
	defer clear(knownHosts)
	if !privateKeyOK || !knownHostsOK || bytes.IndexByte(privateKey, 0) >= 0 || bytes.IndexByte(knownHosts, 0) >= 0 {
		return ErrInfrastructure
	}
	return nil
}

func validateCreatedSourceSecret(live, desired map[string]any) error {
	if err := validateSourceSecret(live, desired); err != nil {
		return err
	}
	liveData, liveOK := live["data"].(map[string]any)
	desiredData, desiredOK := desired["data"].(map[string]any)
	if !liveOK || !desiredOK || !reflect.DeepEqual(liveData, desiredData) {
		return ErrInfrastructure
	}
	return nil
}

func validateExactNetworkPolicy(live, desired map[string]any) error {
	if err := validateIdentityMetadata(live, desired); err != nil || !reflect.DeepEqual(live["spec"], desired["spec"]) {
		return ErrInfrastructure
	}
	return nil
}

func validateLiveJob(live, desired map[string]any) error {
	if err := validateJobIdentityMetadata(live, desired); err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	normalized := normalizeGoIntegers(cloneMap(live)).(map[string]any)
	expected := normalizeGoIntegers(cloneMap(desired)).(map[string]any)
	metadata := normalized["metadata"].(map[string]any)
	uid, _ := metadata["uid"].(string)
	if uid == "" {
		return fmt.Errorf("uid: %w", ErrInfrastructure)
	}
	spec, ok := normalized["spec"].(map[string]any)
	if !ok {
		return fmt.Errorf("spec: %w", ErrInfrastructure)
	}
	selector, _ := spec["selector"].(map[string]any)
	if selector == nil || !validJobSelector(selector, uid) {
		return fmt.Errorf("selector: %w", ErrInfrastructure)
	}
	delete(spec, "selector")
	template, _ := spec["template"].(map[string]any)
	templateMetadata, _ := template["metadata"].(map[string]any)
	desiredTemplate := expected["spec"].(map[string]any)["template"].(map[string]any)
	desiredTemplateMetadata := desiredTemplate["metadata"].(map[string]any)
	if !validTemplateLabels(templateMetadata, desiredTemplateMetadata, uid, objectName(desired)) {
		return fmt.Errorf("template labels: %w", ErrInfrastructure)
	}
	template["metadata"] = cloneMap(desiredTemplateMetadata)
	podSpec, _ := template["spec"].(map[string]any)
	if podSpec == nil || stripExactPodDefaults(podSpec, desiredTemplate["spec"].(map[string]any)) != nil {
		return fmt.Errorf("pod defaults: %w", ErrInfrastructure)
	}
	if !reflect.DeepEqual(spec, expected["spec"]) {
		return fmt.Errorf("normalized Job spec: %w", ErrInfrastructure)
	}
	return nil
}

func validateJobIdentityMetadata(live, desired map[string]any) error {
	if live == nil || live["apiVersion"] != desired["apiVersion"] || live["kind"] != desired["kind"] || objectName(live) != objectName(desired) || objectNamespace(live) != objectNamespace(desired) {
		return ErrInfrastructure
	}
	liveMetadata, _ := live["metadata"].(map[string]any)
	desiredMetadata, _ := desired["metadata"].(map[string]any)
	if liveMetadata == nil || desiredMetadata == nil || validateOwnedMetadataFields(liveMetadata, true) != nil || !reflect.DeepEqual(liveMetadata["labels"], desiredMetadata["labels"]) {
		return ErrInfrastructure
	}
	liveAnnotations, _ := liveMetadata["annotations"].(map[string]any)
	desiredAnnotations, _ := desiredMetadata["annotations"].(map[string]any)
	for key, value := range desiredAnnotations {
		if liveAnnotations[key] != value {
			return ErrInfrastructure
		}
	}
	for key, value := range liveAnnotations {
		if _, owned := desiredAnnotations[key]; owned {
			continue
		}
		if key != "batch.kubernetes.io/job-tracking" || value != "" {
			return ErrInfrastructure
		}
	}
	return nil
}

func validateIdentityMetadata(live, desired map[string]any) error {
	if live == nil || live["apiVersion"] != desired["apiVersion"] || live["kind"] != desired["kind"] || objectName(live) != objectName(desired) || objectNamespace(live) != objectNamespace(desired) {
		return ErrInfrastructure
	}
	liveMetadata, _ := live["metadata"].(map[string]any)
	desiredMetadata, _ := desired["metadata"].(map[string]any)
	if liveMetadata == nil || desiredMetadata == nil || validateOwnedMetadataFields(liveMetadata, false) != nil || !reflect.DeepEqual(liveMetadata["labels"], desiredMetadata["labels"]) || !reflect.DeepEqual(liveMetadata["annotations"], desiredMetadata["annotations"]) {
		return ErrInfrastructure
	}
	return nil
}

func validateOwnedMetadataFields(metadata map[string]any, allowDeletingJob bool) error {
	if value, exists := metadata["ownerReferences"]; exists && !emptyMetadataList(value) {
		return ErrInfrastructure
	}
	if generateName, _ := metadata["generateName"].(string); generateName != "" {
		return ErrInfrastructure
	}
	deleting := false
	if value, exists := metadata["deletionTimestamp"]; exists && value != nil && value != "" {
		if _, ok := value.(string); !ok {
			return ErrInfrastructure
		}
		deleting = true
	}
	if value, exists := metadata["deletionGracePeriodSeconds"]; exists && value != nil {
		if !allowDeletingJob || !deleting {
			return ErrInfrastructure
		}
	}
	if !allowDeletingJob && deleting {
		return ErrInfrastructure
	}
	if value, exists := metadata["finalizers"]; exists && !emptyMetadataList(value) {
		finalizers, ok := value.([]any)
		if !allowDeletingJob || !deleting || !ok || len(finalizers) != 1 || finalizers[0] != "foregroundDeletion" {
			return ErrInfrastructure
		}
	}
	return nil
}

func emptyMetadataList(value any) bool {
	if value == nil {
		return true
	}
	switch typed := value.(type) {
	case []any:
		return len(typed) == 0
	case []string:
		return len(typed) == 0
	default:
		return false
	}
}

func validJobSelector(selector map[string]any, uid string) bool {
	match, _ := selector["matchLabels"].(map[string]any)
	if len(match) < 1 || len(match) > 2 {
		return false
	}
	found := false
	for key, value := range match {
		switch key {
		case "batch.kubernetes.io/controller-uid", "controller-uid":
			if value != uid {
				return false
			}
			found = true
		default:
			return false
		}
	}
	return found
}

func validTemplateLabels(liveMetadata, desiredMetadata map[string]any, uid, jobName string) bool {
	liveLabels, _ := liveMetadata["labels"].(map[string]any)
	desiredLabels, _ := desiredMetadata["labels"].(map[string]any)
	for key, value := range desiredLabels {
		if liveLabels[key] != value {
			return false
		}
	}
	controllerFound, jobFound := false, false
	for key, value := range liveLabels {
		switch key {
		case "batch.kubernetes.io/controller-uid", "controller-uid":
			if value != uid {
				return false
			}
			controllerFound = true
		case "batch.kubernetes.io/job-name", "job-name":
			if value != jobName {
				return false
			}
			jobFound = true
		default:
			if desiredLabels[key] != value {
				return false
			}
		}
	}
	annotations, _ := liveMetadata["annotations"].(map[string]any)
	return controllerFound && jobFound && (len(annotations) == 0 || len(annotations) == 1 && annotations["batch.kubernetes.io/job-tracking"] == "")
}

func stripExactPodDefaults(live, desired map[string]any) error {
	if account, exists := live["serviceAccount"]; exists {
		if account != desired["serviceAccountName"] {
			return fmt.Errorf("service account default: %w", ErrInfrastructure)
		}
		delete(live, "serviceAccount")
	}
	for key, value := range map[string]any{
		"dnsPolicy": "ClusterFirst", "schedulerName": "default-scheduler", "terminationGracePeriodSeconds": int64(30),
	} {
		if actual, exists := live[key]; exists {
			if actual != value {
				return fmt.Errorf("%s default: %w", key, ErrInfrastructure)
			}
			delete(live, key)
		}
	}
	for _, field := range []string{"initContainers", "containers"} {
		liveContainers, _ := live[field].([]any)
		desiredContainers, _ := desired[field].([]any)
		if len(liveContainers) != len(desiredContainers) {
			return fmt.Errorf("%s count: %w", field, ErrInfrastructure)
		}
		for index := range liveContainers {
			actual, _ := liveContainers[index].(map[string]any)
			expected, _ := desiredContainers[index].(map[string]any)
			if actual == nil || expected == nil {
				return fmt.Errorf("%s %d shape: %w", field, index, ErrInfrastructure)
			}
			for _, key := range []string{"terminationMessagePath", "terminationMessagePolicy"} {
				if _, owned := expected[key]; owned {
					continue
				}
				if value, exists := actual[key]; exists {
					want := any("/dev/termination-log")
					if key == "terminationMessagePolicy" {
						want = "File"
					}
					if value != want {
						return fmt.Errorf("%s %d %s default: %w", field, index, key, ErrInfrastructure)
					}
					delete(actual, key)
				}
			}
			if probe, exists := actual["startupProbe"].(map[string]any); exists {
				expectedProbe, _ := expected["startupProbe"].(map[string]any)
				if threshold, defaulted := probe["successThreshold"]; defaulted {
					if threshold != int64(1) || expectedProbe != nil && expectedProbe["successThreshold"] != nil {
						return fmt.Errorf("%s %d startup probe success threshold: %w", field, index, ErrInfrastructure)
					}
					delete(probe, "successThreshold")
				}
			}
			if !equivalentResourceQuantities(actual["resources"], expected["resources"]) {
				return fmt.Errorf("%s %d resources: %w", field, index, ErrInfrastructure)
			}
			actual["resources"] = cloneAny(expected["resources"])
		}
	}
	return nil
}

func equivalentResourceQuantities(actualValue, expectedValue any) bool {
	actual, actualOK := actualValue.(map[string]any)
	expected, expectedOK := expectedValue.(map[string]any)
	if !actualOK || !expectedOK || len(actual) != len(expected) {
		return false
	}
	for _, class := range []string{"requests", "limits"} {
		a, aOK := actual[class].(map[string]any)
		e, eOK := expected[class].(map[string]any)
		if !aOK || !eOK || len(a) != len(e) {
			return false
		}
		for resource, expectedQuantity := range e {
			actualQuantity, ok := a[resource].(string)
			want, wantOK := expectedQuantity.(string)
			if !ok || !wantOK || !quantityEqual(resource, actualQuantity, want) {
				return false
			}
		}
	}
	return true
}

func quantityEqual(resource, one, two string) bool {
	a, ok := parseQuantity(resource, one)
	if !ok {
		return false
	}
	b, ok := parseQuantity(resource, two)
	return ok && a.Cmp(b) == 0
}

func parseQuantity(resource, value string) (*big.Int, bool) {
	multiplier := big.NewInt(1)
	digits := value
	if resource == "cpu" {
		if strings.HasSuffix(value, "m") {
			digits = strings.TrimSuffix(value, "m")
		} else {
			multiplier = big.NewInt(1000)
		}
	} else {
		for suffix, factor := range map[string]int64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40} {
			if strings.HasSuffix(value, suffix) {
				digits = strings.TrimSuffix(value, suffix)
				multiplier = big.NewInt(factor)
				break
			}
		}
	}
	integer := new(big.Int)
	if digits == "" {
		return nil, false
	}
	if _, ok := integer.SetString(digits, 10); !ok || integer.Sign() <= 0 {
		return nil, false
	}
	return integer.Mul(integer, multiplier), true
}

func validateBuildPod(pod, job map[string]any) error {
	if pod["apiVersion"] != "v1" {
		return fmt.Errorf("api version: %w", ErrInfrastructure)
	}
	if pod["kind"] != "Pod" {
		return fmt.Errorf("kind: %w", ErrInfrastructure)
	}
	if objectNamespace(pod) != objectNamespace(job) {
		return fmt.Errorf("namespace: %w", ErrInfrastructure)
	}
	if !kubeNameRE.MatchString(objectName(pod)) {
		return fmt.Errorf("name: %w", ErrInfrastructure)
	}
	jobSpec, _ := job["spec"].(map[string]any)
	jobTemplate, _ := jobSpec["template"].(map[string]any)
	podMetadata, _ := pod["metadata"].(map[string]any)
	jobMetadata, _ := job["metadata"].(map[string]any)
	jobUID, _ := jobMetadata["uid"].(string)
	jobTemplateMetadata, _ := jobTemplate["metadata"].(map[string]any)
	if podMetadata == nil || jobTemplateMetadata == nil || !validTemplateLabels(podMetadata, jobTemplateMetadata, jobUID, objectName(job)) {
		return fmt.Errorf("controller labels: %w", ErrInfrastructure)
	}
	owners, _ := podMetadata["ownerReferences"].([]any)
	if len(owners) != 1 {
		return fmt.Errorf("owner count: %w", ErrInfrastructure)
	}
	owner, _ := owners[0].(map[string]any)
	if owner["apiVersion"] == "batch/v1" && owner["kind"] == "Job" && owner["name"] == objectName(job) && owner["uid"] == jobUID && owner["controller"] == true && owner["blockOwnerDeletion"] == true {
		return nil
	}
	return fmt.Errorf("controller owner: %w", ErrInfrastructure)
}

func observeBuild(attempt BuildAttempt, job map[string]any, pods []map[string]any) WorkloadObservation {
	// The verified agent termination result is the build completion authority
	// once the privileged native sidecar has also stopped. Kubernetes can delay
	// the Job Complete condition after both containers have exited, so waiting
	// only on Job status can hold the build lease and block the queue despite a
	// finished build.
	if len(pods) == 1 && nativeBuildSidecarTerminated(pods[0]) {
		if result, ok := terminationResult(pods[0]); ok {
			return WorkloadObservation{State: WorkloadSucceeded, Result: result, LogReference: buildLogReference(attempt.JobNamespace, objectName(pods[0]))}
		}
	}
	if jobSucceeded(job) {
		if len(pods) != 1 {
			return WorkloadObservation{State: WorkloadFailed, FailureCode: "builder-result-missing"}
		}
		result, ok := terminationResult(pods[0])
		if !ok {
			return WorkloadObservation{State: WorkloadFailed, FailureCode: "builder-result-invalid"}
		}
		return WorkloadObservation{State: WorkloadSucceeded, Result: result, LogReference: buildLogReference(attempt.JobNamespace, objectName(pods[0]))}
	}
	if jobFailed(job) {
		return WorkloadObservation{State: WorkloadFailed, FailureCode: jobFailureCode(job, pods)}
	}
	if jobCounter(job, "active") > 0 || len(pods) == 1 && podPhase(pods[0]) == "Running" {
		return WorkloadObservation{State: WorkloadRunning}
	}
	return WorkloadObservation{State: WorkloadPending}
}

func terminationResult(pod map[string]any) ([]byte, bool) {
	status, _ := pod["status"].(map[string]any)
	containers, _ := status["containerStatuses"].([]any)
	for _, raw := range containers {
		container, _ := raw.(map[string]any)
		if container["name"] != "agent" {
			continue
		}
		state, _ := container["state"].(map[string]any)
		terminated, _ := state["terminated"].(map[string]any)
		message, _ := terminated["message"].(string)
		exitCode, exitOK := integerValue(terminated["exitCode"])
		if !exitOK || exitCode != 0 || message == "" || len(message) >= builder.MaxTerminationResultBytes {
			return nil, false
		}
		result := []byte(message)
		if !json.Valid(result) {
			clear(result)
			return nil, false
		}
		return result, true
	}
	return nil, false
}

func nativeBuildSidecarTerminated(pod map[string]any) bool {
	status, _ := pod["status"].(map[string]any)
	containers, _ := status["initContainerStatuses"].([]any)
	seen := false
	for _, raw := range containers {
		container, _ := raw.(map[string]any)
		if container["name"] != "dind" {
			continue
		}
		if seen {
			return false
		}
		seen = true
		state, _ := container["state"].(map[string]any)
		if _, terminated := state["terminated"].(map[string]any); !terminated {
			return false
		}
	}
	return seen
}

func shouldDeleteSourceSecret(job map[string]any, pods []map[string]any) bool {
	if isTerminalJob(job) {
		return true
	}
	if len(pods) != 1 {
		return false
	}
	status, _ := pods[0]["status"].(map[string]any)
	containers, _ := status["initContainerStatuses"].([]any)
	for _, raw := range containers {
		container, _ := raw.(map[string]any)
		if container["name"] == "checkout" {
			state, _ := container["state"].(map[string]any)
			_, terminated := state["terminated"].(map[string]any)
			return terminated
		}
	}
	return false
}

func isTerminalJob(job map[string]any) bool { return jobSucceeded(job) || jobFailed(job) }

func jobDeletionInProgress(job map[string]any) bool {
	metadata, _ := job["metadata"].(map[string]any)
	value, _ := metadata["deletionTimestamp"].(string)
	return value != ""
}

func jobSucceeded(job map[string]any) bool {
	return jobCondition(job, "Complete") || jobCounter(job, "succeeded") > 0
}

func jobFailed(job map[string]any) bool { return jobCondition(job, "Failed") }

func jobCondition(job map[string]any, wanted string) bool {
	status, _ := job["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] == wanted && condition["status"] == "True" {
			return true
		}
	}
	return false
}

func jobCounter(job map[string]any, key string) int64 {
	status, _ := job["status"].(map[string]any)
	value, _ := integerValue(status[key])
	return value
}

func jobFailureCode(job map[string]any, pods []map[string]any) string {
	status, _ := job["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] == "Failed" && condition["status"] == "True" {
			reason, _ := condition["reason"].(string)
			switch reason {
			case "DeadlineExceeded":
				return "builder-deadline-exceeded"
			case "BackoffLimitExceeded":
				return "builder-backoff-exhausted"
			}
		}
	}
	if len(pods) == 1 {
		reason, _ := nestedString(pods[0], "status", "reason")
		switch reason {
		case "Evicted":
			return "builder-pod-evicted"
		case "NodeLost":
			return "builder-node-lost"
		}
	}
	return "builder-job-failed"
}

func buildLogReference(namespace, pod string) string {
	return "k8s://" + namespace + "/pods/" + pod + "/containers/agent"
}

func podPhase(pod map[string]any) string {
	phase, _ := nestedString(pod, "status", "phase")
	return phase
}

func objectName(object map[string]any) string {
	name, _ := nestedString(object, "metadata", "name")
	return name
}

func objectNamespace(object map[string]any) string {
	namespace, _ := nestedString(object, "metadata", "namespace")
	return namespace
}

func metadataLabels(object map[string]any) map[string]any {
	metadata, _ := object["metadata"].(map[string]any)
	labels, _ := metadata["labels"].(map[string]any)
	return labels
}

func nestedString(object map[string]any, path ...string) (string, bool) {
	var current any = object
	for _, part := range path {
		mapping, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = mapping[part]
		if !ok {
			return "", false
		}
	}
	value, ok := current.(string)
	return value, ok
}

func integerValue(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func decodeSecretValue(value any, maximum int) ([]byte, bool) {
	encoded, ok := value.(string)
	if !ok || len(encoded) > base64.StdEncoding.EncodedLen(maximum) {
		return nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 || len(decoded) > maximum {
		clear(decoded)
		return nil, false
	}
	return decoded, true
}

func clearSecretData(secret map[string]any) {
	if secret == nil {
		return
	}
	for _, field := range []string{"data", "stringData"} {
		values, _ := secret[field].(map[string]any)
		for key, raw := range values {
			if bytes, ok := raw.([]byte); ok {
				clear(bytes)
			}
			values[key] = nil
			delete(values, key)
		}
		delete(secret, field)
	}
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	return cloneAny(input).(map[string]any)
}

func cloneAny(input any) any {
	switch value := input.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			result[key] = cloneAny(item)
		}
		return result
	case []any:
		result := make([]any, len(value))
		for index, item := range value {
			result[index] = cloneAny(item)
		}
		return result
	case []byte:
		return slices.Clone(value)
	default:
		return value
	}
}

func normalizeGoIntegers(input any) any {
	switch value := input.(type) {
	case map[string]any:
		for key, item := range value {
			value[key] = normalizeGoIntegers(item)
		}
		return value
	case []any:
		for index, item := range value {
			value[index] = normalizeGoIntegers(item)
		}
		return value
	case int:
		return int64(value)
	case int32:
		return int64(value)
	default:
		return value
	}
}
