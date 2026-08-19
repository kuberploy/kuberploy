package releases

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func releaseCacheSnapshot(t *testing.T) Snapshot {
	t.Helper()
	raw, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	manifest, err := ParseExactManifest(raw, digest)
	if err != nil {
		t.Fatal(err)
	}
	checked := time.Now().UTC().Truncate(time.Second)
	return Snapshot{Release: domain.ReleaseInfo{Tag: manifest.Release.Tag, Version: manifest.Release.Version, ManifestDigest: digest,
		Manifest: manifest, ManifestBytes: raw, PublishedAt: time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)},
		UpstreamETag: `"github-etag"`, LastCheckedAt: checked}
}

func TestReleaseCacheRoundTripRetainsExactManifestBytes(t *testing.T) {
	snapshot := releaseCacheSnapshot(t)
	encoded, err := encodeReleaseCache(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeReleaseCache(encoded, snapshot.LastCheckedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Release.ManifestBytes, snapshot.Release.ManifestBytes) || decoded.Release.ManifestDigest != snapshot.Release.ManifestDigest ||
		decoded.Release.Version != snapshot.Release.Version || decoded.UpstreamETag != snapshot.UpstreamETag {
		t.Fatalf("decoded=%#v", decoded)
	}
}

func TestReleaseCacheRejectsTamperingExtensionsAndFutureData(t *testing.T) {
	snapshot := releaseCacheSnapshot(t)
	encoded, err := encodeReleaseCache(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err = json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["unexpected"] = true
	extended, _ := json.Marshal(envelope)
	if _, err = decodeReleaseCache(extended, snapshot.LastCheckedAt.Add(time.Second)); err == nil {
		t.Fatal("extended cache envelope was accepted")
	}

	var typed releaseCacheEnvelope
	if err = json.Unmarshal(encoded, &typed); err != nil {
		t.Fatal(err)
	}
	typed.ManifestBytes[0] ^= 1
	tampered, _ := json.Marshal(typed)
	if _, err = decodeReleaseCache(tampered, snapshot.LastCheckedAt.Add(time.Second)); err == nil {
		t.Fatal("tampered exact manifest bytes were accepted")
	}

	typed = releaseCacheEnvelope{}
	if err = json.Unmarshal(encoded, &typed); err != nil {
		t.Fatal(err)
	}
	typed.LastCheckedAt = snapshot.LastCheckedAt.Add(2 * time.Minute)
	future, _ := json.Marshal(typed)
	if _, err = decodeReleaseCache(future, snapshot.LastCheckedAt); err == nil {
		t.Fatal("future cache timestamp was accepted")
	}
	if _, err = decodeReleaseCache(append(encoded, []byte("{}")...), snapshot.LastCheckedAt.Add(time.Second)); err == nil {
		t.Fatal("trailing JSON document was accepted")
	}
}

func TestReleaseCacheSnapshotValidationAndOptions(t *testing.T) {
	snapshot := releaseCacheSnapshot(t)
	snapshot.Release.ManifestBytes = append([]byte(nil), snapshot.Release.ManifestBytes...)
	snapshot.Release.ManifestBytes[0] ^= 1
	if _, err := encodeReleaseCache(snapshot); err == nil {
		t.Fatal("tampered snapshot was encoded")
	}
	if _, err := NewValkeyCache(ValkeyCacheOptions{}); err == nil {
		t.Fatal("empty Valkey addresses were accepted")
	}
	if _, err := NewValkeyCache(ValkeyCacheOptions{Addresses: []string{"bad\naddress"}}); err == nil {
		t.Fatal("invalid Valkey address was accepted")
	}
}

func TestReleaseCacheConstructionHasBoundedUnresponsiveServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				select {
				case <-stop:
					_ = connection.Close()
				case <-time.After(5 * time.Second):
					_ = connection.Close()
				}
			}()
		}
	}()

	started := time.Now()
	if _, err = NewValkeyCache(ValkeyCacheOptions{Addresses: []string{listener.Addr().String()}}); err == nil {
		t.Fatal("unresponsive Valkey server was accepted")
	}
	if elapsed := time.Since(started); elapsed > 4*releaseCacheIOTimeout {
		t.Fatalf("release cache construction exceeded fallback budget: %s", elapsed)
	}
}

func TestValkeyReleaseCacheIntegration(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("KUBERPLOY_TEST_VALKEY_ADDRESS"))
	if address == "" {
		t.Skip("KUBERPLOY_TEST_VALKEY_ADDRESS is not set")
	}
	cache, err := NewValkeyCache(ValkeyCacheOptions{Addresses: []string{address}})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx := context.Background()
	t.Cleanup(func() { _ = cache.client.Do(ctx, cache.client.B().Del().Key(releaseCacheKey).Build()).Error() })
	if err = cache.Store(ctx, releaseCacheSnapshot(t), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := cache.Load(ctx)
	if err != nil || !ok || len(loaded.Release.ManifestBytes) == 0 {
		t.Fatalf("loaded=%#v ok=%v err=%v", loaded, ok, err)
	}
}
