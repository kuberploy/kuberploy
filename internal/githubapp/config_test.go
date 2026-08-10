package githubapp

import (
	"errors"
	"testing"
	"time"
)

func TestConfigValidationIsStrict(t *testing.T) {
	valid := validTestConfig(t)
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	tests := map[string]func(*Config){
		"missing app id":        func(c *Config) { c.AppID = 0 },
		"malformed client id":   func(c *Config) { c.ClientID = "client id" },
		"tenant file path":      func(c *Config) { c.PrivateKeySecret.Key = "../../private.pem" },
		"secret namespace path": func(c *Config) { c.WebhookSecret.Name = "tenant/github-app" },
		"insecure API":          func(c *Config) { c.APIBaseURL = "http://api.github.com" },
		"API credentials":       func(c *Config) { c.APIBaseURL = "https://token@api.github.com" },
		"noncanonical path":     func(c *Config) { c.APIBaseURL = "https://github.example/api/v3/" },
		"stale API version":     func(c *Config) { c.APIVersion = "2022-11-28" },
		"header injection":      func(c *Config) { c.UserAgent = "kuberploy\r\nAuthorization: bad" },
		"unbounded timeout":     func(c *Config) { c.RequestTimeout = time.Minute },
		"unbounded response":    func(c *Config) { c.MaxResponseBytes = 17 << 20 },
		"GitHub payload limit":  func(c *Config) { c.MaxWebhookBytes = 25<<20 + 1 },
		"long replay":           func(c *Config) { c.WebhookReplayWindow = 8 * 24 * time.Hour },
		"long state":            func(c *Config) { c.StateTTL = 16 * time.Minute },
		"long handoff":          func(c *Config) { c.HandoffTTL = 11 * time.Minute },
		"long JWT":              func(c *Config) { c.JWTLifetime = 10*time.Minute + time.Second },
		"long JWT backdate":     func(c *Config) { c.JWTBackdate = 61 * time.Second },
		"implicit permissions":  func(c *Config) { c.MaximumTokenPermissions = nil },
		"metadata broadening":   func(c *Config) { c.MaximumTokenPermissions["metadata"] = PermissionWrite },
		"organization scope":    func(c *Config) { c.MaximumTokenPermissions["organization_administration"] = PermissionWrite },
		"invalid permission":    func(c *Config) { c.MaximumTokenPermissions["contents"] = "owner" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.MaximumTokenPermissions = clonePermissions(valid.MaximumTokenPermissions)
			mutate(&candidate)
			if err := candidate.Validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("expected invalid config, got %v", err)
			}
		})
	}
	write := valid
	write.MaximumTokenPermissions = clonePermissions(valid.MaximumTokenPermissions)
	write.MaximumTokenPermissions["contents"] = PermissionWrite
	if err := write.Validate(); err != nil {
		t.Fatalf("explicit contents-write cap required by the GitOps writer was rejected: %v", err)
	}
	pullRequestWrite := write
	pullRequestWrite.MaximumTokenPermissions = clonePermissions(write.MaximumTokenPermissions)
	pullRequestWrite.MaximumTokenPermissions["pull_requests"] = PermissionWrite
	if err := pullRequestWrite.Validate(); err != nil {
		t.Fatalf("exact pull-request-write cap required by protected publication was rejected: %v", err)
	}
	protectionRead := valid
	protectionRead.MaximumTokenPermissions = clonePermissions(valid.MaximumTokenPermissions)
	protectionRead.MaximumTokenPermissions["administration"] = PermissionRead
	if err := protectionRead.Validate(); err != nil {
		t.Fatalf("exact administration-read cap required by protection observation was rejected: %v", err)
	}
	protectionWrite := protectionRead
	protectionWrite.MaximumTokenPermissions = clonePermissions(protectionRead.MaximumTokenPermissions)
	protectionWrite.MaximumTokenPermissions["administration"] = PermissionWrite
	if err := protectionWrite.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("administration-write cap was accepted: %v", err)
	}
}
