package builder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaximumValidBuildResultFitsKubernetesTerminationMessage(t *testing.T) {
	result := BuildResult{
		APIVersion: ProtocolVersion, OperationID: "11111111-1111-4111-8111-111111111111", Generation: 1_000_000_000,
		Status: "Succeeded",
		Image: Image{
			Reference: strings.Repeat("r", 439) + "@sha256:" + strings.Repeat("a", 64),
			Digest:    "sha256:" + strings.Repeat("a", 64),
			Platforms: []string{"linux/amd64", "linux/arm64"},
		},
		Cache: &Cache{
			Reference: strings.Repeat("c", 480) + ":generation-1000000000",
			Digest:    "sha256:" + strings.Repeat("b", 64),
		},
		CacheReuse: CacheReuseHit,
		Warnings:   []Warning{WarningColdBuild, WarningCacheDegraded},
		StartedAt:  time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), CompletedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= MaxTerminationResultBytes {
		t.Fatalf("maximal typed result is %d bytes, Kubernetes limit is %d", len(encoded), MaxTerminationResultBytes)
	}
	path := filepath.Join(t.TempDir(), "result.json")
	if err = os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = WriteTerminationResultAtomic(path, result); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil || string(stored) != string(encoded) {
		t.Fatalf("stored termination result changed: %v", err)
	}
}

func TestTerminationResultRejectsTruncationBeforePublish(t *testing.T) {
	for name, result := range map[string]any{
		"at-boundary": map[string]string{"x": strings.Repeat("x", MaxTerminationResultBytes-8)},
		"oversized":   map[string]string{"oversized": strings.Repeat("x", MaxTerminationResultBytes)},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.json")
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if name == "at-boundary" && len(encoded) != MaxTerminationResultBytes {
				t.Fatalf("test fixture is %d bytes, want exact %d-byte boundary", len(encoded), MaxTerminationResultBytes)
			}
			if err = WriteTerminationResultAtomic(path, result); err == nil {
				t.Fatal("potentially truncated termination result was published")
			}
			stored, readErr := os.ReadFile(path)
			if readErr != nil || len(stored) != 0 {
				t.Fatalf("rejected result changed the pre-created file: %q, %v", stored, readErr)
			}
		})
	}
}

func TestTerminationResultRequiresPrecreatedRegularFile(t *testing.T) {
	result := map[string]string{"status": "Failed"}
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing.json")
	if err := WriteTerminationResultAtomic(missing, result); err == nil {
		t.Fatal("missing runtime termination file was created")
	}
	target := filepath.Join(directory, "target.json")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "result.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteTerminationResultAtomic(link, result); err == nil {
		t.Fatal("symlink termination file was followed")
	}
	stored, err := os.ReadFile(target)
	if err != nil || string(stored) != "unchanged" {
		t.Fatalf("symlink target changed: %q, %v", stored, err)
	}
}
