package domain

import (
	"encoding/json"
	"testing"
)

func TestWorkloadDefaultsAreExplicitAndValid(t *testing.T) {
	runtime := NormalizeWorkloadRuntime(WorkloadRuntime{
		Ports: []WorkloadPort{{Name: "http", ContainerPort: 8080}},
	})
	if runtime.Replicas != 1 || runtime.Strategy.Type != "RollingUpdate" || runtime.Resources.Requests.CPU != "50m" || runtime.Resources.Requests.Memory != "100Mi" || runtime.Ports[0].Protocol != "TCP" {
		t.Fatalf("unexpected defaults: %#v", runtime)
	}
	if problems := ValidateWorkloadRuntime(runtime); len(problems) != 0 {
		t.Fatalf("default runtime is invalid: %#v", problems)
	}
}

func TestWorkloadDeploymentStrategyIsClosed(t *testing.T) {
	legacy := WorkloadRuntime{Replicas: 1, Ports: []WorkloadPort{{Name: "http", ContainerPort: 8080, Protocol: "TCP"}}, Resources: WorkloadResources{Requests: ResourceList{CPU: DefaultCPURequest, Memory: DefaultMemoryRequest}}}
	if problems := ValidateWorkloadRuntime(legacy); len(problems) != 0 {
		t.Fatalf("legacy omitted strategy must remain RollingUpdate-compatible: %#v", problems)
	}
	for _, strategy := range []string{"RollingUpdate", "Recreate"} {
		runtime := NormalizeWorkloadRuntime(WorkloadRuntime{Strategy: WorkloadDeploymentStrategy{Type: strategy}, Ports: []WorkloadPort{{Name: "http", ContainerPort: 8080}}})
		if problems := ValidateWorkloadRuntime(runtime); len(problems) != 0 {
			t.Fatalf("%s: %#v", strategy, problems)
		}
	}
	runtime := NormalizeWorkloadRuntime(WorkloadRuntime{Strategy: WorkloadDeploymentStrategy{Type: "OnDelete"}, Ports: []WorkloadPort{{Name: "http", ContainerPort: 8080}}})
	problems := ValidateWorkloadRuntime(runtime)
	if len(problems) == 0 || problems[0].Pointer != "/runtime/strategy/type" {
		t.Fatalf("invalid strategy problems=%#v", problems)
	}
}

func TestWorkloadRejectsKubernetesSystemPriorityClasses(t *testing.T) {
	for _, name := range []string{"system-node-critical", "system-cluster-critical", "system-future-critical"} {
		runtime := NormalizeWorkloadRuntime(WorkloadRuntime{
			PriorityClassName: name,
			Ports:             []WorkloadPort{{Name: "http", ContainerPort: 8080}},
		})
		problems := ValidateWorkloadRuntime(runtime)
		if len(problems) != 1 || problems[0].Pointer != "/runtime/priorityClassName" || problems[0].Code != "ReservedPriorityClass" {
			t.Fatalf("%s problems=%#v", name, problems)
		}
	}
	ordinary := NormalizeWorkloadRuntime(WorkloadRuntime{
		PriorityClassName: "workload-high",
		Ports:             []WorkloadPort{{Name: "http", ContainerPort: 8080}},
	})
	if problems := ValidateWorkloadRuntime(ordinary); len(problems) != 0 {
		t.Fatalf("ordinary priority class rejected: %#v", problems)
	}
}

func TestCompleteSchedulingContractIsValid(t *testing.T) {
	plain := "info"
	minDomains := 2
	seconds := int64(60)
	terminationGrace := 45
	runtime := NormalizeWorkloadRuntime(WorkloadRuntime{
		Replicas:                      2,
		Command:                       []string{"/app/server"},
		Args:                          []string{"--listen=:8080", "--mode=production"},
		TerminationGracePeriodSeconds: &terminationGrace,
		Ports:                         []WorkloadPort{{Name: "http", ContainerPort: 8080, ServicePort: 80}},
		Env: []WorkloadEnv{
			{Name: "LOG_LEVEL", Value: &plain},
			{Name: "DATABASE_PASSWORD", ValueFrom: &WorkloadEnvValueFrom{SecretBindingRef: SecretBindingRef{BindingID: "44444444-4444-4444-8444-444444444444", Name: "database", Key: "password", Version: 3}}},
		},
		Resources:    WorkloadResources{Requests: ResourceList{CPU: "50m", Memory: "100Mi"}, Limits: &ResourceList{CPU: "500m", Memory: "512Mi"}},
		NodeSelector: map[string]string{"kubernetes.io/arch": "amd64"},
		Affinity: &WorkloadAffinity{
			NodeAffinity: &NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution:  &NodeSelector{NodeSelectorTerms: []NodeSelectorTerm{{MatchExpressions: []NodeSelectorRequirement{{Key: "karpenter.sh/capacity-type", Operator: "In", Values: []string{"on-demand"}}}}}},
				PreferredDuringSchedulingIgnoredDuringExecution: []PreferredSchedulingTerm{{Weight: 50, Preference: NodeSelectorTerm{MatchExpressions: []NodeSelectorRequirement{{Key: "topology.kubernetes.io/zone", Operator: "Exists"}}}}},
			},
			PodAffinity:     &PodAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []WeightedPodAffinityTerm{{Weight: 25, PodAffinityTerm: PodAffinityTerm{TopologyKey: "kubernetes.io/hostname", LabelSelector: LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "api"}}}}}},
			PodAntiAffinity: &PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []PodAffinityTerm{{TopologyKey: "kubernetes.io/hostname", LabelSelector: LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "api"}}}}},
		},
		TopologySpreadConstraints: []TopologySpreadConstraint{{MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: "DoNotSchedule", LabelSelector: LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "api"}}, MinDomains: &minDomains, NodeAffinityPolicy: "Honor", NodeTaintsPolicy: "Honor"}},
		Tolerations:               []WorkloadToleration{{Key: "workload.kuberploy.io/class", Operator: "Equal", Value: "application", Effect: "NoSchedule"}, {Key: "node.kubernetes.io/not-ready", Operator: "Exists", Effect: "NoExecute", TolerationSeconds: &seconds}},
	})
	if problems := ValidateWorkloadRuntime(runtime); len(problems) != 0 {
		t.Fatalf("complete runtime is invalid: %#v", problems)
	}
}

func TestTopologySpreadMinDomainsRequiresDoNotSchedule(t *testing.T) {
	minDomains := 1
	runtime := NormalizeWorkloadRuntime(WorkloadRuntime{
		Ports: []WorkloadPort{{Name: "http", ContainerPort: 8080}},
		TopologySpreadConstraints: []TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: "ScheduleAnyway",
			LabelSelector:     LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "api"}},
			MinDomains:        &minDomains,
		}},
	})
	problems := ValidateWorkloadRuntime(runtime)
	if len(problems) != 1 || problems[0].Pointer != "/runtime/topologySpreadConstraints/0/minDomains" || problems[0].Code != "InvalidValue" {
		t.Fatalf("invalid minDomains policy was accepted: %#v", problems)
	}
}

func TestWorkloadCommandContractIsBoundedAndRoundTrips(t *testing.T) {
	grace := 30
	runtime := NormalizeWorkloadRuntime(WorkloadRuntime{
		Command:                       []string{"/app/server"},
		Args:                          []string{"--listen=:8080"},
		TerminationGracePeriodSeconds: &grace,
		Ports:                         []WorkloadPort{{Name: "http", ContainerPort: 8080}},
	})
	encoded, err := json.Marshal(runtime)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip WorkloadRuntime
	if err = json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if len(roundTrip.Command) != 1 || roundTrip.Command[0] != "/app/server" || len(roundTrip.Args) != 1 || roundTrip.Args[0] != "--listen=:8080" || roundTrip.TerminationGracePeriodSeconds == nil || *roundTrip.TerminationGracePeriodSeconds != grace {
		t.Fatalf("command contract was not preserved: %#v", roundTrip)
	}

	zero := 0
	invalid := runtime
	invalid.Command = []string{}
	invalid.Args = []string{"bad\x00argument"}
	invalid.TerminationGracePeriodSeconds = &zero
	wanted := map[string]bool{"/runtime/command": false, "/runtime/args/0": false, "/runtime/terminationGracePeriodSeconds": false}
	for _, problem := range ValidateWorkloadRuntime(invalid) {
		if _, ok := wanted[problem.Pointer]; ok {
			wanted[problem.Pointer] = true
		}
	}
	for pointer, found := range wanted {
		if !found {
			t.Errorf("missing command validation at %s", pointer)
		}
	}
}

func TestWorkloadRejectsUnsafeOrAmbiguousInputs(t *testing.T) {
	secret := "must-not-be-treated-as-a-reference"
	runtime := NormalizeWorkloadRuntime(WorkloadRuntime{
		Ports:       []WorkloadPort{{Name: "http", ContainerPort: 8080}},
		Env:         []WorkloadEnv{{Name: "SECRET", Value: &secret, ValueFrom: &WorkloadEnvValueFrom{SecretBindingRef: SecretBindingRef{BindingID: "44444444-4444-4444-8444-444444444444", Name: "database", Key: "password", Version: 1}}}},
		Resources:   WorkloadResources{Requests: ResourceList{CPU: "500m", Memory: "512Mi"}, Limits: &ResourceList{CPU: "100m", Memory: "100Mi"}},
		Tolerations: []WorkloadToleration{{Operator: "Exists", Effect: "NoSchedule"}},
	})
	problems := ValidateWorkloadRuntime(runtime)
	wanted := map[string]bool{
		"/runtime/env/0":                   false,
		"/runtime/resources/limits/cpu":    false,
		"/runtime/resources/limits/memory": false,
		"/runtime/tolerations/0/key":       false,
	}
	for _, problem := range problems {
		if _, exists := wanted[problem.Pointer]; exists {
			wanted[problem.Pointer] = true
		}
	}
	for pointer, found := range wanted {
		if !found {
			t.Errorf("missing validation problem for %s: %#v", pointer, problems)
		}
	}
}

func TestCPUAndMemoryLimitsAreIndependent(t *testing.T) {
	for name, limits := range map[string]ResourceList{
		"cpu-only":    {CPU: "500m"},
		"memory-only": {Memory: "512Mi"},
	} {
		t.Run(name, func(t *testing.T) {
			runtime := NormalizeWorkloadRuntime(WorkloadRuntime{
				Ports:     []WorkloadPort{{Name: "http", ContainerPort: 8080}},
				Resources: WorkloadResources{Limits: &limits},
			})
			if problems := ValidateWorkloadRuntime(runtime); len(problems) != 0 {
				t.Fatalf("independent limit is invalid: %#v", problems)
			}
		})
	}
}

func TestReservedSystemAndBuilderPlacementIsRejected(t *testing.T) {
	runtime := NormalizeWorkloadRuntime(WorkloadRuntime{
		Ports:        []WorkloadPort{{Name: "http", ContainerPort: 8080}},
		NodeSelector: map[string]string{"kuberploy.io/node-class": "builder"},
		Affinity: &WorkloadAffinity{NodeAffinity: &NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &NodeSelector{NodeSelectorTerms: []NodeSelectorTerm{{MatchExpressions: []NodeSelectorRequirement{{Key: "node-role.kubernetes.io/control-plane", Operator: "Exists"}}}}},
		}},
		TopologySpreadConstraints: []TopologySpreadConstraint{{MaxSkew: 1, TopologyKey: "kuberploy.io/system-zone", WhenUnsatisfiable: "DoNotSchedule", LabelSelector: LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "api"}}}},
		Tolerations:               []WorkloadToleration{{Key: "kuberploy.io/builder", Operator: "Exists", Effect: "NoSchedule"}},
	})
	wanted := map[string]bool{
		"/runtime/nodeSelector/kuberploy.io/node-class": false,
		"/runtime/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution/nodeSelectorTerms/0/matchExpressions/0/key": false,
		"/runtime/topologySpreadConstraints/0/topologyKey":                                                                         false,
		"/runtime/tolerations/0/key": false,
	}
	for _, problem := range ValidateWorkloadRuntime(runtime) {
		if problem.Code == "ReservedSchedulingKey" {
			if _, exists := wanted[problem.Pointer]; exists {
				wanted[problem.Pointer] = true
			}
		}
	}
	for pointer, found := range wanted {
		if !found {
			t.Errorf("missing reserved-key error for %s", pointer)
		}
	}
}

func TestHealthProbesRoundTripAndReferenceConfiguredTCPPorts(t *testing.T) {
	period, timeout, success, failure := 10, 2, 1, 3
	runtime := NormalizeWorkloadRuntime(WorkloadRuntime{
		Ports: []WorkloadPort{{Name: "http", ContainerPort: 8080, Protocol: "TCP"}, {Name: "dns", ContainerPort: 5353, Protocol: "UDP"}},
		Probes: &WorkloadProbes{
			Startup:   &WorkloadProbe{TCPSocket: &WorkloadTCPSocketAction{Port: WorkloadProbePort{Number: 8080}}, PeriodSeconds: &period, TimeoutSeconds: &timeout, SuccessThreshold: &success, FailureThreshold: &failure},
			Readiness: &WorkloadProbe{HTTPGet: &WorkloadHTTPGetAction{Path: "/ready", Port: WorkloadProbePort{Name: "http"}, Scheme: "HTTP"}, PeriodSeconds: &period, TimeoutSeconds: &timeout, SuccessThreshold: &success, FailureThreshold: &failure},
			Liveness:  &WorkloadProbe{Exec: &WorkloadExecAction{Command: []string{"/bin/check", "--live"}}, PeriodSeconds: &period, TimeoutSeconds: &timeout, SuccessThreshold: &success, FailureThreshold: &failure},
		},
	})
	if problems := ValidateWorkloadRuntime(runtime); len(problems) != 0 {
		t.Fatalf("valid probes rejected: %#v", problems)
	}
	encoded, err := json.Marshal(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || !json.Valid(encoded) {
		t.Fatalf("invalid probe JSON: %s", encoded)
	}
	var roundTrip WorkloadRuntime
	if err = json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.Probes == nil || roundTrip.Probes.Readiness == nil || roundTrip.Probes.Readiness.HTTPGet == nil || roundTrip.Probes.Readiness.HTTPGet.Port.Name != "http" || roundTrip.Probes.Startup.TCPSocket.Port.Number != 8080 {
		t.Fatalf("probe action/port was not preserved: %#v", roundTrip.Probes)
	}
}

func TestHealthProbesRejectAmbiguousActionsUDPPortsAndUnsafeCommands(t *testing.T) {
	two := 2
	runtime := NormalizeWorkloadRuntime(WorkloadRuntime{
		Ports: []WorkloadPort{{Name: "http", ContainerPort: 8080, Protocol: "TCP"}, {Name: "dns", ContainerPort: 5353, Protocol: "UDP"}},
		Probes: &WorkloadProbes{
			Startup:  &WorkloadProbe{HTTPGet: &WorkloadHTTPGetAction{Path: "ready\nInjected", Port: WorkloadProbePort{Name: "dns"}}, TCPSocket: &WorkloadTCPSocketAction{Port: WorkloadProbePort{Number: 9000}}, SuccessThreshold: &two},
			Liveness: &WorkloadProbe{Exec: &WorkloadExecAction{Command: []string{"ok", "bad\x00argument"}}},
		},
	})
	wanted := map[string]bool{
		"/runtime/probes/startup":                  false,
		"/runtime/probes/startup/httpGet/path":     false,
		"/runtime/probes/startup/httpGet/port":     false,
		"/runtime/probes/startup/tcpSocket/port":   false,
		"/runtime/probes/startup/successThreshold": false,
		"/runtime/probes/liveness/exec/command/1":  false,
	}
	for _, problem := range ValidateWorkloadRuntime(runtime) {
		if _, exists := wanted[problem.Pointer]; exists {
			wanted[problem.Pointer] = true
		}
	}
	for pointer, found := range wanted {
		if !found {
			t.Errorf("missing probe validation at %s", pointer)
		}
	}
}
