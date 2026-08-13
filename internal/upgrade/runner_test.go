package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func TestExecutableUsesClosedPersistedProtocolAndDeterministicJobName(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "request.json")
	operation := domain.Operation{ID: "11111111-2222-4333-8444-555555555555", Generation: 7}
	expected := JobName(operation.ID, operation.Generation)
	script := filepath.Join(dir, "runner.sh")
	source := "#!/bin/sh\ncat > " + capture + "\nprintf '%s\\n' '{\"runnerRef\":\"" + expected + "\",\"details\":{\"helmRevision\":3}}'\n"
	if err := os.WriteFile(script, []byte(source), 0o700); err != nil {
		t.Fatal(err)
	}
	manifestBytes := []byte(`{"release":{"tag":"v1.1.0","version":"1.1.0"}}`)
	sum := sha256.Sum256(manifestBytes)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	u := domain.PlatformUpgrade{Version: "1.1.0", ManifestDigest: digest, ManifestBytes: manifestBytes, Action: "rollback", HelmRevision: 3}
	result, err := (Executable{Path: script, Namespace: "kuberploy-system", ReleaseName: "kuberploy"}).Run(context.Background(), operation, u)
	if err != nil {
		t.Fatal(err)
	}
	if result.RunnerRef != expected || result.Details["helmRevision"] != float64(3) {
		t.Fatalf("result %#v", result)
	}
	body, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err = json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if request["jobName"] != expected || request["targetVersion"] != "1.1.0" || request["manifestDigest"] != digest {
		t.Fatalf("request %#v", request)
	}
	if request["action"] != "rollback" || request["helmRevision"] != float64(3) {
		t.Fatalf("rollback protocol %#v", request)
	}
	if _, ok := request["url"]; ok {
		t.Fatal("runner protocol must never accept an arbitrary URL")
	}
	if _, ok := request["manifest"]; ok {
		t.Fatal("runner protocol must use exact manifest bytes, not a JSON reserialization")
	}
}

func TestJobNameIsBoundedAndStable(t *testing.T) {
	got := JobName("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", 99)
	if got != "kuberploy-upgrade-aaaaaaaabbbb-g99" {
		t.Fatalf("job name %q", got)
	}
	if len(got) > 63 {
		t.Fatal("job name exceeds Kubernetes limit")
	}
}
