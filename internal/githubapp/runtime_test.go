package githubapp

import (
	"context"
	"errors"
	"testing"
)

func TestNewProjectedClientValidatesBeforeReadingSecrets(t *testing.T) {
	invalid := DefaultConfig()
	if _, err := NewProjectedClient(invalid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid production config accepted: %v", err)
	}

	valid := validTestConfig(t)
	client, err := NewProjectedClient(valid)
	if err != nil || client == nil {
		t.Fatalf("projected provider construction failed before first credential use: %v", err)
	}
}

func TestNewProjectedConfigFixesSecretRefsAndClonesPermissions(t *testing.T) {
	permissions := Permissions{"metadata": PermissionRead, "contents": PermissionRead}
	config, err := NewProjectedConfig(12345, "Iv1_kuberploy_test", permissions)
	if err != nil {
		t.Fatal(err)
	}
	permissions["contents"] = PermissionWrite
	if config.PrivateKeySecret != (SecretRef{Name: "runtime", Key: "private-key.pem"}) ||
		config.WebhookSecret != (SecretRef{Name: "runtime", Key: "webhook-secret"}) ||
		config.StateSigningSecret != (SecretRef{Name: "runtime", Key: "state-signing-secret"}) ||
		config.MaximumTokenPermissions["contents"] != PermissionRead {
		t.Fatalf("config=%#v", config)
	}
}

func TestProjectedWorkerProbeRequiresUsablePrivateKey(t *testing.T) {
	config := validTestConfig(t)
	secrets := testSecrets(t, config)
	if err := probeProjectedAppKey(context.Background(), config, secrets); err != nil {
		t.Fatalf("valid projected App key was rejected: %v", err)
	}

	secrets.values[config.PrivateKeySecret] = []byte("not a private key")
	if err := probeProjectedAppKey(context.Background(), config, secrets); err == nil {
		t.Fatal("invalid projected App key was accepted")
	}
}
