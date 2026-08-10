package httpapi

import (
	"bytes"
	"testing"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

func TestVariableSaveRequestFingerprintBindsParserPolicy(t *testing.T) {
	plan := gitprojection.WritePlan{BindingID: "11111111-1111-4111-8111-111111111111", ProjectID: "22222222-2222-4222-8222-222222222222",
		EnvironmentID: "33333333-3333-4333-8333-333333333333", BaseRevision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Precondition: gitprojection.MutationCreateIfAbsent, PolicyVersion: "variables/v1", VariableScope: "project",
		VariablePath: "tenants/22222222-2222-4222-8222-222222222222/variables.yaml"}
	tokenHash, candidateHash := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	first := variableSaveRequestFingerprint("44444444-4444-4444-8444-444444444444", plan.EnvironmentID, plan.VariableScope, plan, tokenHash, candidateHash)
	if repeated := variableSaveRequestFingerprint("44444444-4444-4444-8444-444444444444", plan.EnvironmentID, plan.VariableScope, plan, tokenHash, candidateHash); repeated != first {
		t.Fatalf("canonical request digest changed: first=%q repeated=%q", first, repeated)
	}
	plan.PolicyVersion = "variables/v2"
	if changed := variableSaveRequestFingerprint("44444444-4444-4444-8444-444444444444", plan.EnvironmentID, plan.VariableScope, plan, tokenHash, candidateHash); changed == first {
		t.Fatal("parser policy substitution did not change the request digest")
	}
}
