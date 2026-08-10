package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	ErrRegistryMaintenanceUnavailable = errors.New("managed registry maintenance adapter is unavailable")
	ErrRegistryMaintenanceInvalid     = errors.New("managed registry maintenance proof is invalid")
	ErrRegistryCheckpointIncomplete   = errors.New("managed registry reachability checkpoint is incomplete")
	ErrRegistryGCSweepUnconfirmed     = errors.New("managed registry garbage-collection sweep is unconfirmed")
)

type RegistryMaintenanceMode string

const (
	RegistryMaintenanceReadOnly RegistryMaintenanceMode = "read_only"
	RegistryMaintenanceStopped  RegistryMaintenanceMode = "stopped"
)

type MaintenanceAcquireRequest struct {
	TargetID     string
	PlanID       string
	ExecutionKey string
	Owner        string
}

type MaintenanceReady struct {
	TargetID     string
	LeaseID      string
	ExecutionKey string
	Mode         RegistryMaintenanceMode
	Exclusive    bool
	Tested       bool
	EnteredAt    time.Time
}

// RegistryMaintenanceAdapter is the narrow seam a future Kubernetes
// controller must implement. Acquire must be exclusive across the entire
// registry target, not merely one service or repository.
type RegistryMaintenanceAdapter interface {
	Acquire(context.Context, MaintenanceAcquireRequest) (RegistryMaintenanceSession, error)
}

// RegistryMaintenanceSession owns a single exclusive maintenance lease.
// GarbageCollect must be durable and idempotent by ExecutionKey: replaying the
// same key may return a stored receipt but must not invoke a second provider
// sweep. The key binds the plan and candidate set; a replay is accepted only
// when the fresh checkpoint explicitly proves every candidate is now absent.
type RegistryMaintenanceSession interface {
	Enter(context.Context) (MaintenanceReady, error)
	GarbageCollect(context.Context, GCSweepRequest) (GCSweepResult, error)
	Restore(context.Context) error
	Release(context.Context) error
}

// UnavailableMaintenanceAdapter is the honest default until a tested
// Kubernetes/operator implementation is wired. It deliberately does not fake
// a stop/read-only transition.
type UnavailableMaintenanceAdapter struct{}

func (UnavailableMaintenanceAdapter) Acquire(context.Context, MaintenanceAcquireRequest) (RegistryMaintenanceSession, error) {
	return nil, ErrRegistryMaintenanceUnavailable
}

type RegistryBlobReachability struct {
	Digest    string `json:"digest"`
	Present   bool   `json:"present"`
	Reachable bool   `json:"reachable"`
}

type ReachabilityCheckpointRequest struct {
	TargetID           string
	PlanID             string
	PlanDigest         string
	ExecutionKey       string
	CandidateSetDigest string
	CandidateDigests   []string
	NotBefore          time.Time
}

// RegistryReachabilityCheckpoint is a complete registry-wide observation
// taken only after the registry has entered maintenance. Candidate omission is
// not interpreted as absence: every requested digest must have an explicit
// reachability row.
type RegistryReachabilityCheckpoint struct {
	TargetID             string                     `json:"targetId"`
	PlanID               string                     `json:"planId"`
	PlanDigest           string                     `json:"planDigest"`
	ExecutionKey         string                     `json:"executionKey"`
	CandidateSetDigest   string                     `json:"candidateSetDigest"`
	Revision             string                     `json:"revision"`
	InventoryRevision    string                     `json:"inventoryRevision"`
	AuthorityRevision    string                     `json:"authorityRevision"`
	GraphDigest          string                     `json:"graphDigest"`
	RegistryWide         bool                       `json:"registryWide"`
	InventoryComplete    bool                       `json:"inventoryComplete"`
	AuthorityComplete    bool                       `json:"authorityComplete"`
	ReachabilityComplete bool                       `json:"reachabilityComplete"`
	StartedAt            time.Time                  `json:"startedAt"`
	ObservedAt           time.Time                  `json:"observedAt"`
	Blobs                []RegistryBlobReachability `json:"blobs"`
}

type RegistryCheckpointProvider interface {
	Capture(context.Context, ReachabilityCheckpointRequest) (RegistryReachabilityCheckpoint, error)
}

// UnavailableCheckpointProvider is the fail-closed default until a complete
// physical-registry and authority observer is wired.
type UnavailableCheckpointProvider struct{}

func (UnavailableCheckpointProvider) Capture(context.Context, ReachabilityCheckpointRequest) (RegistryReachabilityCheckpoint, error) {
	return RegistryReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
}

type GCSweepRequest struct {
	TargetID           string
	PlanID             string
	ExecutionKey       string
	CandidateSetDigest string
	CandidateDigests   []string
	Checkpoint         RegistryReachabilityCheckpoint
}

// GCSweepResult is a durable controller receipt for one provider-wide sweep.
// Replay=true means the provider returned the receipt for an already completed
// ExecutionKey without running the Distribution GC command again.
type GCSweepResult struct {
	TargetID           string
	ExecutionKey       string
	CandidateSetDigest string
	CheckpointRevision string
	ProviderSweepID    string
	Complete           bool
	Replay             bool
	StartedAt          time.Time
	CompletedAt        time.Time
}

func cleanupCandidateSetDigest(digests []string) (string, []string, error) {
	ordered := append([]string(nil), digests...)
	sort.Strings(ordered)
	for i, digest := range ordered {
		if !validDigest(digest) || i > 0 && digest == ordered[i-1] {
			return "", nil, ErrRegistryMaintenanceInvalid
		}
	}
	encoded, err := json.Marshal(ordered)
	if err != nil {
		return "", nil, ErrRegistryMaintenanceInvalid
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), ordered, nil
}

func cleanupExecutionKey(planID, planDigest, candidateSetDigest string) (string, error) {
	if !validSafeIdentity(planID) || !validDigest(planDigest) || !validDigest(candidateSetDigest) {
		return "", ErrRegistryMaintenanceInvalid
	}
	sum := sha256.Sum256([]byte(planID + "\n" + planDigest + "\n" + candidateSetDigest))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ReachabilityCheckpointDigest fingerprints the immutable graph evidence. A
// provider sets GraphDigest to this value after filling all other fields.
func ReachabilityCheckpointDigest(checkpoint RegistryReachabilityCheckpoint) string {
	type checkpointView struct {
		TargetID             string
		PlanID               string
		PlanDigest           string
		ExecutionKey         string
		CandidateSetDigest   string
		Revision             string
		InventoryRevision    string
		AuthorityRevision    string
		RegistryWide         bool
		InventoryComplete    bool
		AuthorityComplete    bool
		ReachabilityComplete bool
		StartedAt            time.Time
		ObservedAt           time.Time
		Blobs                []RegistryBlobReachability
	}
	blobs := append([]RegistryBlobReachability(nil), checkpoint.Blobs...)
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].Digest < blobs[j].Digest })
	encoded, _ := json.Marshal(checkpointView{
		TargetID:             checkpoint.TargetID,
		PlanID:               checkpoint.PlanID,
		PlanDigest:           checkpoint.PlanDigest,
		ExecutionKey:         checkpoint.ExecutionKey,
		CandidateSetDigest:   checkpoint.CandidateSetDigest,
		Revision:             checkpoint.Revision,
		InventoryRevision:    checkpoint.InventoryRevision,
		AuthorityRevision:    checkpoint.AuthorityRevision,
		RegistryWide:         checkpoint.RegistryWide,
		InventoryComplete:    checkpoint.InventoryComplete,
		AuthorityComplete:    checkpoint.AuthorityComplete,
		ReachabilityComplete: checkpoint.ReachabilityComplete,
		StartedAt:            checkpoint.StartedAt.UTC(),
		ObservedAt:           checkpoint.ObservedAt.UTC(),
		Blobs:                blobs,
	})
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateMaintenanceReady(ready MaintenanceReady, request MaintenanceAcquireRequest, notBefore, now time.Time) error {
	if ready.TargetID != request.TargetID || ready.ExecutionKey != request.ExecutionKey || !validSafeIdentity(ready.LeaseID) ||
		!ready.Exclusive || !ready.Tested || ready.EnteredAt.IsZero() || ready.EnteredAt.Before(notBefore) || ready.EnteredAt.After(now.Add(time.Minute)) ||
		(ready.Mode != RegistryMaintenanceReadOnly && ready.Mode != RegistryMaintenanceStopped) {
		return ErrRegistryMaintenanceInvalid
	}
	return nil
}

func validateReachabilityCheckpoint(checkpoint RegistryReachabilityCheckpoint, request ReachabilityCheckpointRequest, now time.Time) error {
	candidateSetDigest, _, candidateErr := cleanupCandidateSetDigest(request.CandidateDigests)
	if checkpoint.TargetID != request.TargetID || checkpoint.PlanID != request.PlanID || checkpoint.PlanDigest != request.PlanDigest ||
		checkpoint.ExecutionKey != request.ExecutionKey || checkpoint.CandidateSetDigest != request.CandidateSetDigest ||
		candidateErr != nil || candidateSetDigest != request.CandidateSetDigest ||
		!validSafeIdentity(checkpoint.Revision) || !validSafeIdentity(checkpoint.InventoryRevision) || !validSafeIdentity(checkpoint.AuthorityRevision) ||
		!checkpoint.RegistryWide || !checkpoint.InventoryComplete || !checkpoint.AuthorityComplete || !checkpoint.ReachabilityComplete ||
		checkpoint.StartedAt.IsZero() || checkpoint.ObservedAt.IsZero() || checkpoint.StartedAt.Before(request.NotBefore) || checkpoint.ObservedAt.Before(checkpoint.StartedAt) || checkpoint.ObservedAt.After(now.Add(time.Minute)) ||
		checkpoint.GraphDigest != ReachabilityCheckpointDigest(checkpoint) {
		return ErrRegistryCheckpointIncomplete
	}
	rows := make(map[string]RegistryBlobReachability, len(checkpoint.Blobs))
	for _, blob := range checkpoint.Blobs {
		if !validDigest(blob.Digest) || blob.Reachable && !blob.Present {
			return ErrRegistryCheckpointIncomplete
		}
		if _, exists := rows[blob.Digest]; exists {
			return ErrRegistryCheckpointIncomplete
		}
		rows[blob.Digest] = blob
	}
	for _, digest := range request.CandidateDigests {
		blob, exists := rows[digest]
		if !exists || blob.Reachable {
			return ErrRegistryCheckpointIncomplete
		}
	}
	return nil
}

func validateGCSweepResult(result GCSweepResult, request GCSweepRequest, checkpointObservedAt, now time.Time) error {
	candidateSetDigest, _, candidateErr := cleanupCandidateSetDigest(request.CandidateDigests)
	if result.TargetID != request.TargetID || result.ExecutionKey != request.ExecutionKey || result.CandidateSetDigest != request.CandidateSetDigest ||
		candidateErr != nil || candidateSetDigest != request.CandidateSetDigest ||
		!result.Complete || !validSafeIdentity(result.ProviderSweepID) || !validSafeIdentity(result.CheckpointRevision) ||
		result.StartedAt.IsZero() || result.CompletedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) || result.CompletedAt.After(now.Add(time.Minute)) {
		return ErrRegistryGCSweepUnconfirmed
	}
	if !result.Replay && (result.CheckpointRevision != request.Checkpoint.Revision || result.StartedAt.Before(checkpointObservedAt)) {
		return ErrRegistryGCSweepUnconfirmed
	}
	if result.Replay {
		rows := make(map[string]RegistryBlobReachability, len(request.Checkpoint.Blobs))
		for _, blob := range request.Checkpoint.Blobs {
			rows[blob.Digest] = blob
		}
		for _, digest := range request.CandidateDigests {
			blob, exists := rows[digest]
			if !exists || blob.Present || blob.Reachable {
				return ErrRegistryGCSweepUnconfirmed
			}
		}
	}
	return nil
}

func validSafeIdentity(value string) bool {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._:@/+", r) || r == '-') {
			return false
		}
	}
	return true
}

func maintenanceError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrRegistryMaintenanceUnavailable) || errors.Is(err, ErrRegistryMaintenanceInvalid) ||
		errors.Is(err, ErrRegistryCheckpointIncomplete) || errors.Is(err, ErrRegistryGCSweepUnconfirmed) {
		return err
	}
	return fmt.Errorf("managed registry maintenance %s failed", operation)
}
