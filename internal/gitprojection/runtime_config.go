package gitprojection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

const (
	ProjectionEnabledEnv       = "KUBERPLOY_GIT_PROJECTION_ENABLED"
	ProjectionCacheMaxBytesEnv = "KUBERPLOY_GIT_PROJECTION_CACHE_MAX_BYTES"
	ProjectionPollSecondsEnv   = "KUBERPLOY_GIT_PROJECTION_POLL_INTERVAL_SECONDS"
	ProjectionWebhookWakeEnv   = "KUBERPLOY_GIT_PROJECTION_WEBHOOK_WAKE_ENABLED"
	ProjectionGitAuthModeEnv   = "KUBERPLOY_GIT_PROJECTION_AUTH_MODE"
	ProjectionGitHubAppIDEnv   = "KUBERPLOY_GITHUB_APP_ID"
	ProjectionGitHubClientEnv  = "KUBERPLOY_GITHUB_APP_CLIENT_ID"
	ProjectionChartDigestEnv   = "KUBERPLOY_GIT_PROJECTION_CHART_DIGEST"
	ProjectionPolicyVersionEnv = "KUBERPLOY_GIT_PROJECTION_POLICY_VERSION"

	ProjectionCacheRoot       = "/var/lib/kuberploy/git-projection"
	minimumProjectionCache    = int64(64 << 20)
	maximumProjectionCache    = int64(2 << 30)
	minimumProjectionPoll     = 15 * time.Second
	maximumProjectionPoll     = 24 * time.Hour
	defaultProjectionMaxFiles = 100_000
	productionIndexDocuments  = 1_000
)

// RuntimeConfig contains only operator-controlled, non-secret projection
// settings and fixed projected-secret references. The feature is default-off;
// no private key or Git credential is read while parsing it.
type RuntimeConfig struct {
	Enabled       bool
	CacheMaxBytes int64
	PollInterval  time.Duration
	WebhookWake   bool
	ChartDigest   string
	PolicyVersion string
	GitHub        githubapp.Config
}

func RuntimeConfigFromEnvironment() (RuntimeConfig, error) {
	return RuntimeConfigFromLookup(os.LookupEnv)
}

func RuntimeConfigFromLookup(lookup func(string) (string, bool)) (RuntimeConfig, error) {
	if lookup == nil {
		return RuntimeConfig{}, ErrInvalid
	}
	enabled, present := lookup(ProjectionEnabledEnv)
	if !present || enabled == "" || enabled == "false" {
		return RuntimeConfig{}, nil
	}
	if enabled != "true" {
		return RuntimeConfig{}, errors.New("KUBERPLOY_GIT_PROJECTION_ENABLED must be exactly true or false")
	}

	cache, err := canonicalProjectionInteger(lookup, ProjectionCacheMaxBytesEnv, minimumProjectionCache, maximumProjectionCache)
	if err != nil {
		return RuntimeConfig{}, err
	}
	pollSeconds, err := canonicalProjectionInteger(lookup, ProjectionPollSecondsEnv, int64(minimumProjectionPoll/time.Second), int64(maximumProjectionPoll/time.Second))
	if err != nil {
		return RuntimeConfig{}, err
	}
	webhookWakeValue := exactProjectionValue(lookup, ProjectionWebhookWakeEnv)
	if webhookWakeValue != "true" && webhookWakeValue != "false" {
		return RuntimeConfig{}, errors.New(ProjectionWebhookWakeEnv + " must be exactly true or false")
	}
	appID, err := canonicalProjectionInteger(lookup, ProjectionGitHubAppIDEnv, 1, int64(^uint64(0)>>1))
	if err != nil {
		return RuntimeConfig{}, err
	}
	clientID := exactProjectionValue(lookup, ProjectionGitHubClientEnv)
	if clientID == "" {
		return RuntimeConfig{}, errors.New("enabled Git projection requires a GitHub App client id")
	}
	authMode := exactProjectionValue(lookup, ProjectionGitAuthModeEnv)
	if authMode != "github-app" {
		return RuntimeConfig{}, errors.New(ProjectionGitAuthModeEnv + " must be exactly github-app")
	}
	provider, err := githubapp.NewProjectedConfig(appID, clientID, githubapp.Permissions{
		"metadata":       githubapp.PermissionRead,
		"contents":       githubapp.PermissionWrite,
		"pull_requests":  githubapp.PermissionWrite,
		"administration": githubapp.PermissionRead,
	})
	if err != nil {
		return RuntimeConfig{}, err
	}
	chartDigest := exactProjectionValue(lookup, ProjectionChartDigestEnv)
	policyVersion := exactProjectionValue(lookup, ProjectionPolicyVersionEnv)
	if !digestRE.MatchString(chartDigest) {
		return RuntimeConfig{}, errors.New(ProjectionChartDigestEnv + " must be an exact sha256 digest")
	}
	if policyVersion == "" || len(policyVersion) > 128 || strings.ContainsAny(policyVersion, "\x00\r\n") {
		return RuntimeConfig{}, errors.New(ProjectionPolicyVersionEnv + " must be a bounded exact value")
	}
	return RuntimeConfig{
		Enabled: true, CacheMaxBytes: cache, PollInterval: time.Duration(pollSeconds) * time.Second, WebhookWake: webhookWakeValue == "true", ChartDigest: chartDigest, PolicyVersion: policyVersion, GitHub: provider,
	}, nil
}

func canonicalProjectionInteger(lookup func(string) (string, bool), name string, minimum, maximum int64) (int64, error) {
	value := exactProjectionValue(lookup, name)
	if value == "" {
		return 0, errors.New(name + " is required when Git projection is enabled")
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < minimum || parsed > maximum || strconv.FormatInt(parsed, 10) != value {
		return 0, errors.New(name + " must be a canonical bounded positive integer")
	}
	return parsed, nil
}

func exactProjectionValue(lookup func(string) (string, bool), name string) string {
	value, found := lookup(name)
	if !found || value == "" || strings.TrimSpace(value) != value {
		return ""
	}
	return value
}

func (c RuntimeConfig) MirrorManager() *MirrorManager {
	return &MirrorManager{
		Root: ProjectionCacheRoot, Timeout: defaultGitTimeout, MaxBytes: c.CacheMaxBytes,
		MaxFiles: defaultProjectionMaxFiles,
	}
}

func (c RuntimeConfig) Indexer(store Store) Indexer {
	return Indexer{Store: store, MaxDocuments: productionIndexDocuments, MaxBytes: defaultMaxIndexBytes}
}

func (c RuntimeConfig) Validate() error {
	if !c.Enabled {
		if c.CacheMaxBytes != 0 || c.PollInterval != 0 || c.WebhookWake || c.ChartDigest != "" || c.PolicyVersion != "" || c.GitHub.AppID != 0 {
			return ErrInvalid
		}
		return nil
	}
	if c.CacheMaxBytes < minimumProjectionCache || c.CacheMaxBytes > maximumProjectionCache || c.PollInterval < minimumProjectionPoll || c.PollInterval > maximumProjectionPoll ||
		!digestRE.MatchString(c.ChartDigest) || c.PolicyVersion == "" || len(c.PolicyVersion) > 128 || strings.ContainsAny(c.PolicyVersion, "\x00\r\n") || c.GitHub.Validate() != nil {
		return ErrInvalid
	}
	return nil
}

func (c RuntimeConfig) RuntimeDigest() (string, error) {
	if c.Validate() != nil || !c.Enabled {
		return "", ErrInvalid
	}
	canonical := struct {
		Enabled       bool   `json:"enabled"`
		CacheMaxBytes int64  `json:"cacheMaxBytes"`
		PollNanos     int64  `json:"pollNanos"`
		WebhookWake   bool   `json:"webhookWake"`
		ChartDigest   string `json:"chartDigest"`
		PolicyVersion string `json:"policyVersion"`
		GitHubAppID   int64  `json:"githubAppId"`
		GitHubClient  string `json:"githubClientId"`
		AuthMode      string `json:"authMode"`
		Publication   string `json:"publicationWorkflow"`
	}{true, c.CacheMaxBytes, int64(c.PollInterval), c.WebhookWake, c.ChartDigest, c.PolicyVersion, c.GitHub.AppID, c.GitHub.ClientID, "github-app", "github-pull-request.v1"}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", ErrInvalid
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
