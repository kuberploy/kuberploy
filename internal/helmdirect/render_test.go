package helmdirect

import (
	"strings"
	"testing"
	"time"
)

func renderFixture(kind SourceKind) Revision {
	source := Source{Kind: kind, RepositoryURL: "https://github.com/valkey-io/valkey-helm.git", Path: "valkey", TargetRevision: "main"}
	if kind == SourceHelmRepository {
		source = Source{Kind: kind, RepositoryURL: "https://charts.example.test", Chart: "valkey", TargetRevision: "0.11.0"}
	}
	if kind == SourceOCI {
		source = Source{Kind: kind, RepositoryURL: "ghcr.io/example/charts", Chart: "valkey", TargetRevision: "0.11.0"}
	}
	now := time.Now().UTC()
	values := []byte("replicaCount: 1\n")
	return Revision{ID: "11111111-1111-4111-8111-111111111111", Generation: 1,
		Target:      Target{ProjectID: "22222222-2222-4222-8222-222222222222", EnvironmentID: "33333333-3333-4333-8333-333333333333", ApplicationID: "44444444-4444-4444-8444-444444444444"},
		ReleaseName: "valkey", DestinationNamespace: "kp-demo-production", ArgoProject: "kp-demo-production",
		Source: source, ValuesYAML: values, ValuesDigest: Digest(values), Action: ActionDeploy, DesiredEnabled: true,
		State: StatePending, ActorID: "55555555-5555-4555-8555-555555555555", IdempotencyKey: "11111111-1111-4111-8111-111111111111", RequestID: "request", CreatedAt: now, UpdatedAt: now}
}

func TestRenderApplicationForwardsEverySourceAndValuesToArgo(t *testing.T) {
	for _, kind := range []SourceKind{SourceHelmRepository, SourceOCI, SourceGit} {
		raw, err := RenderApplication(renderFixture(kind), ArgoNamespace)
		if err != nil {
			t.Fatalf("%s render: %v", kind, err)
		}
		text := string(raw)
		for _, required := range []string{"kind: Application", "project: kp-demo-production", "releaseName: valkey", "replicaCount: 1", "ServerSideApply=true"} {
			if !strings.Contains(text, required) {
				t.Fatalf("%s manifest omitted %q:\n%s", kind, required, text)
			}
		}
		if kind == SourceGit && (!strings.Contains(text, "path: valkey") || strings.Contains(text, "chart:")) {
			t.Fatalf("Git Helm source shape drifted:\n%s", text)
		}
		if kind != SourceGit && !strings.Contains(text, "chart: valkey") {
			t.Fatalf("repository Helm source omitted chart:\n%s", text)
		}
	}
}
