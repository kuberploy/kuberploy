package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// DeploymentConfig is the server-authoritative AppConfig projection. RawYAML
// is also copied into an immutable operation input before any Git work starts.
type DeploymentConfig struct {
	DeploymentID string
	RawYAML      []byte
	ETag         string
	Version      int64
	UpdatedAt    time.Time
}

type ConfigPreviewLease struct {
	ID            string
	DeploymentID  string
	ActorID       string
	TokenHash     []byte
	BaseETag      string
	CandidateHash []byte
	ExpiresAt     time.Time
	ConsumedAt    *time.Time
	CreatedAt     time.Time
}

type CreateConfigPreview struct {
	DeploymentID  string
	BaseETag      string
	TokenHash     []byte
	CandidateHash []byte
	CandidateRaw  []byte
	Runtime       WorkloadRuntime
	ExpiresAt     time.Time
}

type SaveDeploymentConfig struct {
	DeploymentID  string
	BaseETag      string
	TokenHash     []byte
	CandidateHash []byte
	RawYAML       []byte
	Runtime       WorkloadRuntime
}

func DeploymentConfigETag(deploymentID string, version int64, raw []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("kuberploy-appconfig-v1\x00"))
	_, _ = hash.Write([]byte(deploymentID))
	_, _ = hash.Write([]byte{'\x00'})
	_, _ = hash.Write([]byte(strconv.FormatInt(version, 10)))
	_, _ = hash.Write([]byte{'\x00'})
	_, _ = hash.Write(raw)
	return `"cfg-sha256-` + hex.EncodeToString(hash.Sum(nil)) + `"`
}
