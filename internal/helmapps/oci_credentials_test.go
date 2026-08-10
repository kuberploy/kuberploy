package helmapps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func projectedOCIProfile(mode string) OCIRegistryCredentialProfile {
	return OCIRegistryCredentialProfile{RegistryHost: "registry.example.com", AuthHost: "auth.example.com",
		Name: "private-main", Mode: mode, ProjectionDigest: digestBytes([]byte("private-main-projection"))}
}

func TestOCIRegistryCredentialFormattingIsAlwaysRedacted(t *testing.T) {
	credential := &OCIRegistryCredential{Username: []byte("registry-user"), Password: []byte("registry-password"),
		BearerToken: []byte("operator-bearer"), AuthHost: "auth.example.com"}
	for _, formatted := range []string{fmt.Sprint(credential), fmt.Sprintf("%#v", credential), credential.LogValue().String()} {
		if formatted != "<redacted OCI registry credential>" {
			t.Fatalf("credential formatting was not opaque: %q", formatted)
		}
	}
}

func writeProjectedOCICredential(t *testing.T, root, profile, key, value string) {
	t.Helper()
	directory := filepath.Join(root, profile)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, key), []byte(value), 0o440); err != nil {
		t.Fatal(err)
	}
}

func TestProjectedOCIRegistryCredentialProviderResolvesOnlyExactOperatorProfile(t *testing.T) {
	root := t.TempDir()
	profile := projectedOCIProfile(OCICredentialModeBasic)
	writeProjectedOCICredential(t, root, profile.Name, "username", "registry-user")
	writeProjectedOCICredential(t, root, profile.Name, "password", "registry-password")
	provider, err := newProjectedOCIRegistryCredentialProviderAt(root, []OCIRegistryCredentialProfile{profile}, true)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := provider.AcquireOCIRegistryCredential(t.Context(), profile.RegistryHost, "team/chart")
	if err != nil || credential == nil || string(credential.Username) != "registry-user" ||
		string(credential.Password) != "registry-password" || credential.AuthHost != profile.AuthHost ||
		len(credential.BearerToken) != 0 {
		t.Fatalf("credential=%#v err=%v", credential, err)
	}
	credential.Destroy()
	if credential.Username != nil || credential.Password != nil || credential.AuthHost != "" {
		t.Fatal("credential material was not destroyed")
	}
	public, err := provider.AcquireOCIRegistryCredential(t.Context(), "public.example.com", "team/chart")
	if err != nil || public != nil {
		t.Fatalf("public registry credential=%#v err=%v", public, err)
	}
	if err = provider.Probe(t.Context()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestProjectedOCIRegistryCredentialProviderReadsBoundedBearerAndRejectsUnsafeFiles(t *testing.T) {
	profile := projectedOCIProfile(OCICredentialModeBearer)
	root := t.TempDir()
	writeProjectedOCICredential(t, root, profile.Name, "token", "opaque-bearer-token")
	provider, err := newProjectedOCIRegistryCredentialProviderAt(root, []OCIRegistryCredentialProfile{profile}, true)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := provider.AcquireOCIRegistryCredential(t.Context(), profile.RegistryHost, "team/chart")
	if err != nil || credential == nil || string(credential.BearerToken) != "opaque-bearer-token" ||
		len(credential.Username) != 0 || len(credential.Password) != 0 {
		t.Fatalf("credential=%#v err=%v", credential, err)
	}
	credential.Destroy()

	unsafeRoot := t.TempDir()
	if err = os.Symlink(root, filepath.Join(unsafeRoot, "credentials")); err != nil {
		t.Fatal(err)
	}
	unsafe, err := newProjectedOCIRegistryCredentialProviderAt(filepath.Join(unsafeRoot, "credentials"),
		[]OCIRegistryCredentialProfile{profile}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = unsafe.AcquireOCIRegistryCredential(t.Context(), profile.RegistryHost, "team/chart"); !errors.Is(err, ErrOCICredentialUnavailable) {
		t.Fatalf("symlink root error=%v", err)
	}
}

func TestProjectedOCIRegistryCredentialProviderRejectsScopeAndProfileCollisions(t *testing.T) {
	profile := projectedOCIProfile(OCICredentialModeBasic)
	for _, profiles := range [][]OCIRegistryCredentialProfile{
		{{RegistryHost: "HTTPS://registry.example.com", AuthHost: profile.AuthHost, Name: profile.Name, Mode: profile.Mode}},
		{profile, profile},
		{profile, {RegistryHost: "other.example.com", AuthHost: "other-auth.example.com", Name: profile.Name, Mode: profile.Mode,
			ProjectionDigest: digestBytes([]byte("other-projection"))}},
	} {
		if _, err := newProjectedOCIRegistryCredentialProviderAt(t.TempDir(), profiles, true); err == nil {
			t.Fatalf("profiles=%#v unexpectedly accepted", profiles)
		}
	}
	provider, err := newProjectedOCIRegistryCredentialProviderAt(t.TempDir(), []OCIRegistryCredentialProfile{profile}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct{ host, repository string }{
		{profile.RegistryHost, "../escape"}, {"registry.example.com/path", "team/chart"}, {profile.RegistryHost, "team/chart:tag"},
	} {
		if _, err = provider.AcquireOCIRegistryCredential(context.Background(), request.host, request.repository); err == nil {
			t.Fatalf("unsafe request=%#v accepted", request)
		}
	}
}
