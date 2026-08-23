package gitprojection

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestExternalDNSMutationAuthorityIsClosedToExactIntegrationBundle(t *testing.T) {
	bindingID, integrationID, operationID := "11111111-1111-4111-8111-111111111111", "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"
	binding, err := NewGitHubPlatformBinding(bindingID, RepositoryIdentity{Provider: "github", InstallationID: 9000000000001, RepositoryID: 9000000000002, Owner: "kuberploy", Name: "platform"}, "refs/heads/main", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("{\"apiVersion\":\"v1\",\"kind\":\"ConfigMap\"}\n")
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	base := strings.Repeat("a", 40)
	mutation := Mutation{BindingID: binding.ID, OperationID: operationID, Path: binding.Prefix + "/argocd/platform/external-dns/" + integrationID + ".yaml", BaseRevision: base, Precondition: MutationCreateIfAbsent, Content: content, ContentSHA256: digest, Message: "materialize external-dns integration", Action: MutationUpsert, Authority: MutationAuthorityExternalDNS, CommitTrailer: "Kuberploy-External-DNS-Intent: " + operationID, RequiredAncestor: base}
	if err = mutation.Validate(binding); err != nil {
		t.Fatal(err)
	}
	cases := []Mutation{mutation, mutation, mutation, mutation}
	cases[0].Path = binding.Prefix + "/argocd/platform/external-dns/escape.yaml"
	cases[1].CommitTrailer = "Kuberploy-External-DNS-Intent: " + integrationID
	cases[2].Content = append(cases[2].Content, 'x')
	cases[3].Authority = MutationAuthorityCertificateIssuer
	for index, candidate := range cases {
		if candidate.Validate(binding) == nil {
			t.Fatalf("authority escape %d accepted", index)
		}
	}
}
