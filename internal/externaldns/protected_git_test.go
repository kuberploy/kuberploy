package externaldns

import (
	"testing"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

func TestProtectedOperationIdentityIsStableForRecoveryAndRevisionBound(t *testing.T) {
	item := runtimeIntegration()
	config := ProtectedGitConfig{BindingID: "33333333-3333-4333-8333-333333333333", Owner: "edge-worker:recovery-test", Template: runtimeTemplate()}
	content, _, err := RenderManagedBundle(item, config.Template)
	if err != nil {
		t.Fatal(err)
	}
	contentDigest := digest(content)
	first := externalDNSOperationID(config, item, gitprojection.MutationUpsert, contentDigest)
	second := externalDNSOperationID(config, item, gitprojection.MutationUpsert, contentDigest)
	if first != second {
		t.Fatalf("recovery operation identity drifted: %s %s", first, second)
	}
	revised := item
	revised.RuntimeRevision++
	revisedID := externalDNSOperationID(config, revised, gitprojection.MutationUpsert, contentDigest)
	deletedID := externalDNSOperationID(config, item, gitprojection.MutationDelete, contentDigest)
	if revisedID == first || deletedID == first || deletedID == revisedID {
		t.Fatal("operation identity did not bind revision and action")
	}
}
