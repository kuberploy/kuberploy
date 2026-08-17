package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSecretFileRequiresPrivateSingleLineFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := readSecretFile(path, "test")
	if err != nil || value != "value" {
		t.Fatalf("value=%q err=%v", value, err)
	}

	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = readSecretFile(path, "test"); err == nil || !strings.Contains(err.Error(), "mode 0600") {
		t.Fatalf("world-readable file err=%v", err)
	}

	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = readSecretFile(path, "test"); err == nil || !strings.Contains(err.Error(), "one non-empty line") {
		t.Fatalf("multi-line file err=%v", err)
	}
}

func TestReadSecretFileRejectsRelativeAndSymlinkPaths(t *testing.T) {
	if _, err := readSecretFile("relative-secret", "test"); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("relative path err=%v", err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(link, "test"); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlink err=%v", err)
	}
}
