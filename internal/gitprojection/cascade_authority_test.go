package gitprojection

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestHelmCascadeMutationAuthorityIsClosedToFinalizerCAS(t *testing.T) {
	const (
		bindingID = "11111111-1111-4111-8111-111111111111"
		envID     = "33333333-3333-4333-8333-333333333333"
		appID     = "44444444-4444-4444-8444-444444444444"
		intentID  = "55555555-5555-4555-8555-555555555555"
	)
	repository := RepositoryIdentity{Provider: "github", InstallationID: 1, RepositoryID: 2, Owner: "kuberploy", Name: "platform"}
	binding, err := NewGitHubPlatformBinding(bindingID, repository, "refs/heads/main", time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\n")
	sum := sha256.Sum256(content)
	mutation := Mutation{
		BindingID: bindingID, OperationID: intentID,
		Path:         PlatformPrefix() + "/argocd/helm-applications/" + envID + "/" + appID + ".yaml",
		BaseRevision: strings.Repeat("b", 40), RequiredAncestor: strings.Repeat("a", 40),
		Precondition: MutationMatchETag, ExpectedETag: `"sha256:` + strings.Repeat("f", 64) + `"`,
		Action: MutationUpsert, Content: content, ContentSHA256: "sha256:" + hex.EncodeToString(sum[:]),
		Message: "adopt foreground cascade finalizer", Authority: MutationAuthorityHelmCascade,
		CommitTrailer: "Kuberploy-Helm-Cascade-Preflight: " + intentID,
	}
	if err = mutation.Validate(binding); err != nil {
		t.Fatalf("exact cascade mutation rejected: %v", err)
	}

	tests := map[string]func(*Mutation){
		"application authority": func(v *Mutation) { v.Authority = MutationAuthorityHelmApplication },
		"payload authority":     func(v *Mutation) { v.Authority = MutationAuthorityHelmPayload },
		"wrong trailer":         func(v *Mutation) { v.CommitTrailer = "Kuberploy-Helm-Application-Intent: " + intentID },
		"missing ancestor":      func(v *Mutation) { v.RequiredAncestor = "" },
		"create":                func(v *Mutation) { v.Precondition, v.ExpectedETag = MutationCreateIfAbsent, "" },
		"delete":                func(v *Mutation) { v.Action, v.Content, v.ContentSHA256 = MutationDelete, nil, "" },
		"payload path": func(v *Mutation) {
			v.Path = PlatformPrefix() + "/helm-manifests/environments/" + envID + "/applications/" + appID + "/revisions/66666666-6666-4666-8666-666666666666/release.yaml"
		},
		"digest mismatch": func(v *Mutation) { v.ContentSHA256 = "sha256:" + strings.Repeat("0", 64) },
	}
	for name, alter := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := mutation
			candidate.Content = append([]byte(nil), mutation.Content...)
			alter(&candidate)
			if candidate.Validate(binding) == nil {
				t.Fatal("adversarial cascade mutation accepted")
			}
		})
	}
}
