package autodeploy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"time"
)

const RuntimeEnabledEnv = "KUBERPLOY_AUTO_DEPLOY_ENABLED"

const (
	RuntimeClaimPollInterval = time.Second
	RuntimeRunLease          = 30 * time.Second
	RuntimeRetryBackoff      = time.Minute
)

type RuntimeConfig struct {
	Enabled  bool
	Identity RuntimeIdentity
}

// RuntimeAuthorities is derived from the independently validated source-build,
// Git projection, and protected Argo runtime identities. None of these fields
// is supplied by the auto-deploy feature flag.
type RuntimeAuthorities struct {
	SourceBuildConfigDigest     string
	SourceBuildGitHubAppID      int64
	GitProjectionConfigDigest   string
	GitProjectionGitHubAppID    int64
	FoundationConfigDigest      string
	FoundationPollNanos         int64
	FoundationPlatformBindingID string
	ArgoConfigDigest            string
	ArgoGitHubAppID             int64
	ArgoPlatformBindingID       string
}

func RuntimeConfigFromEnvironment() (RuntimeConfig, error) {
	return runtimeConfigFromLookup(os.LookupEnv)
}

func runtimeConfigFromLookup(lookup func(string) (string, bool)) (RuntimeConfig, error) {
	if lookup == nil {
		return RuntimeConfig{}, ErrInvalid
	}
	rawEnabled, present := lookup(RuntimeEnabledEnv)
	if !present || rawEnabled == "" || rawEnabled == "false" {
		return RuntimeConfig{}, nil
	}
	if rawEnabled != "true" {
		return RuntimeConfig{}, ErrInvalid
	}
	return RuntimeConfig{Enabled: true}, nil
}

func RuntimeIdentityForAuthorities(authorities RuntimeAuthorities) (RuntimeIdentity, error) {
	if !digestRE.MatchString(authorities.SourceBuildConfigDigest) || !digestRE.MatchString(authorities.GitProjectionConfigDigest) ||
		!digestRE.MatchString(authorities.FoundationConfigDigest) || authorities.FoundationPollNanos < int64(time.Second) || authorities.FoundationPollNanos > int64(time.Minute) ||
		!digestRE.MatchString(authorities.ArgoConfigDigest) || authorities.SourceBuildGitHubAppID < 1 ||
		authorities.SourceBuildGitHubAppID != authorities.GitProjectionGitHubAppID ||
		authorities.SourceBuildGitHubAppID != authorities.ArgoGitHubAppID || !uuidRE.MatchString(authorities.ArgoPlatformBindingID) ||
		authorities.FoundationPlatformBindingID != authorities.ArgoPlatformBindingID {
		return RuntimeIdentity{}, ErrInvalid
	}
	contract := struct {
		Version                  string             `json:"version"`
		Claim                    string             `json:"claim"`
		ReleaseProjection        string             `json:"releaseProjection"`
		ClaimPollNanos           int64              `json:"claimPollNanos"`
		RunLeaseNanos            int64              `json:"runLeaseNanos"`
		SubmissionHeartbeatNanos int64              `json:"submissionHeartbeatNanos"`
		RetryBackoffNanos        int64              `json:"retryBackoffNanos"`
		ReadinessHeartbeatNanos  int64              `json:"readinessHeartbeatNanos"`
		ReadinessLeaseNanos      int64              `json:"readinessLeaseNanos"`
		ReadinessMaximumAgeNanos int64              `json:"readinessMaximumAgeNanos"`
		Authorities              RuntimeAuthorities `json:"authorities"`
	}{RuntimeContractVersion, "auto_deploy_runs.fenced.v1", "verified-build-release-projection.v1",
		int64(RuntimeClaimPollInterval), int64(RuntimeRunLease), int64(RuntimeRunLease / 3), int64(RuntimeRetryBackoff),
		int64(RuntimeHeartbeatPeriod), int64(RuntimeReadinessLease), int64(RuntimeReadinessMaxAge), authorities}
	encoded, err := json.Marshal(contract)
	if err != nil {
		return RuntimeIdentity{}, ErrInvalid
	}
	sum := sha256.Sum256(encoded)
	identity := RuntimeIdentity{ContractVersion: RuntimeContractVersion, OperatorConfigDigest: "sha256:" + hex.EncodeToString(sum[:])}
	if identity.Validate() != nil {
		return RuntimeIdentity{}, ErrInvalid
	}
	return identity, nil
}
