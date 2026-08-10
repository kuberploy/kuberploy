package githubapp

import "context"

const (
	projectedRuntimeSecret = "runtime"
	projectedPrivateKey    = "private-key.pem"
	projectedWebhookKey    = "webhook-secret"
	projectedStateKey      = "state-signing-secret"
)

// NewProjectedConfig binds provider identity and an explicit permission cap to
// the fixed projected Secret layout shared by trusted control-plane processes.
// It stores references only and does not read any credential bytes.
func NewProjectedConfig(appID int64, clientID string, maximum Permissions) (Config, error) {
	config := DefaultConfig()
	config.AppID = appID
	config.ClientID = clientID
	config.PrivateKeySecret = SecretRef{Name: projectedRuntimeSecret, Key: projectedPrivateKey}
	config.WebhookSecret = SecretRef{Name: projectedRuntimeSecret, Key: projectedWebhookKey}
	config.StateSigningSecret = SecretRef{Name: projectedRuntimeSecret, Key: projectedStateKey}
	config.MaximumTokenPermissions = clonePermissions(maximum)
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// NewProjectedClient builds the production provider from the fixed projected
// Secret reader. Construction never reads or caches key bytes; JWTSigner reads
// and erases the selected private-key file for each App JWT.
func NewProjectedClient(config Config) (*Client, error) {
	secrets := NewProjectedSecretReader()
	signer, err := NewJWTSigner(config, secrets, nil)
	if err != nil {
		return nil, err
	}
	return NewClient(config, signer, nil, nil)
}

// ProbeProjectedWorkerRuntime proves that the worker's only projected GitHub
// credential is present and is a usable App private key. The resulting local
// JWT is discarded; this performs no provider or other network request.
func ProbeProjectedWorkerRuntime(ctx context.Context, config Config) error {
	return probeProjectedAppKey(ctx, config, NewProjectedSecretReader())
}

func probeProjectedAppKey(ctx context.Context, config Config, reader SecretReader) error {
	if reader == nil {
		return ErrInvalidConfig
	}
	signer, err := NewJWTSigner(config, reader, nil)
	if err != nil {
		return err
	}
	_, err = signer.AppToken(ctx)
	return err
}

// ProbeProjectedRuntime validates every API-side projected credential without
// retaining it. It signs a local App JWT to prove the private key is usable;
// no outbound provider request is made.
func ProbeProjectedRuntime(ctx context.Context, config Config) error {
	reader := NewProjectedSecretReader()
	if err := probeProjectedAppKey(ctx, config, reader); err != nil {
		return err
	}
	for _, requirement := range []struct {
		ref   SecretRef
		min   int
		ascii bool
	}{
		{config.WebhookSecret, 16, false},
		{config.StateSigningSecret, 32, false},
		{SecretRef{Name: projectedRuntimeSecret, Key: projectedOAuthClientSecretKey}, 16, true},
	} {
		value, readErr := reader.ReadSecret(ctx, requirement.ref)
		if readErr != nil || len(value) < requirement.min || len(value) > 4096 || requirement.ascii && !validASCIISecret(value) {
			zeroBytes(value)
			return ErrSecretUnavailable
		}
		zeroBytes(value)
	}
	return nil
}
