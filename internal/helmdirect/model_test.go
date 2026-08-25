package helmdirect

import "testing"

func TestSourceNormalizesAllArgoHelmModes(t *testing.T) {
	cases := []Source{
		{Kind: SourceHelmRepository, RepositoryURL: "https://charts.example.test/stable", Chart: "valkey", TargetRevision: "0.11.0"},
		{Kind: SourceOCI, RepositoryURL: "oci://ghcr.io/example/charts", Chart: "app", TargetRevision: "1.2.3"},
		{Kind: SourceGit, RepositoryURL: "https://github.com/valkey-io/valkey-helm.git", Path: "valkey", TargetRevision: "main"},
		{Kind: SourceGit, RepositoryURL: "ssh://git@gitlab.example.test/team/charts.git", Path: "charts/app", TargetRevision: "main"},
	}
	for _, source := range cases {
		if _, err := source.Normalize(); err != nil {
			t.Fatalf("source %#v rejected: %v", source, err)
		}
	}
}

func TestSourceRejectsCredentialsAndTraversalInCoordinates(t *testing.T) {
	cases := []Source{
		{Kind: SourceHelmRepository, RepositoryURL: "https://user:pass@example.test", Chart: "app", TargetRevision: "1.0.0"},
		{Kind: SourceOCI, RepositoryURL: "user:pass@example.test/charts", Chart: "app", TargetRevision: "1.0.0"},
		{Kind: SourceGit, RepositoryURL: "https://example.test/repo.git", Path: "../chart", TargetRevision: "main"},
	}
	for _, source := range cases {
		if _, err := source.Normalize(); err == nil {
			t.Fatalf("unsafe source accepted: %#v", source)
		}
	}
}

func TestNormalizeValuesRequiresOneMapping(t *testing.T) {
	if got, err := NormalizeValues([]byte("replicaCount: 1\nservice:\n  port: 6379\n")); err != nil || len(got) == 0 {
		t.Fatalf("normal values rejected: %q %v", got, err)
	}
	if got, err := NormalizeValues(nil); err != nil || string(got) != "{}\n" {
		t.Fatalf("empty values not normalized: %q %v", got, err)
	}
	for _, raw := range [][]byte{[]byte("- one\n- two\n"), []byte("a: 1\n---\nb: 2\n")} {
		if _, err := NormalizeValues(raw); err == nil {
			t.Fatalf("invalid values accepted: %q", raw)
		}
	}
}
