package gitprojection

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestFoundationMutationAuthorityIsExactAndCannotBeSubstituted(t *testing.T) {
	const (
		bindingID = "11111111-1111-4111-8111-111111111111"
		clusterID = "22222222-2222-4222-8222-222222222222"
		envID     = "33333333-3333-4333-8333-333333333333"
		intentID  = "44444444-4444-4444-8444-444444444444"
	)
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	repository := RepositoryIdentity{Provider: "github", InstallationID: 1, RepositoryID: 2, Owner: "kuberploy", Name: "platform"}
	binding, err := NewGitHubPlatformBinding(bindingID, clusterID, repository, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("apiVersion: v1\nkind: Namespace\n")
	sum := sha256.Sum256(content)
	mutation := Mutation{BindingID: bindingID, OperationID: intentID,
		Path:         PlatformPrefix(clusterID) + "/argocd/foundations/" + envID + ".yaml",
		BaseRevision: strings.Repeat("b", 40), RequiredAncestor: strings.Repeat("a", 40),
		Precondition: MutationCreateIfAbsent, Action: MutationUpsert, Content: content,
		ContentSHA256: "sha256:" + hex.EncodeToString(sum[:]), Message: "publish environment foundation " + envID,
		Authority: MutationAuthorityFoundation, CommitTrailer: "Kuberploy-Environment-Foundation-Intent: " + intentID}
	if err = mutation.Validate(binding); err != nil {
		t.Fatalf("exact foundation mutation rejected: %v", err)
	}

	tests := map[string]func(*Mutation){
		"ordinary authority bypass": func(v *Mutation) { v.Authority, v.CommitTrailer, v.RequiredAncestor, v.ContentSHA256 = "", "", "", "" },
		"helm authority":            func(v *Mutation) { v.Authority = MutationAuthorityHelmApplication },
		"path sibling":              func(v *Mutation) { v.Path = PlatformPrefix(clusterID) + "/argocd/environments/" + envID + ".yaml" },
		"path suffix substitution": func(v *Mutation) {
			v.Path = PlatformPrefix(clusterID) + "/argocd/foundations/" + envID + "/foundation.yaml"
		},
		"path traversal":   func(v *Mutation) { v.Path = PlatformPrefix(clusterID) + "/argocd/foundations/../" + envID + ".yaml" },
		"wrong trailer":    func(v *Mutation) { v.CommitTrailer = "Kuberploy-Operation: " + intentID },
		"missing ancestor": func(v *Mutation) { v.RequiredAncestor = "" },
		"digest mismatch":  func(v *Mutation) { v.ContentSHA256 = "sha256:" + strings.Repeat("0", 64) },
		"delete":           func(v *Mutation) { v.Action = MutationDelete },
		"match update": func(v *Mutation) {
			v.Precondition = MutationMatchETag
			v.ExpectedETag = `"sha256:` + strings.Repeat("0", 64) + `"`
		},
	}
	for name, alter := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := mutation
			candidate.Content = append([]byte(nil), mutation.Content...)
			alter(&candidate)
			if candidate.Validate(binding) == nil {
				t.Fatal("adversarial foundation mutation accepted")
			}
		})
	}

	environment, err := NewGitHubEnvironmentBinding(bindingID, "55555555-5555-4555-8555-555555555555", envID, repository, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	mutation.Path = environment.Prefix + "/argocd/foundations/" + envID + ".yaml"
	if mutation.Validate(environment) == nil {
		t.Fatal("environment binding obtained platform foundation authority")
	}
}
