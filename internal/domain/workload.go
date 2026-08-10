package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	DefaultCPURequest    = "50m"
	DefaultMemoryRequest = "100Mi"
)

type WorkloadRuntime struct {
	Replicas                      int                        `json:"replicas"`
	Command                       []string                   `json:"command,omitempty"`
	Args                          []string                   `json:"args,omitempty"`
	TerminationGracePeriodSeconds *int                       `json:"terminationGracePeriodSeconds,omitempty"`
	Ports                         []WorkloadPort             `json:"ports"`
	Env                           []WorkloadEnv              `json:"env,omitempty"`
	Resources                     WorkloadResources          `json:"resources"`
	SchedulingProfile             *SchedulingProfileRef      `json:"schedulingProfile,omitempty"`
	NodeSelector                  map[string]string          `json:"nodeSelector,omitempty"`
	Affinity                      *WorkloadAffinity          `json:"affinity,omitempty"`
	TopologySpreadConstraints     []TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	Tolerations                   []WorkloadToleration       `json:"tolerations,omitempty"`
	PriorityClassName             string                     `json:"priorityClassName,omitempty"`
	Probes                        *WorkloadProbes            `json:"probes,omitempty"`
}

// SchedulingProfileRef is the immutable, non-secret scheduling authority
// selected by a workload caller. The effective Pod fields beside it are
// server-derived and must match this exact revision and both durable digests.
type SchedulingProfileRef struct {
	ProfileID         string `json:"profileId"`
	Revision          int64  `json:"revision"`
	SpecDigest        string `json:"specDigest"`
	AssignmentsDigest string `json:"assignmentsDigest"`
}

type WorkloadPort struct {
	Name          string `json:"name"`
	ContainerPort int    `json:"containerPort"`
	ServicePort   int    `json:"servicePort,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
}

type WorkloadEnv struct {
	Name      string                `json:"name"`
	Value     *string               `json:"value,omitempty"`
	ValueFrom *WorkloadEnvValueFrom `json:"valueFrom,omitempty"`
}

type WorkloadEnvValueFrom struct {
	SecretBindingRef SecretBindingRef `json:"secretBindingRef"`
}

type SecretBindingRef struct {
	BindingID string `json:"bindingId"`
	Name      string `json:"name"`
	Key       string `json:"key"`
	Version   int64  `json:"version"`
}

func (r SecretBindingRef) Valid() bool {
	return uuidPattern.MatchString(r.BindingID) && validDNSLabel(r.Name) && len(r.Key) <= 253 &&
		secretPartPattern.MatchString(r.Key) && r.Version > 0
}

type WorkloadResources struct {
	Requests ResourceList  `json:"requests"`
	Limits   *ResourceList `json:"limits,omitempty"`
}

type ResourceList struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

type WorkloadAffinity struct {
	NodeAffinity    *NodeAffinity `json:"nodeAffinity,omitempty"`
	PodAffinity     *PodAffinity  `json:"podAffinity,omitempty"`
	PodAntiAffinity *PodAffinity  `json:"podAntiAffinity,omitempty"`
}

type NodeAffinity struct {
	RequiredDuringSchedulingIgnoredDuringExecution  *NodeSelector             `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
	PreferredDuringSchedulingIgnoredDuringExecution []PreferredSchedulingTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

type NodeSelector struct {
	NodeSelectorTerms []NodeSelectorTerm `json:"nodeSelectorTerms"`
}

type NodeSelectorTerm struct {
	MatchExpressions []NodeSelectorRequirement `json:"matchExpressions"`
}

type NodeSelectorRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values,omitempty"`
}

type PreferredSchedulingTerm struct {
	Weight     int              `json:"weight"`
	Preference NodeSelectorTerm `json:"preference"`
}

type PodAffinity struct {
	RequiredDuringSchedulingIgnoredDuringExecution  []PodAffinityTerm         `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
	PreferredDuringSchedulingIgnoredDuringExecution []WeightedPodAffinityTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

type PodAffinityTerm struct {
	LabelSelector LabelSelector `json:"labelSelector"`
	TopologyKey   string        `json:"topologyKey"`
}

type WeightedPodAffinityTerm struct {
	Weight          int             `json:"weight"`
	PodAffinityTerm PodAffinityTerm `json:"podAffinityTerm"`
}

type LabelSelector struct {
	MatchLabels      map[string]string          `json:"matchLabels,omitempty"`
	MatchExpressions []LabelSelectorRequirement `json:"matchExpressions,omitempty"`
}

type LabelSelectorRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values,omitempty"`
}

type TopologySpreadConstraint struct {
	MaxSkew            int           `json:"maxSkew"`
	TopologyKey        string        `json:"topologyKey"`
	WhenUnsatisfiable  string        `json:"whenUnsatisfiable"`
	LabelSelector      LabelSelector `json:"labelSelector"`
	MinDomains         *int          `json:"minDomains,omitempty"`
	NodeAffinityPolicy string        `json:"nodeAffinityPolicy,omitempty"`
	NodeTaintsPolicy   string        `json:"nodeTaintsPolicy,omitempty"`
}

type WorkloadToleration struct {
	Key               string `json:"key"`
	Operator          string `json:"operator"`
	Value             string `json:"value,omitempty"`
	Effect            string `json:"effect"`
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`
}

// WorkloadProbes contains the three Kubernetes probe phases exposed by the
// guided editor. Probe actions remain a closed subset: arbitrary HTTP headers,
// host overrides and gRPC endpoints are deliberately not accepted in P0.
type WorkloadProbes struct {
	Startup   *WorkloadProbe `json:"startup,omitempty"`
	Readiness *WorkloadProbe `json:"readiness,omitempty"`
	Liveness  *WorkloadProbe `json:"liveness,omitempty"`
}

type WorkloadProbe struct {
	HTTPGet             *WorkloadHTTPGetAction   `json:"httpGet,omitempty"`
	TCPSocket           *WorkloadTCPSocketAction `json:"tcpSocket,omitempty"`
	Exec                *WorkloadExecAction      `json:"exec,omitempty"`
	InitialDelaySeconds *int                     `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       *int                     `json:"periodSeconds,omitempty"`
	TimeoutSeconds      *int                     `json:"timeoutSeconds,omitempty"`
	SuccessThreshold    *int                     `json:"successThreshold,omitempty"`
	FailureThreshold    *int                     `json:"failureThreshold,omitempty"`
}

type WorkloadHTTPGetAction struct {
	Path   string            `json:"path"`
	Port   WorkloadProbePort `json:"port"`
	Scheme string            `json:"scheme,omitempty"`
}

type WorkloadTCPSocketAction struct {
	Port WorkloadProbePort `json:"port"`
}

type WorkloadExecAction struct {
	Command []string `json:"command"`
}

// WorkloadProbePort is Kubernetes' IntOrString port without exposing a loose
// any value to the rest of the domain. Exactly one of Name or Number is set.
type WorkloadProbePort struct {
	Name   string
	Number int
}

func (p WorkloadProbePort) MarshalJSON() ([]byte, error) {
	if p.Name != "" && p.Number == 0 {
		return json.Marshal(p.Name)
	}
	if p.Name == "" && p.Number >= 1 && p.Number <= 65535 {
		return json.Marshal(p.Number)
	}
	return nil, fmt.Errorf("invalid workload probe port")
}

func (p *WorkloadProbePort) UnmarshalJSON(raw []byte) error {
	if p == nil {
		return fmt.Errorf("invalid workload probe port")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("invalid workload probe port")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("invalid workload probe port")
	}
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return fmt.Errorf("invalid workload probe port")
		}
		p.Name, p.Number = typed, 0
	case float64:
		if typed < 1 || typed > 65535 || typed != float64(int(typed)) {
			return fmt.Errorf("invalid workload probe port")
		}
		p.Name, p.Number = "", int(typed)
	default:
		return fmt.Errorf("invalid workload probe port")
	}
	return nil
}

type WorkloadValidationError struct {
	Pointer string
	Code    string
	Detail  string
}

var (
	portNamePattern   = regexp.MustCompile(`^[a-z](?:[-a-z0-9]*[a-z0-9])?$`)
	envNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretPartPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	uuidPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	labelNamePattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[-_.A-Za-z0-9]*[A-Za-z0-9])?$`)
	dnsLabelPattern   = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
	sha256Pattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

func DefaultWorkloadRuntime(port int, ordinary map[string]string) WorkloadRuntime {
	env := make([]WorkloadEnv, 0, len(ordinary))
	keys := make([]string, 0, len(ordinary))
	for key := range ordinary {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := ordinary[key]
		env = append(env, WorkloadEnv{Name: key, Value: &value})
	}
	return WorkloadRuntime{
		Replicas:  1,
		Ports:     []WorkloadPort{{Name: "http", ContainerPort: port}},
		Env:       env,
		Resources: WorkloadResources{Requests: ResourceList{CPU: DefaultCPURequest, Memory: DefaultMemoryRequest}},
	}
}

func RuntimeForCreateDeployment(input CreateDeployment) WorkloadRuntime {
	runtime := input.Runtime
	if len(runtime.Ports) == 0 {
		runtime = DefaultWorkloadRuntime(input.Port, input.Environment)
		runtime.Replicas = input.Replicas
	}
	return NormalizeWorkloadRuntime(runtime)
}

func LegacyWorkloadFields(runtime WorkloadRuntime) (int, int, map[string]string) {
	port := 0
	if len(runtime.Ports) > 0 {
		port = runtime.Ports[0].ContainerPort
	}
	ordinary := make(map[string]string)
	for _, variable := range runtime.Env {
		if variable.Value != nil {
			ordinary[variable.Name] = *variable.Value
		}
	}
	return runtime.Replicas, port, ordinary
}

func NormalizeWorkloadRuntime(runtime WorkloadRuntime) WorkloadRuntime {
	if runtime.Replicas == 0 {
		runtime.Replicas = 1
	}
	if runtime.Resources.Requests.CPU == "" {
		runtime.Resources.Requests.CPU = DefaultCPURequest
	}
	if runtime.Resources.Requests.Memory == "" {
		runtime.Resources.Requests.Memory = DefaultMemoryRequest
	}
	for index := range runtime.Ports {
		if runtime.Ports[index].Protocol == "" {
			runtime.Ports[index].Protocol = "TCP"
		}
	}
	for index := range runtime.Env {
		runtime.Env[index].Name = strings.TrimSpace(runtime.Env[index].Name)
	}
	return runtime
}

func ValidateWorkloadRuntime(runtime WorkloadRuntime) []WorkloadValidationError {
	var problems []WorkloadValidationError
	add := func(pointer, code, detail string) {
		problems = append(problems, WorkloadValidationError{Pointer: pointer, Code: code, Detail: detail})
	}
	if runtime.SchedulingProfile != nil {
		ref := runtime.SchedulingProfile
		if !uuidPattern.MatchString(ref.ProfileID) {
			add("/runtime/schedulingProfile/profileId", "InvalidUUID", "profileId must identify an immutable scheduling profile")
		}
		if ref.Revision < 1 {
			add("/runtime/schedulingProfile/revision", "OutOfRange", "revision must be at least 1")
		}
		if !sha256Pattern.MatchString(ref.SpecDigest) {
			add("/runtime/schedulingProfile/specDigest", "InvalidDigest", "specDigest must be an exact lowercase SHA-256 digest")
		}
		if !sha256Pattern.MatchString(ref.AssignmentsDigest) {
			add("/runtime/schedulingProfile/assignmentsDigest", "InvalidDigest", "assignmentsDigest must be an exact lowercase SHA-256 digest")
		}
	}
	if runtime.PriorityClassName != "" && !validDNSLabel(runtime.PriorityClassName) {
		add("/runtime/priorityClassName", "InvalidDNSLabel", "priorityClassName must be a Kubernetes DNS label")
	}
	if runtime.Replicas < 1 || runtime.Replicas > 100 {
		add("/runtime/replicas", "OutOfRange", "replicas must be between 1 and 100")
	}
	commandBytes := validateRuntimeCommandVector(runtime.Command, 64, "/runtime/command", add)
	argumentBytes := validateRuntimeCommandVector(runtime.Args, 128, "/runtime/args", add)
	if commandBytes+argumentBytes > 64<<10 {
		add("/runtime", "CommandTooLong", "container command and arguments are limited to 65536 bytes in total")
	}
	if runtime.TerminationGracePeriodSeconds != nil && (*runtime.TerminationGracePeriodSeconds < 1 || *runtime.TerminationGracePeriodSeconds > 3600) {
		add("/runtime/terminationGracePeriodSeconds", "OutOfRange", "termination grace period must be between 1 and 3600 seconds")
	}
	if len(runtime.Ports) < 1 || len(runtime.Ports) > 32 {
		add("/runtime/ports", "OutOfRange", "configure between 1 and 32 ports")
	}
	portNames := map[string]struct{}{}
	for index, port := range runtime.Ports {
		pointer := fmt.Sprintf("/runtime/ports/%d", index)
		if len(port.Name) > 15 || !portNamePattern.MatchString(port.Name) {
			add(pointer+"/name", "InvalidPortName", "use a unique lowercase Kubernetes port name of at most 15 characters")
		}
		if _, exists := portNames[port.Name]; exists {
			add(pointer+"/name", "Duplicate", "port names must be unique")
		}
		portNames[port.Name] = struct{}{}
		if port.ContainerPort < 1 || port.ContainerPort > 65535 || port.ServicePort < 0 || port.ServicePort > 65535 {
			add(pointer, "InvalidPort", "containerPort and optional servicePort must be between 1 and 65535")
		}
		if port.Protocol != "TCP" && port.Protocol != "UDP" {
			add(pointer+"/protocol", "InvalidProtocol", "protocol must be TCP or UDP")
		}
	}
	if len(runtime.Env) > 256 {
		add("/runtime/env", "OutOfRange", "at most 256 environment entries are allowed")
	}
	envNames := map[string]struct{}{}
	for index, variable := range runtime.Env {
		pointer := fmt.Sprintf("/runtime/env/%d", index)
		if len(variable.Name) > 128 || !envNamePattern.MatchString(variable.Name) {
			add(pointer+"/name", "InvalidEnvironmentName", "use a POSIX-style name of at most 128 characters")
		}
		if _, exists := envNames[variable.Name]; exists {
			add(pointer+"/name", "Duplicate", "environment names must be unique")
		}
		envNames[variable.Name] = struct{}{}
		if (variable.Value == nil) == (variable.ValueFrom == nil) {
			add(pointer, "ExactlyOneRequired", "set exactly one of value or valueFrom.secretBindingRef")
			continue
		}
		if variable.Value != nil && len(*variable.Value) > 4096 {
			add(pointer+"/value", "TooLong", "ordinary ConfigMap values are limited to 4096 bytes")
		}
		if variable.ValueFrom != nil {
			ref := variable.ValueFrom.SecretBindingRef
			if !ref.Valid() {
				add(pointer+"/valueFrom/secretBindingRef", "InvalidSecretBindingRef", "reference an existing binding by immutable UUID, reviewed DNS name, key, and positive integer version; secret values are not accepted here")
			}
		}
	}
	requestCPU, ok := cpuMilli(runtime.Resources.Requests.CPU)
	if !ok {
		add("/runtime/resources/requests/cpu", "InvalidQuantity", "CPU must be positive, at most 1000 cores, and use cores or millicores")
	}
	requestMemory, memoryOK := memoryBytes(runtime.Resources.Requests.Memory)
	if !memoryOK {
		add("/runtime/resources/requests/memory", "InvalidQuantity", "memory must be positive, at most 64Ti, and use Ki, Mi, Gi, or Ti")
	}
	if runtime.Resources.Limits != nil {
		if runtime.Resources.Limits.CPU == "" && runtime.Resources.Limits.Memory == "" {
			add("/runtime/resources/limits", "Required", "configure at least one CPU or memory limit")
		}
		if runtime.Resources.Limits.CPU != "" {
			limitCPU, limitCPUOK := cpuMilli(runtime.Resources.Limits.CPU)
			if !limitCPUOK {
				add("/runtime/resources/limits/cpu", "InvalidQuantity", "CPU must be positive, at most 1000 cores, and use cores or millicores")
			} else if ok && limitCPU < requestCPU {
				add("/runtime/resources/limits/cpu", "LimitBelowRequest", "CPU limit must be greater than or equal to its request")
			}
		}
		if runtime.Resources.Limits.Memory != "" {
			limitMemory, limitMemoryOK := memoryBytes(runtime.Resources.Limits.Memory)
			if !limitMemoryOK {
				add("/runtime/resources/limits/memory", "InvalidQuantity", "memory must be positive, at most 64Ti, and use Ki, Mi, Gi, or Ti")
			} else if memoryOK && limitMemory < requestMemory {
				add("/runtime/resources/limits/memory", "LimitBelowRequest", "memory limit must be greater than or equal to its request")
			}
		}
	}
	validateNodeSelector(runtime.NodeSelector, "/runtime/nodeSelector", add)
	if runtime.Affinity != nil {
		validateAffinity(*runtime.Affinity, add)
	}
	if len(runtime.TopologySpreadConstraints) > 16 {
		add("/runtime/topologySpreadConstraints", "OutOfRange", "at most 16 topology spread constraints are allowed")
	}
	for index, constraint := range runtime.TopologySpreadConstraints {
		pointer := fmt.Sprintf("/runtime/topologySpreadConstraints/%d", index)
		if constraint.MaxSkew < 1 || constraint.MaxSkew > 100 {
			add(pointer+"/maxSkew", "OutOfRange", "maxSkew must be between 1 and 100")
		}
		if !validLabelKey(constraint.TopologyKey) {
			add(pointer+"/topologyKey", "InvalidLabelKey", "topologyKey must be a Kubernetes label key")
		}
		if reservedSchedulingKey(constraint.TopologyKey) {
			add(pointer+"/topologyKey", "ReservedSchedulingKey", "system and builder placement labels require an administrator-managed scheduling profile")
		}
		if constraint.WhenUnsatisfiable != "DoNotSchedule" && constraint.WhenUnsatisfiable != "ScheduleAnyway" {
			add(pointer+"/whenUnsatisfiable", "InvalidValue", "use DoNotSchedule or ScheduleAnyway")
		}
		validateLabelSelector(constraint.LabelSelector, pointer+"/labelSelector", add)
		if constraint.MinDomains != nil && (*constraint.MinDomains < 1 || *constraint.MinDomains > 1000) {
			add(pointer+"/minDomains", "OutOfRange", "minDomains must be between 1 and 1000")
		}
		for _, policy := range []struct{ value, suffix string }{{constraint.NodeAffinityPolicy, "nodeAffinityPolicy"}, {constraint.NodeTaintsPolicy, "nodeTaintsPolicy"}} {
			if policy.value != "" && policy.value != "Honor" && policy.value != "Ignore" {
				add(pointer+"/"+policy.suffix, "InvalidValue", "use Honor or Ignore")
			}
		}
	}
	if len(runtime.Tolerations) > 32 {
		add("/runtime/tolerations", "OutOfRange", "at most 32 tolerations are allowed")
	}
	for index, toleration := range runtime.Tolerations {
		pointer := fmt.Sprintf("/runtime/tolerations/%d", index)
		if !validLabelKey(toleration.Key) {
			add(pointer+"/key", "InvalidLabelKey", "a specific taint key is required; broad all-taint tolerations are not allowed")
		}
		if reservedSchedulingKey(toleration.Key) {
			add(pointer+"/key", "ReservedSchedulingKey", "system, control-plane, and builder taints cannot be tolerated by a service")
		}
		if toleration.Operator != "Equal" && toleration.Operator != "Exists" {
			add(pointer+"/operator", "InvalidValue", "operator must be Equal or Exists")
		}
		if toleration.Operator == "Exists" && toleration.Value != "" {
			add(pointer+"/value", "Forbidden", "Exists tolerations must not set a value")
		}
		if len(toleration.Value) > 63 || (toleration.Value != "" && !labelNamePattern.MatchString(toleration.Value)) {
			add(pointer+"/value", "InvalidLabelValue", "toleration value must be a Kubernetes label value")
		}
		if toleration.Effect != "NoSchedule" && toleration.Effect != "PreferNoSchedule" && toleration.Effect != "NoExecute" {
			add(pointer+"/effect", "InvalidValue", "a specific NoSchedule, PreferNoSchedule, or NoExecute effect is required")
		}
		if toleration.TolerationSeconds != nil && (toleration.Effect != "NoExecute" || *toleration.TolerationSeconds < 0 || *toleration.TolerationSeconds > 604800) {
			add(pointer+"/tolerationSeconds", "InvalidValue", "tolerationSeconds is allowed only for NoExecute and must be between 0 and 604800")
		}
	}
	validateProbes(runtime, add)
	if runtime.SchedulingProfile != nil {
		filtered := problems[:0]
		for _, problem := range problems {
			if problem.Code != "ReservedSchedulingKey" {
				filtered = append(filtered, problem)
			}
		}
		problems = filtered
	}
	return problems
}

func validateRuntimeCommandVector(values []string, maximum int, pointer string, add func(string, string, string)) int {
	if values != nil && (len(values) < 1 || len(values) > maximum) {
		add(pointer, "OutOfRange", fmt.Sprintf("configure between 1 and %d entries or omit the field", maximum))
	}
	total := 0
	for index, value := range values {
		total += len(value)
		if value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			add(fmt.Sprintf("%s/%d", pointer, index), "InvalidCommandArgument", "command entries must be valid UTF-8, non-empty, NUL-free, and at most 4096 bytes")
		}
	}
	return total
}

func validateProbes(runtime WorkloadRuntime, add func(string, string, string)) {
	if runtime.Probes == nil {
		return
	}
	configured := 0
	for _, candidate := range []struct {
		name  string
		probe *WorkloadProbe
	}{
		{"startup", runtime.Probes.Startup},
		{"readiness", runtime.Probes.Readiness},
		{"liveness", runtime.Probes.Liveness},
	} {
		if candidate.probe == nil {
			continue
		}
		configured++
		validateProbe(runtime, candidate.name, *candidate.probe, add)
	}
	if configured == 0 {
		add("/runtime/probes", "Required", "configure at least one startup, readiness, or liveness probe")
	}
}

func validateProbe(runtime WorkloadRuntime, phase string, probe WorkloadProbe, add func(string, string, string)) {
	pointer := "/runtime/probes/" + phase
	actions := 0
	if probe.HTTPGet != nil {
		actions++
		action := probe.HTTPGet
		if len(action.Path) == 0 || len(action.Path) > 2048 || !strings.HasPrefix(action.Path, "/") || strings.IndexFunc(action.Path, func(r rune) bool { return r == 0 || r == '\r' || r == '\n' || r < 0x20 }) >= 0 {
			add(pointer+"/httpGet/path", "InvalidProbePath", "HTTP probe path must begin with /, contain no control characters, and be at most 2048 bytes")
		}
		if action.Scheme != "" && action.Scheme != "HTTP" && action.Scheme != "HTTPS" {
			add(pointer+"/httpGet/scheme", "InvalidValue", "HTTP probe scheme must be HTTP or HTTPS")
		}
		validateProbePort(runtime, action.Port, pointer+"/httpGet/port", add)
	}
	if probe.TCPSocket != nil {
		actions++
		validateProbePort(runtime, probe.TCPSocket.Port, pointer+"/tcpSocket/port", add)
	}
	if probe.Exec != nil {
		actions++
		command := probe.Exec.Command
		if len(command) < 1 || len(command) > 32 {
			add(pointer+"/exec/command", "OutOfRange", "exec probe command must contain between 1 and 32 arguments")
		}
		total := 0
		for index, argument := range command {
			total += len(argument)
			if argument == "" || len(argument) > 4096 || !utf8.ValidString(argument) || strings.IndexByte(argument, 0) >= 0 {
				add(fmt.Sprintf("%s/exec/command/%d", pointer, index), "InvalidCommandArgument", "exec probe arguments must be valid UTF-8, non-empty, NUL-free, and at most 4096 bytes")
			}
		}
		if total > 16<<10 {
			add(pointer+"/exec/command", "TooLong", "exec probe command is limited to 16384 bytes")
		}
	}
	if actions != 1 {
		add(pointer, "ExactlyOneRequired", "set exactly one of httpGet, tcpSocket, or exec")
	}
	validateProbeSeconds(probe.InitialDelaySeconds, 0, 3600, pointer+"/initialDelaySeconds", add)
	validateProbeSeconds(probe.PeriodSeconds, 1, 300, pointer+"/periodSeconds", add)
	validateProbeSeconds(probe.TimeoutSeconds, 1, 300, pointer+"/timeoutSeconds", add)
	validateProbeSeconds(probe.SuccessThreshold, 1, 100, pointer+"/successThreshold", add)
	validateProbeSeconds(probe.FailureThreshold, 1, 100, pointer+"/failureThreshold", add)
	if phase != "readiness" && probe.SuccessThreshold != nil && *probe.SuccessThreshold != 1 {
		add(pointer+"/successThreshold", "InvalidValue", "startup and liveness probe successThreshold must be 1")
	}
}

func validateProbeSeconds(value *int, minimum, maximum int, pointer string, add func(string, string, string)) {
	if value != nil && (*value < minimum || *value > maximum) {
		add(pointer, "OutOfRange", fmt.Sprintf("value must be between %d and %d", minimum, maximum))
	}
}

func validateProbePort(runtime WorkloadRuntime, port WorkloadProbePort, pointer string, add func(string, string, string)) {
	matched := false
	for _, candidate := range runtime.Ports {
		if candidate.Protocol != "TCP" {
			continue
		}
		if port.Name != "" && port.Number == 0 && candidate.Name == port.Name || port.Name == "" && port.Number >= 1 && port.Number <= 65535 && candidate.ContainerPort == port.Number {
			matched = true
			break
		}
	}
	if !matched {
		add(pointer, "InvalidProbePort", "probe port must reference a configured TCP container port by name or number")
	}
}

func validateAffinity(affinity WorkloadAffinity, add func(string, string, string)) {
	if affinity.NodeAffinity == nil && affinity.PodAffinity == nil && affinity.PodAntiAffinity == nil {
		add("/runtime/affinity", "Required", "affinity must configure node, pod, or pod anti-affinity")
	}
	if affinity.NodeAffinity != nil {
		node := affinity.NodeAffinity
		if node.RequiredDuringSchedulingIgnoredDuringExecution == nil && len(node.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
			add("/runtime/affinity/nodeAffinity", "Required", "nodeAffinity must configure a required or preferred term")
		}
		if node.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			terms := node.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
			if len(terms) < 1 || len(terms) > 16 {
				add("/runtime/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution/nodeSelectorTerms", "OutOfRange", "configure between 1 and 16 required node selector terms")
			}
			for index, term := range terms {
				validateNodeTerm(term, fmt.Sprintf("/runtime/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution/nodeSelectorTerms/%d", index), add)
			}
		}
		if len(node.PreferredDuringSchedulingIgnoredDuringExecution) > 16 {
			add("/runtime/affinity/nodeAffinity/preferredDuringSchedulingIgnoredDuringExecution", "OutOfRange", "at most 16 preferred node selector terms are allowed")
		}
		for index, term := range node.PreferredDuringSchedulingIgnoredDuringExecution {
			pointer := fmt.Sprintf("/runtime/affinity/nodeAffinity/preferredDuringSchedulingIgnoredDuringExecution/%d", index)
			if term.Weight < 1 || term.Weight > 100 {
				add(pointer+"/weight", "OutOfRange", "weight must be between 1 and 100")
			}
			validateNodeTerm(term.Preference, pointer+"/preference", add)
		}
	}
	validatePodAffinity(affinity.PodAffinity, "/runtime/affinity/podAffinity", add)
	validatePodAffinity(affinity.PodAntiAffinity, "/runtime/affinity/podAntiAffinity", add)
}

func validateNodeTerm(term NodeSelectorTerm, pointer string, add func(string, string, string)) {
	if len(term.MatchExpressions) < 1 || len(term.MatchExpressions) > 32 {
		add(pointer+"/matchExpressions", "OutOfRange", "configure between 1 and 32 match expressions")
	}
	for index, expression := range term.MatchExpressions {
		path := fmt.Sprintf("%s/matchExpressions/%d", pointer, index)
		if !validLabelKey(expression.Key) {
			add(path+"/key", "InvalidLabelKey", "key must be a Kubernetes label key")
		}
		if reservedSchedulingKey(expression.Key) {
			add(path+"/key", "ReservedSchedulingKey", "system, control-plane, and builder node labels require an administrator-managed scheduling profile")
		}
		validOperator := expression.Operator == "In" || expression.Operator == "NotIn" || expression.Operator == "Exists" || expression.Operator == "DoesNotExist" || expression.Operator == "Gt" || expression.Operator == "Lt"
		if !validOperator {
			add(path+"/operator", "InvalidValue", "unsupported node selector operator")
		}
		needsValues := expression.Operator == "In" || expression.Operator == "NotIn" || expression.Operator == "Gt" || expression.Operator == "Lt"
		if needsValues && (len(expression.Values) < 1 || len(expression.Values) > 32) || !needsValues && len(expression.Values) > 0 {
			add(path+"/values", "InvalidValue", "values must match the selected operator")
		}
		if (expression.Operator == "Gt" || expression.Operator == "Lt") && len(expression.Values) != 1 {
			add(path+"/values", "InvalidValue", "Gt and Lt require exactly one integer value")
		}
		for valueIndex, value := range expression.Values {
			if len(value) > 63 || !labelNamePattern.MatchString(value) {
				add(fmt.Sprintf("%s/values/%d", path, valueIndex), "InvalidLabelValue", "selector values must be Kubernetes label values")
			}
			if expression.Operator == "Gt" || expression.Operator == "Lt" {
				if _, err := strconv.Atoi(value); err != nil {
					add(fmt.Sprintf("%s/values/%d", path, valueIndex), "InvalidInteger", "Gt and Lt values must be integers")
				}
			}
		}
	}
}

func validatePodAffinity(affinity *PodAffinity, pointer string, add func(string, string, string)) {
	if affinity == nil {
		return
	}
	if len(affinity.RequiredDuringSchedulingIgnoredDuringExecution) == 0 && len(affinity.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
		add(pointer, "Required", "pod affinity must configure a required or preferred term")
	}
	if len(affinity.RequiredDuringSchedulingIgnoredDuringExecution) > 16 || len(affinity.PreferredDuringSchedulingIgnoredDuringExecution) > 16 {
		add(pointer, "OutOfRange", "at most 16 required and 16 preferred pod affinity terms are allowed")
	}
	for index, term := range affinity.RequiredDuringSchedulingIgnoredDuringExecution {
		validatePodAffinityTerm(term, fmt.Sprintf("%s/requiredDuringSchedulingIgnoredDuringExecution/%d", pointer, index), add)
	}
	for index, term := range affinity.PreferredDuringSchedulingIgnoredDuringExecution {
		path := fmt.Sprintf("%s/preferredDuringSchedulingIgnoredDuringExecution/%d", pointer, index)
		if term.Weight < 1 || term.Weight > 100 {
			add(path+"/weight", "OutOfRange", "weight must be between 1 and 100")
		}
		validatePodAffinityTerm(term.PodAffinityTerm, path+"/podAffinityTerm", add)
	}
}

func validatePodAffinityTerm(term PodAffinityTerm, pointer string, add func(string, string, string)) {
	if !validLabelKey(term.TopologyKey) {
		add(pointer+"/topologyKey", "InvalidLabelKey", "topologyKey must be a Kubernetes label key")
	}
	if reservedSchedulingKey(term.TopologyKey) {
		add(pointer+"/topologyKey", "ReservedSchedulingKey", "system and builder placement labels require an administrator-managed scheduling profile")
	}
	validateLabelSelector(term.LabelSelector, pointer+"/labelSelector", add)
}

func validateLabelSelector(selector LabelSelector, pointer string, add func(string, string, string)) {
	if len(selector.MatchLabels) == 0 && len(selector.MatchExpressions) == 0 {
		add(pointer, "Required", "an explicit label selector is required")
	}
	if len(selector.MatchLabels) > 32 || len(selector.MatchExpressions) > 32 {
		add(pointer, "OutOfRange", "a label selector supports at most 32 labels and 32 expressions")
	}
	validateNodeSelector(selector.MatchLabels, pointer+"/matchLabels", add)
	for index, expression := range selector.MatchExpressions {
		path := fmt.Sprintf("%s/matchExpressions/%d", pointer, index)
		if !validLabelKey(expression.Key) {
			add(path+"/key", "InvalidLabelKey", "key must be a Kubernetes label key")
		}
		needsValues := expression.Operator == "In" || expression.Operator == "NotIn"
		if !needsValues && expression.Operator != "Exists" && expression.Operator != "DoesNotExist" {
			add(path+"/operator", "InvalidValue", "use In, NotIn, Exists, or DoesNotExist")
		}
		if needsValues && (len(expression.Values) < 1 || len(expression.Values) > 32) || !needsValues && len(expression.Values) > 0 {
			add(path+"/values", "InvalidValue", "values must match the selected operator")
		}
		for valueIndex, value := range expression.Values {
			if len(value) > 63 || (value != "" && !labelNamePattern.MatchString(value)) {
				add(fmt.Sprintf("%s/values/%d", path, valueIndex), "InvalidLabelValue", "selector values must be Kubernetes label values")
			}
		}
	}
}

func validateNodeSelector(selector map[string]string, pointer string, add func(string, string, string)) {
	if len(selector) > 32 {
		add(pointer, "OutOfRange", "at most 32 selector entries are allowed")
	}
	for key, value := range selector {
		if !validLabelKey(key) {
			add(pointer+"/"+key, "InvalidLabelKey", "selector key must be a Kubernetes label key")
		}
		if strings.HasPrefix(pointer, "/runtime/nodeSelector") && reservedSchedulingKey(key) {
			add(pointer+"/"+key, "ReservedSchedulingKey", "system, control-plane, and builder node labels require an administrator-managed scheduling profile")
		}
		if len(value) > 63 || (value != "" && !labelNamePattern.MatchString(value)) {
			add(pointer+"/"+key, "InvalidLabelValue", "selector value must be a Kubernetes label value")
		}
	}
}

func reservedSchedulingKey(key string) bool {
	return key == "node-role.kubernetes.io/control-plane" || key == "node-role.kubernetes.io/master" || strings.HasPrefix(key, "kuberploy.io/")
}

func validDNSLabel(value string) bool {
	return len(value) > 0 && len(value) <= 63 && dnsLabelPattern.MatchString(value)
}

func validLabelKey(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	parts := strings.Split(value, "/")
	if len(parts) > 2 || len(parts[len(parts)-1]) > 63 || !labelNamePattern.MatchString(parts[len(parts)-1]) {
		return false
	}
	if len(parts) == 1 {
		return true
	}
	prefix := parts[0]
	if len(prefix) > 253 {
		return false
	}
	for _, label := range strings.Split(prefix, ".") {
		if len(label) == 0 || len(label) > 63 || !dnsLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func cpuMilli(value string) (int64, bool) {
	if strings.HasSuffix(value, "m") {
		milli, err := strconv.ParseInt(strings.TrimSuffix(value, "m"), 10, 64)
		return milli, err == nil && milli > 0 && milli <= 1_000_000
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" || len(parts[0]) > 4 {
		return 0, false
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole < 0 {
		return 0, false
	}
	fraction := int64(0)
	if len(parts) == 2 {
		if len(parts[1]) < 1 || len(parts[1]) > 3 {
			return 0, false
		}
		padded := parts[1] + strings.Repeat("0", 3-len(parts[1]))
		fraction, err = strconv.ParseInt(padded, 10, 64)
		if err != nil {
			return 0, false
		}
	}
	milli := whole*1000 + fraction
	return milli, milli > 0 && milli <= 1_000_000
}

func memoryBytes(value string) (int64, bool) {
	units := map[string]int64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40}
	for suffix, multiplier := range units {
		if !strings.HasSuffix(value, suffix) {
			continue
		}
		number := strings.TrimSuffix(value, suffix)
		if number == "" || len(number) > 12 || strings.HasPrefix(number, "0") {
			return 0, false
		}
		amount, err := strconv.ParseInt(number, 10, 64)
		if err != nil || amount <= 0 || amount > (64<<40)/multiplier {
			return 0, false
		}
		return amount * multiplier, true
	}
	return 0, false
}
