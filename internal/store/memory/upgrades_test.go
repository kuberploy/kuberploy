package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kuberploy/kuberploy/internal/domain"
	base "github.com/kuberploy/kuberploy/internal/store"
)

func TestPlatformUpgradeRetainsExactManifestBytesWithoutExposingThem(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	original := []byte("{\n  \"release\": {\"version\": \"1.1.0\"}\n}\n")
	sum := sha256.Sum256(original)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	result, _, err := store.CreatePlatformUpgrade(ctx, admin.ID, "upgrade", "fingerprint", "request", domain.CreatePlatformUpgrade{Release: domain.ReleaseInfo{Version: "1.1.0", ManifestDigest: digest, ManifestBytes: original}})
	if err != nil {
		t.Fatal(err)
	}
	result.Value.ManifestBytes[0] = 'x'
	original[1] = 'x'
	persisted, err := store.GetPlatformUpgrade(ctx, result.Value.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("{\n  \"release\": {\"version\": \"1.1.0\"}\n}\n")
	if string(persisted.ManifestBytes) != string(want) {
		t.Fatalf("exact bytes changed: %q", persisted.ManifestBytes)
	}
	encoded, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) == 0 || bytes.Contains(encoded, persisted.ManifestBytes) {
		t.Fatal("private manifest bytes leaked through the API model")
	}
	bad := domain.CreatePlatformUpgrade{Release: domain.ReleaseInfo{Version: "1.2.0", ManifestDigest: digest, ManifestBytes: []byte("tampered")}}
	if _, _, err = store.CreatePlatformUpgrade(ctx, admin.ID, "bad", "bad", "request", bad); !errors.Is(err, base.ErrConflict) {
		t.Fatalf("digest mismatch err=%v", err)
	}
}

func TestPlatformRollbackCreatesNewDurableOperationFromVerifiedHistory(t *testing.T) {
	ctx := context.Background()
	store := New()
	admin := bootstrapAccessAdmin(t, store)
	manifest := []byte(`{"release":{"version":"1.1.0"}}`)
	sum := sha256.Sum256(manifest)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	created, _, err := store.CreatePlatformUpgrade(ctx, admin.ID, "upgrade", "upgrade-fingerprint", "request", domain.CreatePlatformUpgrade{Release: domain.ReleaseInfo{Version: "1.1.0", ManifestDigest: digest, ManifestBytes: manifest}})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	source := store.upgrades[created.Value.ID]
	source.State = "succeeded"
	store.upgrades[source.ID] = source
	store.operations[source.OperationID] = domain.Operation{ID: source.OperationID, Kind: "platform.upgrade", Status: "succeeded", TargetType: "platform-upgrade", TargetID: source.ID}
	store.mu.Unlock()

	rollback, operation, err := store.CreatePlatformRollback(ctx, admin.ID, "rollback", "rollback-fingerprint", "request-rollback", domain.CreatePlatformRollback{SourceUpgradeID: source.ID, HelmRevision: 3})
	if err != nil {
		t.Fatal(err)
	}
	if rollback.Value.Action != "rollback" || rollback.Value.HelmRevision != 3 || rollback.Value.SourceUpgradeID != source.ID || operation.Progress[0].Name != "rollback" {
		t.Fatalf("rollback=%#v operation=%#v", rollback.Value, operation)
	}
	if !bytes.Equal(rollback.Value.ManifestBytes, manifest) || rollback.Value.ManifestDigest != digest {
		t.Fatal("rollback did not retain exact verified release manifest")
	}
	items, err := store.ListPlatformUpgrades(ctx)
	if err != nil || len(items) != 2 || items[0].ID != rollback.Value.ID {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	replay, replayOperation, err := store.CreatePlatformRollback(ctx, admin.ID, "rollback", "rollback-fingerprint", "request-rollback", domain.CreatePlatformRollback{SourceUpgradeID: source.ID, HelmRevision: 3})
	if err != nil || !replay.Replay || replayOperation.ID != operation.ID {
		t.Fatalf("replay=%#v operation=%#v err=%v", replay, replayOperation, err)
	}
}
