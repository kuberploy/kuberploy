package gitssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testSSHPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	publicRaw, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := ssh.NewPublicKey(publicRaw)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey
}

func TestStablePublicFingerprint(t *testing.T) {
	publicKey := testSSHPublicKey(t)
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey)))
	first, err := NewHostKeyPin("git.example.com:22", authorized)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewHostKeyPin("git.example.com:22", authorized+" comment")
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint != second.Fingerprint || first.Fingerprint != ssh.FingerprintSHA256(publicKey) {
		t.Fatalf("fingerprints differ: %q %q", first.Fingerprint, second.Fingerprint)
	}
}

func TestStrictHostKeyValidation(t *testing.T) {
	pinnedKey := testSSHPublicKey(t)
	changedKey := testSSHPublicKey(t)
	pin, err := NewHostKeyPin("git.example.com:22", string(ssh.MarshalAuthorizedKey(pinnedKey)))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewStrictHostKeyVerifier([]HostKeyPin{pin})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify("git.example.com:22", pinnedKey); err != nil {
		t.Fatalf("pinned key rejected: %v", err)
	}
	if err := verifier.Verify("unconfigured.example.com:22", pinnedKey); !errors.Is(err, ErrHostKeyNotPinned) {
		t.Fatalf("missing pin error = %v", err)
	}
	if err := verifier.Verify("git.example.com:22", changedKey); !errors.Is(err, ErrHostKeyChanged) {
		t.Fatalf("changed key error = %v", err)
	}
	empty, err := NewStrictHostKeyVerifier(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.Verify("git.example.com:22", pinnedKey); !errors.Is(err, ErrHostKeyNotPinned) {
		t.Fatalf("empty verifier error = %v", err)
	}
}

func TestHostKeyPinRejectsInvalidAndConflictingPins(t *testing.T) {
	if _, err := NewHostKeyPin("", "bad"); !errors.Is(err, ErrInvalidHostKeyPin) {
		t.Fatalf("invalid pin error = %v", err)
	}
	firstKey := testSSHPublicKey(t)
	secondKey := testSSHPublicKey(t)
	first, err := NewHostKeyPin("git.example.com:22", string(ssh.MarshalAuthorizedKey(firstKey)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewHostKeyPin("git.example.com:22", string(ssh.MarshalAuthorizedKey(secondKey)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewStrictHostKeyVerifier([]HostKeyPin{first, second}); !errors.Is(err, ErrInvalidHostKeyPin) {
		t.Fatalf("conflicting pins error = %v", err)
	}
}

func TestKnownHostsRendersExactPinnedEndpoints(t *testing.T) {
	key := testSSHPublicKey(t)
	pin, err := NewHostKeyPin("git.example.com:2222", string(ssh.MarshalAuthorizedKey(key)))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := KnownHosts([]HostKeyPin{pin, pin})
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(rendered)), "\n"); len(lines) != 1 ||
		!strings.HasPrefix(lines[0], "[git.example.com]:2222 ") || !strings.Contains(lines[0], key.Type()) {
		t.Fatalf("known_hosts=%q", rendered)
	}
	if _, err = KnownHosts(nil); !errors.Is(err, ErrInvalidHostKeyPin) {
		t.Fatalf("empty pins error=%v", err)
	}
}

func TestKnownHostsUsesPlainHostnameForDefaultSSHPort(t *testing.T) {
	key := testSSHPublicKey(t)
	pin, err := NewHostKeyPin("git.example.com:22", string(ssh.MarshalAuthorizedKey(key)))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := KnownHosts([]HostKeyPin{pin})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(rendered), "git.example.com ") {
		t.Fatalf("default-port known_hosts=%q", rendered)
	}
}
