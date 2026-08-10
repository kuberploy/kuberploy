package observability

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectedBearerTokenReadsExactRegularFileAndKubeletSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	version := filepath.Join(root, "..2026_08_09")
	if err := os.Mkdir(version, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(version, "token")
	if err := os.WriteFile(target, []byte("exact-token-123"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "token")
	if err := os.Symlink(filepath.Join("..2026_08_09", "token"), link); err != nil {
		t.Fatal(err)
	}
	source := ProjectedBearerToken{path: link}
	raw, err := source.ReadToken(context.Background())
	if err != nil || string(raw) != "exact-token-123" {
		t.Fatalf("raw=%q err=%v", raw, err)
	}
}

func TestProjectedBearerTokenRejectsUnsafeInputWithGenericErrors(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tests := []struct {
		name string
		path string
	}{
		{name: "missing", path: filepath.Join(root, "provider-secret-name")},
		{name: "directory", path: root},
	}
	oversize := filepath.Join(root, "oversize")
	if err := os.WriteFile(oversize, []byte(strings.Repeat("x", 8193)), 0o600); err != nil {
		t.Fatal(err)
	}
	tests = append(tests, struct {
		name string
		path string
	}{name: "oversize", path: oversize})
	for _, test := range tests {
		_, err := (ProjectedBearerToken{path: test.path}).ReadToken(context.Background())
		if err == nil || err.Error() != "monitoring credential unavailable" || strings.Contains(err.Error(), test.path) {
			t.Fatalf("case=%s error=%v", test.name, err)
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (ProjectedBearerToken{path: oversize}).ReadToken(cancelled); err == nil || err.Error() != "monitoring credential unavailable" {
		t.Fatalf("cancelled error=%v", err)
	}
}
