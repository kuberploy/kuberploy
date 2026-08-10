package scheduling

import (
	"reflect"

	"github.com/kuberploy/kuberploy/internal/domain"
)

// HasEffectiveMaterial reports fields that may influence Pod placement. These
// fields are never accepted as caller authority; they are persisted only as an
// exact server materialization of SchedulingProfile.
func HasEffectiveMaterial(runtime domain.WorkloadRuntime) bool {
	return len(runtime.NodeSelector) != 0 || runtime.Affinity != nil ||
		len(runtime.TopologySpreadConstraints) != 0 || len(runtime.Tolerations) != 0 ||
		runtime.PriorityClassName != ""
}

// Dematerialize removes only fields proven to be owned by an exact scheduling
// profile reference. It is used when a server-owned operation snapshot is fed
// back through deployment admission so the active immutable profile is freshly
// resolved. Legacy free-form placement without a profile is preserved and will
// continue to fail closed at admission.
func Dematerialize(runtime domain.WorkloadRuntime) domain.WorkloadRuntime {
	if runtime.SchedulingProfile == nil {
		return runtime
	}
	runtime.NodeSelector = nil
	runtime.Affinity = nil
	runtime.TopologySpreadConstraints = nil
	runtime.Tolerations = nil
	runtime.PriorityClassName = ""
	return runtime
}

// Materialize overwrites every profile-owned placement field. applicationID
// is used only for the closed topology-spread selector that targets this
// workload's stable runtime label.
func Materialize(runtime domain.WorkloadRuntime, resolution Resolution, applicationID string) (domain.WorkloadRuntime, error) {
	if !uuidRE.MatchString(applicationID) || resolution.Ref.ProfileID == "" || resolution.Ref.Revision < 1 ||
		resolution.SpecDigest == "" || resolution.AssignmentsDigest == "" || resolution.Pod.Validate() != nil {
		return domain.WorkloadRuntime{}, ErrInvalid
	}
	runtime = domain.NormalizeWorkloadRuntime(runtime)
	runtime.SchedulingProfile = ptr(resolution.DomainRef())
	runtime.NodeSelector = cloneStringMap(resolution.Pod.NodeSelector)
	runtime.Affinity = nil
	if len(resolution.Pod.RequiredNodeAffinity) != 0 || len(resolution.Pod.PreferredNodeAffinity) != 0 {
		nodeAffinity := &domain.NodeAffinity{}
		if len(resolution.Pod.RequiredNodeAffinity) != 0 {
			expressions := make([]domain.NodeSelectorRequirement, 0, len(resolution.Pod.RequiredNodeAffinity))
			for _, requirement := range resolution.Pod.RequiredNodeAffinity {
				expressions = append(expressions, domain.NodeSelectorRequirement{Key: requirement.Key, Operator: requirement.Operator, Values: append([]string(nil), requirement.Values...)})
			}
			nodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &domain.NodeSelector{NodeSelectorTerms: []domain.NodeSelectorTerm{{MatchExpressions: expressions}}}
		}
		for _, preferred := range resolution.Pod.PreferredNodeAffinity {
			expressions := make([]domain.NodeSelectorRequirement, 0, len(preferred.Requirements))
			for _, requirement := range preferred.Requirements {
				expressions = append(expressions, domain.NodeSelectorRequirement{Key: requirement.Key, Operator: requirement.Operator, Values: append([]string(nil), requirement.Values...)})
			}
			nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = append(nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution, domain.PreferredSchedulingTerm{Weight: preferred.Weight, Preference: domain.NodeSelectorTerm{MatchExpressions: expressions}})
		}
		runtime.Affinity = &domain.WorkloadAffinity{NodeAffinity: nodeAffinity}
	}
	if len(resolution.Pod.SameApplicationPodAntiAffinity) != 0 {
		if runtime.Affinity == nil {
			runtime.Affinity = &domain.WorkloadAffinity{}
		}
		anti := &domain.PodAffinity{}
		for _, preset := range resolution.Pod.SameApplicationPodAntiAffinity {
			term := domain.PodAffinityTerm{TopologyKey: preset.TopologyKey, LabelSelector: domain.LabelSelector{MatchLabels: map[string]string{"kuberploy.io/application": applicationID}}}
			if preset.Enforcement == "required" {
				anti.RequiredDuringSchedulingIgnoredDuringExecution = append(anti.RequiredDuringSchedulingIgnoredDuringExecution, term)
			} else {
				weight := 100
				if preset.Weight != nil {
					weight = *preset.Weight
				}
				anti.PreferredDuringSchedulingIgnoredDuringExecution = append(anti.PreferredDuringSchedulingIgnoredDuringExecution, domain.WeightedPodAffinityTerm{Weight: weight, PodAffinityTerm: term})
			}
		}
		runtime.Affinity.PodAntiAffinity = anti
	}
	runtime.Tolerations = make([]domain.WorkloadToleration, 0, len(resolution.Pod.Tolerations))
	for _, toleration := range resolution.Pod.Tolerations {
		runtime.Tolerations = append(runtime.Tolerations, domain.WorkloadToleration{Key: toleration.Key, Operator: toleration.Operator, Value: toleration.Value, Effect: toleration.Effect, TolerationSeconds: cloneInt64(toleration.TolerationSeconds)})
	}
	runtime.TopologySpreadConstraints = make([]domain.TopologySpreadConstraint, 0, len(resolution.Pod.TopologySpread))
	for _, spread := range resolution.Pod.TopologySpread {
		runtime.TopologySpreadConstraints = append(runtime.TopologySpreadConstraints, domain.TopologySpreadConstraint{
			MaxSkew: spread.MaxSkew, TopologyKey: spread.TopologyKey, WhenUnsatisfiable: spread.WhenUnsatisfiable,
			LabelSelector: domain.LabelSelector{MatchLabels: map[string]string{"kuberploy.io/application": applicationID}},
		})
	}
	runtime.PriorityClassName = resolution.Pod.PriorityClassName
	if len(domain.ValidateWorkloadRuntime(runtime)) != 0 {
		return domain.WorkloadRuntime{}, ErrInvalid
	}
	return runtime, nil
}

func Matches(runtime domain.WorkloadRuntime, resolution Resolution, applicationID string) bool {
	if runtime.SchedulingProfile == nil {
		return false
	}
	expected, err := Materialize(runtime, resolution, applicationID)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(runtime.SchedulingProfile, expected.SchedulingProfile) &&
		reflect.DeepEqual(runtime.NodeSelector, expected.NodeSelector) &&
		reflect.DeepEqual(runtime.Affinity, expected.Affinity) &&
		reflect.DeepEqual(runtime.TopologySpreadConstraints, expected.TopologySpreadConstraints) &&
		reflect.DeepEqual(runtime.Tolerations, expected.Tolerations) &&
		runtime.PriorityClassName == expected.PriorityClassName
}

func ptr[T any](value T) *T { return &value }
func cloneStringMap(value map[string]string) map[string]string {
	if len(value) == 0 {
		return nil
	}
	out := make(map[string]string, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}
func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
