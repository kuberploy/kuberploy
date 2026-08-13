package store

import (
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func TestRegistryCleanupPlanCanResumeOnlyUnfinishedOfflineSweep(t *testing.T) {
	plan := domain.RegistryCleanupPlan{
		State:   "failed",
		Failure: "managed registry cleanup execution failed",
		Items: []domain.RegistryCleanupItem{
			{ResourceKind: "release-manifest", Disposition: domain.RegistryCleanupDelete, Action: "delete-manifest", State: "deleted"},
			{ResourceKind: "blob", Disposition: domain.RegistryCleanupDelete, Action: "garbage-collect-blob", State: "deleting"},
			{ResourceKind: "blob", Disposition: domain.RegistryCleanupProtect, Action: "none", State: "protected"},
		},
	}
	if !RegistryCleanupPlanCanResumeOfflineSweep(plan) {
		t.Fatal("exact unfinished offline sweep was not recoverable")
	}

	cases := map[string]func(*domain.RegistryCleanupPlan){
		"non-terminal":        func(plan *domain.RegistryCleanupPlan) { plan.State = "executing" },
		"missing failure":     func(plan *domain.RegistryCleanupPlan) { plan.Failure = "" },
		"planned candidate":   func(plan *domain.RegistryCleanupPlan) { plan.Items[1].State = "planned" },
		"failed candidate":    func(plan *domain.RegistryCleanupPlan) { plan.Items[1].State = "failed" },
		"manifest unfinished": func(plan *domain.RegistryCleanupPlan) { plan.Items[0].State = "deleting" },
		"wrong blob action":   func(plan *domain.RegistryCleanupPlan) { plan.Items[1].Action = "delete-manifest" },
		"nothing unfinished":  func(plan *domain.RegistryCleanupPlan) { plan.Items[1].State = "deleted" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := plan
			candidate.Items = append([]domain.RegistryCleanupItem(nil), plan.Items...)
			mutate(&candidate)
			if RegistryCleanupPlanCanResumeOfflineSweep(candidate) {
				t.Fatal("unsafe failed plan was recoverable")
			}
		})
	}
}
