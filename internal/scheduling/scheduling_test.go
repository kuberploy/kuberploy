package scheduling

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

const (
	aid = "11111111-1111-4111-8111-111111111111"
	tid = "22222222-2222-4222-8222-222222222222"
	pid = "33333333-3333-4333-8333-333333333333"
	eid = "44444444-4444-4444-8444-444444444444"
)

func cmd(k string) Command { return Command{aid, k, "req", time.Unix(100, 0)} }
func spec(v string) Spec {
	return Spec{Pod: PodScheduling{NodeSelector: map[string]string{"platform.example/class": v}, Tolerations: []Toleration{{Key: "platform.example/dedicated", Operator: "Equal", Value: v, Effect: "NoSchedule"}}}}
}
func TestImmutableRevisionReplayAndClosedResolver(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	x, e := s.Create(ctx, cmd("create"), "general", spec("general"), []Assignment{{TeamScope, tid}})
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.Create(ctx, cmd("create"), "general", spec("general"), []Assignment{{TeamScope, tid}})
	if e != nil || !again.Replay || again.Profile.ID != x.Profile.ID {
		t.Fatalf("replay %#v %v", again, e)
	}
	if _, e = s.Create(ctx, cmd("create"), "general", spec("other"), []Assignment{{TeamScope, tid}}); !errors.Is(e, ErrConflict) {
		t.Fatalf("idempotency substitution %v", e)
	}
	old := x.Revision.Spec.Pod.NodeSelector["platform.example/class"]
	y, e := s.Revise(ctx, cmd("revise"), Ref{x.Profile.ID, 1}, spec("batch"), []Assignment{{EnvironmentScope, eid}})
	if e != nil || y.Revision.Revision != 2 || x.Revision.Spec.Pod.NodeSelector["platform.example/class"] != old {
		t.Fatal("revision mutation")
	}
	r, _ := NewResolver(s)
	if _, e = r.Resolve(ctx, Ref{x.Profile.ID, 1}, Target{tid, pid, eid}); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale revision %v", e)
	}
	pod, e := r.Resolve(ctx, Ref{x.Profile.ID, 2}, Target{tid, pid, eid})
	if e != nil || pod.NodeSelector["platform.example/class"] != "batch" {
		t.Fatalf("resolve %#v %v", pod, e)
	}
	if _, e = r.Resolve(ctx, Ref{x.Profile.ID, 2}, Target{tid, pid, "55555555-5555-4555-8555-555555555555"}); !errors.Is(e, ErrUnassigned) {
		t.Fatalf("scope bypass %v", e)
	}
	if _, e = s.Deactivate(ctx, cmd("off"), Ref{x.Profile.ID, 2}); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Resolve(ctx, Ref{x.Profile.ID, 2}, Target{tid, pid, eid}); !errors.Is(e, ErrInactive) {
		t.Fatalf("inactive %v", e)
	}
	if len(s.AuditEvents()) != 3 {
		t.Fatalf("audit %d", len(s.AuditEvents()))
	}
}
func TestRejectsUnboundedOrInvalidPodMaterial(t *testing.T) {
	if KarpenterDiagnosticsEnabledByDefault {
		t.Fatal("optional Karpenter diagnostics must remain default-off")
	}
	s := NewMemoryStore()
	bad := spec("general")
	bad.Pod.NodeSelector["bad key"] = "x"
	if _, e := s.Create(context.Background(), cmd("bad"), "general", bad, []Assignment{{TeamScope, tid}}); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	if _, e := s.Create(context.Background(), cmd("empty"), "general", spec("general"), nil); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
}

func TestMaterializeLocksExactProfileIdentityAndPodFields(t *testing.T) {
	s := NewMemoryStore()
	created, err := s.Create(context.Background(), cmd("materialize"), "general", Spec{Pod: PodScheduling{
		NodeSelector:         map[string]string{"karpenter.sh/capacity-type": "on-demand"},
		RequiredNodeAffinity: []Requirement{{Key: "kubernetes.io/arch", Operator: "In", Values: []string{"amd64"}}},
		Tolerations:          []Toleration{{Key: "workload.example/dedicated", Operator: "Equal", Value: "general", Effect: "NoSchedule"}},
		TopologySpread:       []TopologySpread{{MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: "DoNotSchedule"}},
		PriorityClassName:    "normal",
	}}, []Assignment{{EnvironmentScope, eid}})
	if err != nil {
		t.Fatal(err)
	}
	resolver, _ := NewResolver(s)
	resolved, err := resolver.ResolveExact(context.Background(), Ref{created.Profile.ID, 1}, Target{tid, pid, eid})
	if err != nil {
		t.Fatal(err)
	}
	runtime := domain.DefaultWorkloadRuntime(8080, nil)
	materialized, err := Materialize(runtime, resolved, "55555555-5555-4555-8555-555555555555")
	if err != nil || !Matches(materialized, resolved, "55555555-5555-4555-8555-555555555555") {
		t.Fatalf("materialization %#v %v", materialized, err)
	}
	materialized.NodeSelector["karpenter.sh/capacity-type"] = "spot"
	if Matches(materialized, resolved, "55555555-5555-4555-8555-555555555555") {
		t.Fatal("substituted scheduling material matched")
	}
	materialized, _ = Materialize(runtime, resolved, "55555555-5555-4555-8555-555555555555")
	materialized.SchedulingProfile.SpecDigest = "sha256:" + strings.Repeat("0", 64)
	if Matches(materialized, resolved, "55555555-5555-4555-8555-555555555555") {
		t.Fatal("substituted scheduling identity matched")
	}
}

func TestMaterializePreferredNodeAndClosedSameApplicationAntiAffinity(t *testing.T) {
	preferredWeight := 40
	pod := PodScheduling{
		RequiredNodeAffinity: []Requirement{{Key: "kubernetes.io/arch", Operator: "In", Values: []string{"amd64"}}},
		PreferredNodeAffinity: []PreferredNodeAffinity{
			{Weight: 70, Requirements: []Requirement{{Key: "topology.kubernetes.io/zone", Operator: "In", Values: []string{"zone-b", "zone-a"}}}},
		},
		SameApplicationPodAntiAffinity: []SameApplicationPodAntiAffinity{
			{Enforcement: "required", TopologyKey: "kubernetes.io/hostname"},
			{Enforcement: "preferred", TopologyKey: "topology.kubernetes.io/zone", Weight: &preferredWeight},
		},
	}
	created, err := NewMemoryStore().Create(context.Background(), cmd("closed-affinity"), "closed-affinity", Spec{Pod: pod}, []Assignment{{EnvironmentScope, eid}})
	if err != nil {
		t.Fatal(err)
	}
	resolution := Resolution{Ref: Ref{created.Profile.ID, 1}, SpecDigest: created.Revision.SpecDigest, AssignmentsDigest: created.Revision.AssignmentsDigest, Pod: created.Revision.Spec.Pod}
	applicationID := "55555555-5555-4555-8555-555555555555"
	materialized, err := Materialize(domain.DefaultWorkloadRuntime(8080, nil), resolution, applicationID)
	if err != nil {
		t.Fatal(err)
	}
	if materialized.Affinity == nil || materialized.Affinity.NodeAffinity == nil || materialized.Affinity.PodAntiAffinity == nil {
		t.Fatalf("incomplete affinity: %#v", materialized.Affinity)
	}
	node := materialized.Affinity.NodeAffinity
	if node.RequiredDuringSchedulingIgnoredDuringExecution == nil || len(node.PreferredDuringSchedulingIgnoredDuringExecution) != 1 || node.PreferredDuringSchedulingIgnoredDuringExecution[0].Weight != 70 {
		t.Fatalf("required and preferred node affinity were not combined: %#v", node)
	}
	anti := materialized.Affinity.PodAntiAffinity
	if len(anti.RequiredDuringSchedulingIgnoredDuringExecution) != 1 || len(anti.PreferredDuringSchedulingIgnoredDuringExecution) != 1 || anti.PreferredDuringSchedulingIgnoredDuringExecution[0].Weight != preferredWeight {
		t.Fatalf("required and preferred anti-affinity were not combined: %#v", anti)
	}
	for _, term := range []domain.PodAffinityTerm{anti.RequiredDuringSchedulingIgnoredDuringExecution[0], anti.PreferredDuringSchedulingIgnoredDuringExecution[0].PodAffinityTerm} {
		if len(term.LabelSelector.MatchLabels) != 1 || term.LabelSelector.MatchLabels["kuberploy.io/application"] != applicationID || len(term.LabelSelector.MatchExpressions) != 0 {
			t.Fatalf("anti-affinity selector was not derived from the exact application: %#v", term.LabelSelector)
		}
	}
	if !Matches(materialized, resolution, applicationID) {
		t.Fatal("exact closed affinity did not match")
	}
	materialized.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector.MatchLabels["kuberploy.io/application"] = "66666666-6666-4666-8666-666666666666"
	if Matches(materialized, resolution, applicationID) {
		t.Fatal("cross-application anti-affinity selector substitution matched")
	}
}

func TestPreferredAffinityBoundsAndDefaultWeight(t *testing.T) {
	valid := spec("general")
	valid.Pod.PreferredNodeAffinity = []PreferredNodeAffinity{{Weight: 1, Requirements: []Requirement{{Key: "topology.kubernetes.io/zone", Operator: "Exists"}}}}
	valid.Pod.SameApplicationPodAntiAffinity = []SameApplicationPodAntiAffinity{{Enforcement: "preferred", TopologyKey: "kubernetes.io/hostname"}}
	created, err := NewMemoryStore().Create(context.Background(), cmd("default-weight"), "default-weight", valid, []Assignment{{TeamScope, tid}})
	if err != nil {
		t.Fatal(err)
	}
	weight := created.Revision.Spec.Pod.SameApplicationPodAntiAffinity[0].Weight
	if weight == nil || *weight != 100 {
		t.Fatalf("preferred anti-affinity default weight = %v", weight)
	}

	badCases := []PodScheduling{
		{PreferredNodeAffinity: []PreferredNodeAffinity{{Weight: 0, Requirements: []Requirement{{Key: "kubernetes.io/arch", Operator: "Exists"}}}}},
		{PreferredNodeAffinity: []PreferredNodeAffinity{{Weight: 101, Requirements: []Requirement{{Key: "kubernetes.io/arch", Operator: "Exists"}}}}},
		{PreferredNodeAffinity: []PreferredNodeAffinity{{Weight: 50}}},
		{PreferredNodeAffinity: []PreferredNodeAffinity{{Weight: 50, Requirements: []Requirement{{Key: "placement.example/generation", Operator: "Gt", Values: []string{"not-an-integer"}}}}}},
		{SameApplicationPodAntiAffinity: []SameApplicationPodAntiAffinity{{Enforcement: "preferred", TopologyKey: "kubernetes.io/hostname", Weight: intPointer(0)}}},
		{SameApplicationPodAntiAffinity: []SameApplicationPodAntiAffinity{{Enforcement: "required", TopologyKey: "kubernetes.io/hostname", Weight: intPointer(50)}}},
		{SameApplicationPodAntiAffinity: []SameApplicationPodAntiAffinity{{Enforcement: "soft", TopologyKey: "kubernetes.io/hostname"}}},
	}
	tooManyPreferred := PodScheduling{}
	for i := 0; i < 17; i++ {
		tooManyPreferred.PreferredNodeAffinity = append(tooManyPreferred.PreferredNodeAffinity, PreferredNodeAffinity{Weight: 50, Requirements: []Requirement{{Key: fmt.Sprintf("placement.example/zone-%d", i), Operator: "Exists"}}})
	}
	badCases = append(badCases, tooManyPreferred)
	tooManyExpressions := PodScheduling{PreferredNodeAffinity: []PreferredNodeAffinity{{Weight: 50}}}
	for i := 0; i < 33; i++ {
		tooManyExpressions.PreferredNodeAffinity[0].Requirements = append(tooManyExpressions.PreferredNodeAffinity[0].Requirements, Requirement{Key: fmt.Sprintf("placement.example/class-%d", i), Operator: "Exists"})
	}
	badCases = append(badCases, tooManyExpressions)
	for i, pod := range badCases {
		if err := pod.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("case %d accepted: %#v", i, pod)
		}
	}
}

func TestNewAffinityCanonicalizationAndDigestMutation(t *testing.T) {
	weight := 25
	first := spec("general")
	first.Pod.PreferredNodeAffinity = []PreferredNodeAffinity{
		{Weight: 80, Requirements: []Requirement{{Key: "topology.kubernetes.io/zone", Operator: "In", Values: []string{"b", "a"}}}},
		{Weight: 20, Requirements: []Requirement{{Key: "kubernetes.io/arch", Operator: "In", Values: []string{"arm64", "amd64"}}}},
	}
	first.Pod.SameApplicationPodAntiAffinity = []SameApplicationPodAntiAffinity{
		{Enforcement: "preferred", TopologyKey: "topology.kubernetes.io/zone", Weight: &weight},
		{Enforcement: "required", TopologyKey: "kubernetes.io/hostname"},
	}
	second := spec("general")
	second.Pod.PreferredNodeAffinity = []PreferredNodeAffinity{
		{Weight: 20, Requirements: []Requirement{{Key: "kubernetes.io/arch", Operator: "In", Values: []string{"amd64", "arm64"}}}},
		{Weight: 80, Requirements: []Requirement{{Key: "topology.kubernetes.io/zone", Operator: "In", Values: []string{"a", "b"}}}},
	}
	second.Pod.SameApplicationPodAntiAffinity = []SameApplicationPodAntiAffinity{
		{Enforcement: "required", TopologyKey: "kubernetes.io/hostname"},
		{Enforcement: "preferred", TopologyKey: "topology.kubernetes.io/zone", Weight: &weight},
	}
	store := NewMemoryStore()
	created, err := store.Create(context.Background(), cmd("canonical-affinity"), "canonical-affinity", first, []Assignment{{TeamScope, tid}})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Create(context.Background(), cmd("canonical-affinity"), "canonical-affinity", second, []Assignment{{TeamScope, tid}})
	if err != nil || !replayed.Replay || replayed.Revision.SpecDigest != created.Revision.SpecDigest {
		t.Fatalf("semantic permutation was not canonical: %#v %v", replayed, err)
	}
	mutated := second
	mutated.Pod.PreferredNodeAffinity[0].Weight++
	if _, err = store.Create(context.Background(), cmd("canonical-affinity"), "canonical-affinity", mutated, []Assignment{{TeamScope, tid}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("preferred weight digest mutation was not detected: %v", err)
	}
}

func TestCanonicalizationRejectsDuplicatePreferredAndAntiAffinityTerms(t *testing.T) {
	requirements := []Requirement{{Key: "topology.kubernetes.io/zone", Operator: "In", Values: []string{"zone-b", "zone-a"}}}
	weight := 30
	for name, pod := range map[string]PodScheduling{
		"preferred node": {
			PreferredNodeAffinity: []PreferredNodeAffinity{
				{Weight: 70, Requirements: requirements},
				{Weight: 70, Requirements: []Requirement{{Key: "topology.kubernetes.io/zone", Operator: "In", Values: []string{"zone-a", "zone-b"}}}},
			},
		},
		"required anti-affinity": {
			SameApplicationPodAntiAffinity: []SameApplicationPodAntiAffinity{
				{Enforcement: "required", TopologyKey: "kubernetes.io/hostname"},
				{Enforcement: "required", TopologyKey: "kubernetes.io/hostname"},
			},
		},
		"preferred anti-affinity default": {
			SameApplicationPodAntiAffinity: []SameApplicationPodAntiAffinity{
				{Enforcement: "preferred", TopologyKey: "topology.kubernetes.io/zone"},
				{Enforcement: "preferred", TopologyKey: "topology.kubernetes.io/zone", Weight: intPointer(100)},
			},
		},
		"preferred anti-affinity explicit": {
			SameApplicationPodAntiAffinity: []SameApplicationPodAntiAffinity{
				{Enforcement: "preferred", TopologyKey: "kubernetes.io/hostname", Weight: &weight},
				{Enforcement: "preferred", TopologyKey: "kubernetes.io/hostname", Weight: intPointer(30)},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewMemoryStore().Create(context.Background(), cmd("duplicate-"+strings.ReplaceAll(name, " ", "-")), "duplicate-"+strings.ReplaceAll(name, " ", "-"), Spec{Pod: pod}, []Assignment{{TeamScope, tid}}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("duplicate accepted: %v", err)
			}
		})
	}
}

func TestMatchesRejectsDirectGitPreferredAndAntiAffinityTamper(t *testing.T) {
	weight := 35
	resolution := Resolution{
		Ref:               Ref{ProfileID: "11111111-1111-4111-8111-111111111111", Revision: 4},
		SpecDigest:        "sha256:" + strings.Repeat("a", 64),
		AssignmentsDigest: "sha256:" + strings.Repeat("b", 64),
		Pod: PodScheduling{
			PreferredNodeAffinity:          []PreferredNodeAffinity{{Weight: 60, Requirements: []Requirement{{Key: "topology.kubernetes.io/zone", Operator: "Exists"}}}},
			SameApplicationPodAntiAffinity: []SameApplicationPodAntiAffinity{{Enforcement: "preferred", TopologyKey: "kubernetes.io/hostname", Weight: &weight}},
		},
	}
	applicationID := "55555555-5555-4555-8555-555555555555"
	runtime, err := Materialize(domain.DefaultWorkloadRuntime(8080, nil), resolution, applicationID)
	if err != nil || !Matches(runtime, resolution, applicationID) {
		t.Fatalf("baseline materialization: %v", err)
	}
	runtime.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].Weight = 61
	if Matches(runtime, resolution, applicationID) {
		t.Fatal("direct-Git preferred node weight substitution matched")
	}
	runtime, _ = Materialize(domain.DefaultWorkloadRuntime(8080, nil), resolution, applicationID)
	runtime.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].PodAffinityTerm.TopologyKey = "topology.kubernetes.io/zone"
	if Matches(runtime, resolution, applicationID) {
		t.Fatal("direct-Git anti-affinity topology substitution matched")
	}
}

func intPointer(value int) *int { return &value }

func TestRejectsBroadOrSemanticallyInvalidProfileToleration(t *testing.T) {
	for index, toleration := range []Toleration{
		{Operator: "Exists", Effect: "NoSchedule"},
		{Key: "workload.example/class", Operator: "Exists", Value: "x", Effect: "NoSchedule"},
		{Key: "workload.example/class", Operator: "Equal", Effect: "NoSchedule"},
	} {
		bad := spec("general")
		bad.Pod.Tolerations = []Toleration{toleration}
		if _, err := NewMemoryStore().Create(context.Background(), cmd(fmt.Sprintf("bad-toleration-%d", index)), "general", bad, []Assignment{{TeamScope, tid}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("case %d: %v", index, err)
		}
	}
}
