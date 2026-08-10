package secrets

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectedFingerprintKeyProviderReadsPrivateRawProjection(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "fingerprint.key")
	first := bytes.Repeat([]byte{0x41}, 32)
	if err := os.WriteFile(path, first, 0o600); err != nil {
		t.Fatal(err)
	}
	provider, err := NewProjectedFingerprintKeyProvider(path, "key-v1")
	if err != nil {
		t.Fatal(err)
	}
	key, err := provider.ActiveKey(context.Background())
	if err != nil || key.ID != "key-v1" || !bytes.Equal(key.Bytes, first) {
		t.Fatalf("key=%#v err=%v", key, err)
	}
	key.Bytes[0] = 0
	second, err := provider.ActiveKey(context.Background())
	if err != nil || !bytes.Equal(second.Bytes, first) {
		t.Fatalf("provider did not return fresh caller-owned bytes: %#v err=%v", second, err)
	}
	clear(second.Bytes)

	replacement := bytes.Repeat([]byte{0x42}, 64)
	if err = os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	rotated, err := provider.ActiveKey(context.Background())
	if err != nil || !bytes.Equal(rotated.Bytes, replacement) {
		t.Fatalf("projected update was not observed: %#v err=%v", rotated, err)
	}
	clear(rotated.Bytes)
}

func TestProjectedFingerprintKeyProviderAllowsInVolumeAtomicSymlinks(t *testing.T) {
	directory := t.TempDir()
	version := filepath.Join(directory, "..2026_08_09")
	if err := os.Mkdir(version, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version, "key"), bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(version), filepath.Join(directory, "..data")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "fingerprint.key")
	if err := os.Symlink(filepath.Join("..data", "key"), path); err != nil {
		t.Fatal(err)
	}
	provider, err := NewProjectedFingerprintKeyProvider(path, "key-v1")
	if err != nil {
		t.Fatal(err)
	}
	key, err := provider.ActiveKey(context.Background())
	if err != nil || len(key.Bytes) != 32 {
		t.Fatalf("Kubernetes-style projection failed: %#v err=%v", key, err)
	}
	clear(key.Bytes)
}

func TestProjectedFingerprintKeyProviderAllowsExactProcessGroupRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "group-readable.key")
	if err := os.WriteFile(path, bytes.Repeat([]byte{9}, 32), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	provider, err := NewProjectedFingerprintKeyProvider(path, "key-v1")
	if err != nil {
		t.Fatal(err)
	}
	key, err := provider.ActiveKey(t.Context())
	if err != nil || len(key.Bytes) != 32 {
		t.Fatalf("group-readable Kubernetes projection key=%#v err=%v", key, err)
	}
	clear(key.Bytes)
}

func TestProjectedFingerprintKeyProviderFailsClosed(t *testing.T) {
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.key")
	if err := os.WriteFile(outside, bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(directory, "escape.key")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(directory, "short.key")
	if err := os.WriteFile(short, []byte("guessable"), 0o600); err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(directory, "public.key")
	if err := os.WriteFile(public, bytes.Repeat([]byte{2}, 32), 0o644); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(directory, "oversized.key")
	if err := os.WriteFile(oversized, bytes.Repeat([]byte{3}, 129), 0o600); err != nil {
		t.Fatal(err)
	}
	groupWritable := filepath.Join(directory, "group-writable.key")
	if err := os.WriteFile(groupWritable, bytes.Repeat([]byte{4}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(groupWritable, 0o620); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{escape, short, public, oversized, groupWritable, filepath.Join(directory, "missing.key")} {
		provider, err := NewProjectedFingerprintKeyProvider(path, "key-v1")
		if err != nil {
			t.Fatalf("constructor %q: %v", path, err)
		}
		if _, err = provider.ActiveKey(context.Background()); !errors.Is(err, ErrFingerprintKeyUnavailable) {
			t.Fatalf("path %q err=%v", path, err)
		}
	}
	for _, input := range []struct{ path, id string }{{"relative", "key-v1"}, {directory + "/../" + filepath.Base(directory), "key-v1"}, {filepath.Join(directory, "ok"), "bad id"}} {
		if _, err := NewProjectedFingerprintKeyProvider(input.path, input.id); !errors.Is(err, ErrInvalid) {
			t.Fatalf("constructor path=%q id=%q err=%v", input.path, input.id, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider, err := NewProjectedFingerprintKeyProvider(public, "key-v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.ActiveKey(ctx); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrFingerprintKeyUnavailable) {
		t.Fatalf("canceled context err=%v", err)
	}
}
