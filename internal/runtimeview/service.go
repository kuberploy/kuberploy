package runtimeview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

var allowedSelectorKeys = map[string]struct{}{
	"app.kubernetes.io/name":       {},
	"app.kubernetes.io/instance":   {},
	"app.kubernetes.io/managed-by": {},
	"kuberploy.io/application":     {},
	"kuberploy.io/application-id":  {},
	"kuberploy.io/project":         {},
	"kuberploy.io/environment":     {},
	"kuberploy.io/service":         {},
}

type Service struct {
	resolver Resolver
	client   KubernetesClient
	redactor Redactor
	config   Config
	now      func() time.Time
}

func NewService(resolver Resolver, client KubernetesClient, redactor Redactor, config Config) (*Service, error) {
	if resolver == nil || client == nil {
		return nil, ErrInvalidRequest
	}
	security := client.Security()
	if !security.TLSVerified || security.InsecureSkipTLSVerify {
		return nil, ErrInsecureTransport
	}
	if redactor == nil {
		redactor = NewDefenseInDepthRedactor()
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Service{resolver: resolver, client: client, redactor: redactor, config: config, now: func() time.Time { return time.Now().UTC() }}, nil
}

func validateConfig(c Config) error {
	if c.DefaultTailLines <= 0 || c.MaxTailLines < c.DefaultTailLines ||
		c.DefaultLimitBytes <= 0 || c.MaxSourceBytes < c.DefaultLimitBytes ||
		c.MaxSnapshotBytes <= 0 || c.MaxLineBytes <= 0 || c.MaxSources <= 0 ||
		c.MaxLookback <= 0 || c.MaxEvents <= 0 || c.MaxEventMessageBytes <= 0 ||
		c.FollowBuffer <= 0 || c.MaxFollowBytes <= 0 || c.MaxFollowDuration <= 0 ||
		c.RevalidateInterval <= 0 || c.HeartbeatInterval <= 0 || c.RediscoverInterval <= 0 ||
		c.ReconnectDelay <= 0 || c.ReconnectOverlap < 0 || c.DedupeEntries <= 0 {
		return ErrInvalidRequest
	}
	if c.DefaultLimitBytes > int64(c.MaxSnapshotBytes) || c.MaxLineBytes > int(c.MaxSourceBytes) {
		return ErrInvalidRequest
	}
	return nil
}

func (s *Service) ensureSecure() error {
	security := s.client.Security()
	if !security.TLSVerified || security.InsecureSkipTLSVerify {
		return ErrInsecureTransport
	}
	return nil
}

func (s *Service) normalizeOptions(options LogOptions, follow bool) (LogOptions, error) {
	if err := s.ensureSecure(); err != nil {
		return LogOptions{}, err
	}
	if options.Follow != follow {
		return LogOptions{}, ErrInvalidRequest
	}
	if options.Container != "" && !containerPattern.MatchString(options.Container) {
		return LogOptions{}, ErrInvalidRequest
	}
	if options.Pod != "" && !kubeNamePattern.MatchString(options.Pod) {
		return LogOptions{}, ErrInvalidRequest
	}
	if options.Revision != "" && !revisionPattern.MatchString(options.Revision) {
		return LogOptions{}, ErrInvalidRequest
	}
	if options.TailLines == 0 {
		options.TailLines = s.config.DefaultTailLines
	}
	if options.TailLines < 1 || options.TailLines > s.config.MaxTailLines {
		return LogOptions{}, ErrInvalidRequest
	}
	if options.LimitBytes == 0 {
		options.LimitBytes = s.config.DefaultLimitBytes
	}
	if options.LimitBytes < 1 || options.LimitBytes > s.config.MaxSourceBytes {
		return LogOptions{}, ErrInvalidRequest
	}
	if options.SinceTime != nil {
		since := options.SinceTime.UTC()
		now := s.now()
		if since.After(now.Add(time.Minute)) || since.Before(now.Add(-s.config.MaxLookback)) {
			return LogOptions{}, ErrInvalidRequest
		}
		options.SinceTime = &since
	}
	return options, nil
}

func validateOpaqueTarget(target OpaqueTarget) error {
	if (target.Kind != TargetApplication && target.Kind != TargetDeployment) || !opaqueIDPattern.MatchString(target.ID) {
		return ErrInvalidRequest
	}
	return nil
}

func (s *Service) resolve(ctx context.Context, requested OpaqueTarget) (AuthorizedTarget, error) {
	if err := validateOpaqueTarget(requested); err != nil {
		return AuthorizedTarget{}, err
	}
	target, err := s.resolver.Resolve(ctx, requested)
	if err != nil {
		return AuthorizedTarget{}, err
	}
	if target.Reference != requested || !opaqueIDPattern.MatchString(target.ApplicationID) ||
		!kubeNamePattern.MatchString(target.Namespace) || len(target.Deployments) == 0 ||
		(requested.Kind == TargetDeployment && len(target.Deployments) != 1) {
		return AuthorizedTarget{}, ErrScopeViolation
	}
	seenNames := make(map[string]struct{}, len(target.Deployments))
	seenUIDs := make(map[string]struct{}, len(target.Deployments))
	for _, deployment := range target.Deployments {
		if !kubeNamePattern.MatchString(deployment.Name) || !uidPattern.MatchString(deployment.UID) {
			return AuthorizedTarget{}, ErrScopeViolation
		}
		if _, duplicate := seenNames[deployment.Name]; duplicate {
			return AuthorizedTarget{}, ErrScopeViolation
		}
		if _, duplicate := seenUIDs[deployment.UID]; duplicate {
			return AuthorizedTarget{}, ErrScopeViolation
		}
		seenNames[deployment.Name] = struct{}{}
		seenUIDs[deployment.UID] = struct{}{}
	}
	target.Deployments = slices.Clone(target.Deployments)
	return target, nil
}

type resolvedDeployment struct {
	ref      DeploymentRef
	resource Deployment
}

type resolvedReplicaSet struct {
	resource ReplicaSet
	parent   resolvedDeployment
}

type resolvedPod struct {
	resource Pod
	parent   resolvedReplicaSet
}

type runtimeGraph struct {
	target      AuthorizedTarget
	deployments []resolvedDeployment
	replicaSets []resolvedReplicaSet
	pods        []resolvedPod
}

func (s *Service) discoverGraph(ctx context.Context, target AuthorizedTarget) (runtimeGraph, error) {
	graph := runtimeGraph{target: target}
	podUIDs := map[string]struct{}{}
	for _, deploymentRef := range target.Deployments {
		deployment, err := s.client.GetDeployment(ctx, target.Namespace, deploymentRef.Name)
		if err != nil {
			return runtimeGraph{}, err
		}
		if deployment.Namespace != target.Namespace || deployment.Name != deploymentRef.Name {
			return runtimeGraph{}, ErrScopeViolation
		}
		if deployment.UID != deploymentRef.UID {
			return runtimeGraph{}, ErrGone
		}
		if err := validateSelector(deployment.Selector, target.ApplicationID); err != nil {
			return runtimeGraph{}, err
		}
		resolvedDeployment := resolvedDeployment{ref: deploymentRef, resource: deployment}
		graph.deployments = append(graph.deployments, resolvedDeployment)

		replicaSets, err := s.client.ListReplicaSets(ctx, target.Namespace, cloneSelector(deployment.Selector))
		if err != nil {
			return runtimeGraph{}, err
		}
		acceptedReplicaSets := make(map[string]resolvedReplicaSet)
		for _, replicaSet := range replicaSets {
			if replicaSet.Namespace != target.Namespace {
				return runtimeGraph{}, ErrScopeViolation
			}
			if !controlledBy(replicaSet.Owners, "Deployment", deployment.UID) {
				continue
			}
			if !kubeNamePattern.MatchString(replicaSet.Name) || !uidPattern.MatchString(replicaSet.UID) ||
				replicaSet.Revision != "" && !revisionPattern.MatchString(replicaSet.Revision) {
				return runtimeGraph{}, ErrScopeViolation
			}
			resolved := resolvedReplicaSet{resource: replicaSet, parent: resolvedDeployment}
			acceptedReplicaSets[replicaSet.UID] = resolved
			graph.replicaSets = append(graph.replicaSets, resolved)
		}

		pods, err := s.client.ListPods(ctx, target.Namespace, cloneSelector(deployment.Selector))
		if err != nil {
			return runtimeGraph{}, err
		}
		for _, pod := range pods {
			if pod.Namespace != target.Namespace {
				return runtimeGraph{}, ErrScopeViolation
			}
			ownerUID, ok := controllerUID(pod.Owners, "ReplicaSet")
			if !ok {
				continue
			}
			parent, accepted := acceptedReplicaSets[ownerUID]
			if !accepted {
				continue
			}
			if !kubeNamePattern.MatchString(pod.Name) || !uidPattern.MatchString(pod.UID) {
				return runtimeGraph{}, ErrScopeViolation
			}
			if _, duplicate := podUIDs[pod.UID]; duplicate {
				return runtimeGraph{}, ErrScopeViolation
			}
			podUIDs[pod.UID] = struct{}{}
			graph.pods = append(graph.pods, resolvedPod{resource: clonePod(pod), parent: parent})
		}
	}
	sort.Slice(graph.pods, func(i, j int) bool {
		if graph.pods[i].resource.Name != graph.pods[j].resource.Name {
			return graph.pods[i].resource.Name < graph.pods[j].resource.Name
		}
		return graph.pods[i].resource.UID < graph.pods[j].resource.UID
	})
	return graph, nil
}

func controlledBy(owners []OwnerReference, kind, uid string) bool {
	for _, owner := range owners {
		if owner.Controller && owner.Kind == kind && owner.UID == uid {
			return true
		}
	}
	return false
}

func controllerUID(owners []OwnerReference, kind string) (string, bool) {
	var found string
	for _, owner := range owners {
		if !owner.Controller || owner.Kind != kind {
			continue
		}
		if found != "" {
			return "", false
		}
		found = owner.UID
	}
	return found, found != ""
}

func validateSelector(selector LabelSelector, applicationID string) error {
	if len(selector.MatchLabels) == 0 || len(selector.MatchLabels) > 8 {
		return ErrSelectorNotAllowed
	}
	for key, value := range selector.MatchLabels {
		if _, allowed := allowedSelectorKeys[key]; !allowed || !validLabelValue(value) {
			return ErrSelectorNotAllowed
		}
	}
	if value, ok := selector.MatchLabels["kuberploy.io/application"]; !ok || value != applicationID {
		return ErrSelectorNotAllowed
	}
	return nil
}

func validLabelValue(value string) bool {
	if len(value) < 1 || len(value) > 63 {
		return false
	}
	for i, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || ((character == '-' || character == '_' || character == '.') && i > 0 && i < len(value)-1) {
			continue
		}
		return false
	}
	return true
}

func cloneSelector(selector LabelSelector) LabelSelector {
	copyLabels := make(map[string]string, len(selector.MatchLabels))
	for key, value := range selector.MatchLabels {
		copyLabels[key] = value
	}
	return LabelSelector{MatchLabels: copyLabels}
}

func clonePod(pod Pod) Pod {
	pod.Owners = slices.Clone(pod.Owners)
	pod.Containers = slices.Clone(pod.Containers)
	return pod
}

type sourceBinding struct {
	source        LogSource
	namespace     string
	podUID        string
	podName       string
	replicaSetUID string
}

func (s *Service) sources(graph runtimeGraph, options LogOptions) ([]sourceBinding, error) {
	result := make([]sourceBinding, 0, len(graph.pods))
	for _, pod := range graph.pods {
		if options.Pod != "" && pod.resource.Name != options.Pod {
			continue
		}
		if options.Revision != "" && pod.parent.resource.Revision != options.Revision {
			continue
		}
		container, err := selectContainer(pod.resource, options.Container, options.Previous)
		if err != nil {
			return nil, err
		}
		source := LogSource{
			ID:            sourceID(pod.resource.UID, container.Name, container.RestartCount, options.Previous),
			PodName:       pod.resource.Name,
			Container:     container.Name,
			ContainerKind: container.Kind,
			RestartCount:  container.RestartCount,
			Revision:      pod.parent.resource.Revision,
			Ready:         pod.resource.Ready,
			Terminating:   pod.resource.Terminating,
			Previous:      options.Previous,
		}
		result = append(result, sourceBinding{source: source, namespace: graph.target.Namespace, podUID: pod.resource.UID, podName: pod.resource.Name, replicaSetUID: pod.parent.resource.UID})
	}
	if len(result) == 0 && (options.Pod != "" || options.Revision != "") {
		return nil, ErrSourceNotFound
	}
	if len(result) > s.config.MaxSources {
		return nil, &TooManySourcesError{Count: len(result), Limit: s.config.MaxSources}
	}
	return result, nil
}

func selectContainer(pod Pod, requested string, previous bool) (Container, error) {
	if requested != "" {
		for _, container := range pod.Containers {
			if container.Name == requested && (container.Kind == ContainerRegular || container.Kind == ContainerInit) {
				if previous && container.RestartCount < 1 {
					return Container{}, ErrPreviousUnavailable
				}
				return container, nil
			}
		}
		return Container{}, ErrContainerNotFound
	}
	regular := make([]Container, 0, len(pod.Containers))
	for _, container := range pod.Containers {
		if container.Kind == ContainerRegular {
			regular = append(regular, container)
		}
	}
	if len(regular) != 1 {
		return Container{}, ErrContainerRequired
	}
	if previous && regular[0].RestartCount < 1 {
		return Container{}, ErrPreviousUnavailable
	}
	return regular[0], nil
}

func sourceID(podUID, container string, restartCount int32, previous bool) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%t", podUID, container, restartCount, previous)))
	return "src_" + hex.EncodeToString(digest[:16])
}

func (s *Service) openSource(ctx context.Context, binding sourceBinding, options LogOptions, forceTimestamps bool) (ioReadCloser, error) {
	pod, err := s.client.GetPod(ctx, binding.namespace, binding.podName)
	if err != nil {
		return nil, err
	}
	if pod.Namespace != binding.namespace || pod.Name != binding.podName {
		return nil, ErrScopeViolation
	}
	if pod.UID != binding.podUID {
		return nil, ErrGone
	}
	if !controlledBy(pod.Owners, "ReplicaSet", binding.replicaSetUID) {
		return nil, ErrScopeViolation
	}
	container, err := selectContainer(pod, binding.source.Container, options.Previous)
	if err != nil {
		return nil, err
	}
	if container.Kind != binding.source.ContainerKind || container.RestartCount != binding.source.RestartCount {
		return nil, ErrGone
	}
	kubeOptions := PodLogOptions{
		Container:  binding.source.Container,
		TailLines:  options.TailLines,
		SinceTime:  options.SinceTime,
		Previous:   options.Previous,
		Timestamps: options.Timestamps || forceTimestamps,
		LimitBytes: options.LimitBytes,
		Follow:     options.Follow,
	}
	reader, err := s.client.OpenPodLogs(ctx, PodLogRequest{Namespace: binding.namespace, PodName: binding.podName, PodUID: binding.podUID, Options: kubeOptions})
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, errors.New("runtime log reader is unavailable")
	}
	return reader, nil
}

// ioReadCloser is kept as a local alias so the open/re-read invariant remains
// obvious at call sites without broadening KubernetesClient.
type ioReadCloser interface {
	Read([]byte) (int, error)
	Close() error
}

func terminalCode(err error) string {
	switch {
	case errors.Is(err, ErrUnauthorized):
		return "AuthorizationRevoked"
	case errors.Is(err, ErrGone):
		return "ObjectReplaced"
	case errors.Is(err, ErrTooManySources):
		return "TooManySources"
	case errors.Is(err, context.Canceled):
		return "Cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "SessionExpired"
	default:
		return "LogsUnavailable"
	}
}

func safeTerminalDetail(code string) string {
	switch code {
	case "AuthorizationRevoked":
		return "Log access was revoked."
	case "ObjectReplaced":
		return "A resolved Kubernetes object was replaced."
	case "TooManySources":
		return "The workload exceeds the configured source limit."
	case "SessionExpired":
		return "The bounded log session ended."
	case "Cancelled":
		return "The log session was cancelled."
	default:
		return "The Kubernetes log source is unavailable."
	}
}

func safeReason(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "Unknown"
	}
	var builder strings.Builder
	for _, character := range value {
		if builder.Len() >= 128 {
			break
		}
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			builder.WriteRune(character)
		}
	}
	if builder.Len() == 0 {
		return "Unknown"
	}
	return builder.String()
}

func eventType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "normal":
		return "Normal"
	case "warning":
		return "Warning"
	default:
		return "Unknown"
	}
}
