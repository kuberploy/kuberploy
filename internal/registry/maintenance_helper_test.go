package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPhysicalRegistryCheckpointProvesExplicitReachability(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "docker", "registry", "v2")
	configBody := []byte("config-body")
	configDigest := digestBytes(configBody)
	manifestBody := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d,"artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json"},"layers":[]}`,
		ociManifestMediaType, configDigest, len(configBody)))
	manifestDigest := digestBytes(manifestBody)
	indexBody := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd64"}}]}`,
		ociIndexMediaType, ociManifestMediaType, manifestDigest, len(manifestBody)))
	indexDigest := digestBytes(indexBody)
	danglingBody := []byte("unreachable-blob")
	danglingDigest := digestBytes(danglingBody)
	writeRegistryBlobForTest(t, base, configDigest, configBody)
	writeRegistryBlobForTest(t, base, manifestDigest, manifestBody)
	writeRegistryBlobForTest(t, base, indexDigest, indexBody)
	writeRegistryBlobForTest(t, base, danglingDigest, danglingBody)
	revisionLink := filepath.Join(base, "repositories", "kuberploy", "service", "_manifests", "revisions", "sha256",
		indexDigest[len("sha256:"):], "link")
	if err := os.MkdirAll(filepath.Dir(revisionLink), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(revisionLink, []byte(indexDigest), 0o600); err != nil {
		t.Fatal(err)
	}
	candidateDigest, candidates, err := cleanupCandidateSetDigest([]string{danglingDigest, configDigest})
	if err != nil {
		t.Fatal(err)
	}
	request := maintenanceHelperRequest{Version: 1, Mode: "checkpoint", TargetID: "11111111-1111-4111-8111-111111111111",
		PlanID: "22222222-2222-4222-8222-222222222222", PlanDigest: "sha256:" + repeatHex("a", 64),
		ExecutionKey: "sha256:" + repeatHex("b", 64), CandidateSetDigest: candidateDigest,
		CandidateDigests: candidates, NotBefore: time.Now().UTC().Add(-time.Minute)}
	checkpoint, err := scanRegistryStorageAt(context.Background(), root, request)
	if err != nil || !checkpoint.RegistryWide || !checkpoint.InventoryComplete || !checkpoint.ReachabilityComplete || len(checkpoint.Blobs) != 2 {
		t.Fatalf("checkpoint=%+v err=%v", checkpoint, err)
	}
	reachability := make(map[string]RegistryBlobReachability, len(checkpoint.Blobs))
	for _, row := range checkpoint.Blobs {
		reachability[row.Digest] = row
	}
	if row := reachability[configDigest]; !row.Present || !row.Reachable {
		t.Fatalf("referenced config row=%+v", row)
	}
	if row := reachability[danglingDigest]; !row.Present || row.Reachable {
		t.Fatalf("dangling blob row=%+v", row)
	}

	if err = os.Symlink(filepath.Join(base, "blobs"), filepath.Join(base, "attacker-link")); err != nil {
		t.Fatal(err)
	}
	if _, err = scanRegistryStorageAt(context.Background(), root, request); err != ErrRegistryCheckpointIncomplete {
		t.Fatalf("symlinked registry tree accepted: %v", err)
	}
}

func TestPhysicalRegistryCheckpointAcceptsOCIManifestWithoutTopLevelMediaType(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "docker", "registry", "v2")
	configBody := []byte("helm-config")
	configDigest := digestBytes(configBody)
	chartBody := []byte("helm-chart")
	chartDigest := digestBytes(chartBody)
	manifestBody := []byte(fmt.Sprintf(`{"schemaVersion":2,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":%q,"size":%d}]}`,
		configDigest, len(configBody), chartDigest, len(chartBody)))
	manifestDigest := digestBytes(manifestBody)
	writeRegistryBlobForTest(t, base, configDigest, configBody)
	writeRegistryBlobForTest(t, base, chartDigest, chartBody)
	writeRegistryBlobForTest(t, base, manifestDigest, manifestBody)
	revisionLink := filepath.Join(base, "repositories", "kuberploy", "chart", "_manifests", "revisions", "sha256",
		manifestDigest[len("sha256:"):], "link")
	if err := os.MkdirAll(filepath.Dir(revisionLink), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(revisionLink, []byte(manifestDigest), 0o600); err != nil {
		t.Fatal(err)
	}
	candidateDigest, candidates, err := cleanupCandidateSetDigest([]string{chartDigest})
	if err != nil {
		t.Fatal(err)
	}
	request := maintenanceHelperRequest{Version: 1, Mode: "checkpoint", TargetID: "11111111-1111-4111-8111-111111111111",
		PlanID: "22222222-2222-4222-8222-222222222222", PlanDigest: "sha256:" + repeatHex("a", 64),
		ExecutionKey: "sha256:" + repeatHex("b", 64), CandidateSetDigest: candidateDigest,
		CandidateDigests: candidates, NotBefore: time.Now().UTC().Add(-time.Minute)}
	checkpoint, err := scanRegistryStorageAt(context.Background(), root, request)
	if err != nil || len(checkpoint.Blobs) != 1 || !checkpoint.Blobs[0].Present || !checkpoint.Blobs[0].Reachable {
		t.Fatalf("checkpoint=%+v err=%v", checkpoint, err)
	}
}

func writeRegistryBlobForTest(t *testing.T, base, digest string, body []byte) {
	t.Helper()
	hash := digest[len("sha256:"):]
	path := filepath.Join(base, "blobs", "sha256", hash[:2], hash, "data")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}
