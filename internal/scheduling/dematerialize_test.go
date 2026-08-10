package scheduling_test

import (
	"reflect"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/scheduling"
)

func TestDematerializeStripsOnlyProfileOwnedPlacement(t *testing.T) {
	runtime := domain.DefaultWorkloadRuntime(8080, nil)
	runtime.SchedulingProfile = &domain.SchedulingProfileRef{ProfileID: "11111111-1111-4111-8111-111111111111", Revision: 1,
		SpecDigest:        "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AssignmentsDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	runtime.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"}
	runtime.Affinity = &domain.WorkloadAffinity{NodeAffinity: &domain.NodeAffinity{}}
	runtime.TopologySpreadConstraints = []domain.TopologySpreadConstraint{{MaxSkew: 1}}
	runtime.Tolerations = []domain.WorkloadToleration{{Key: "dedicated", Operator: "Exists", Effect: "NoSchedule"}}
	runtime.PriorityClassName = "workload-high"
	dematerialized := scheduling.Dematerialize(runtime)
	if dematerialized.SchedulingProfile == nil || scheduling.HasEffectiveMaterial(dematerialized) {
		t.Fatalf("profile authority was lost or effective fields retained: %#v", dematerialized)
	}
	legacy := runtime
	legacy.SchedulingProfile = nil
	if got := scheduling.Dematerialize(legacy); !reflect.DeepEqual(got, legacy) {
		t.Fatal("legacy free-form scheduling was silently stripped instead of failing at admission")
	}
}
