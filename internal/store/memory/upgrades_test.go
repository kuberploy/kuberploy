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
