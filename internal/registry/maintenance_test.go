package registry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReachabilityCheckpointFailsClosedOnIncompleteOrAmbiguousEvidence(t *testing.T) {
	now := time.Date(2026, 8, 9, 4, 0, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("a", 64)
	candidateSet, candidates, err := cleanupCandidateSetDigest([]string{digest})
	if err != nil {
		t.Fatal(err)
	}
	request := ReachabilityCheckpointRequest{
		TargetID: "registry-1", PlanID: "plan-1", PlanDigest: "sha256:" + strings.Repeat("b", 64),
		ExecutionKey: "sha256:" + strings.Repeat("c", 64), CandidateSetDigest: candidateSet,
		CandidateDigests: candidates, NotBefore: now,
	}
	base := RegistryReachabilityCheckpoint{
		TargetID: request.TargetID, PlanID: request.PlanID, PlanDigest: request.PlanDigest,
		ExecutionKey: request.ExecutionKey, CandidateSetDigest: request.CandidateSetDigest,
		Revision: "checkpoint-1", InventoryRevision: "inventory-1", AuthorityRevision: "authority-1",
		RegistryWide: true, InventoryComplete: true, AuthorityComplete: true, ReachabilityComplete: true,
		StartedAt: now, ObservedAt: now.Add(time.Second),
		Blobs: []RegistryBlobReachability{{Digest: digest, Present: true, Reachable: false}},
	}
	base.GraphDigest = ReachabilityCheckpointDigest(base)
	if err = validateReachabilityCheckpoint(base, request, now); err != nil {
		t.Fatalf("valid checkpoint: %v", err)
	}

	tests := map[string]func(*RegistryReachabilityCheckpoint){
		"not registry wide":       func(c *RegistryReachabilityCheckpoint) { c.RegistryWide = false },
		"inventory incomplete":    func(c *RegistryReachabilityCheckpoint) { c.InventoryComplete = false },
		"authority incomplete":    func(c *RegistryReachabilityCheckpoint) { c.AuthorityComplete = false },
		"reachability incomplete": func(c *RegistryReachabilityCheckpoint) { c.ReachabilityComplete = false },
		"stale start":             func(c *RegistryReachabilityCheckpoint) { c.StartedAt = now.Add(-time.Nanosecond) },
		"candidate omitted":       func(c *RegistryReachabilityCheckpoint) { c.Blobs = nil },
		"candidate reachable":     func(c *RegistryReachabilityCheckpoint) { c.Blobs[0].Reachable = true },
		"duplicate row": func(c *RegistryReachabilityCheckpoint) {
			c.Blobs = append(c.Blobs, c.Blobs[0])
		},
		"wrong execution": func(c *RegistryReachabilityCheckpoint) { c.ExecutionKey = "sha256:" + strings.Repeat("d", 64) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			checkpoint := base
			checkpoint.Blobs = append([]RegistryBlobReachability(nil), base.Blobs...)
			mutate(&checkpoint)
			checkpoint.GraphDigest = ReachabilityCheckpointDigest(checkpoint)
			if err := validateReachabilityCheckpoint(checkpoint, request, now); !errors.Is(err, ErrRegistryCheckpointIncomplete) {
				t.Fatalf("err = %v", err)
			}
		})
	}

	corrupted := base
	corrupted.Blobs = append([]RegistryBlobReachability(nil), base.Blobs...)
	corrupted.Blobs[0].Present = false
	if err := validateReachabilityCheckpoint(corrupted, request, now); !errors.Is(err, ErrRegistryCheckpointIncomplete) {
		t.Fatalf("corrupt graph digest err = %v", err)
	}
}

func TestCleanupCandidateIdentityIsDeterministicAndRejectsDuplicates(t *testing.T) {
	a := "sha256:" + strings.Repeat("a", 64)
	b := "sha256:" + strings.Repeat("b", 64)
	first, ordered, err := cleanupCandidateSetDigest([]string{b, a})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := cleanupCandidateSetDigest([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if first != second || ordered[0] != a || ordered[1] != b {
		t.Fatalf("first=%q second=%q ordered=%#v", first, second, ordered)
	}
	if _, _, err := cleanupCandidateSetDigest([]string{a, a}); !errors.Is(err, ErrRegistryMaintenanceInvalid) {
		t.Fatalf("duplicate err = %v", err)
	}
}

func TestGCSweepReceiptRequiresCurrentCheckpointUnlessReplay(t *testing.T) {
	now := time.Date(2026, 8, 9, 4, 0, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("c", 64)
	candidateSet, _, err := cleanupCandidateSetDigest([]string{digest})
	if err != nil {
		t.Fatal(err)
	}
	request := GCSweepRequest{
		TargetID: "registry-1", ExecutionKey: "sha256:" + strings.Repeat("a", 64), CandidateSetDigest: candidateSet,
		CandidateDigests: []string{digest},
		Checkpoint:       RegistryReachabilityCheckpoint{Revision: "checkpoint-current", ObservedAt: now, Blobs: []RegistryBlobReachability{{Digest: digest, Present: false}}},
	}
	result := GCSweepResult{
		TargetID: request.TargetID, ExecutionKey: request.ExecutionKey, CandidateSetDigest: request.CandidateSetDigest,
		CheckpointRevision: "checkpoint-old", ProviderSweepID: "sweep-1", Complete: true,
		StartedAt: now.Add(-time.Minute), CompletedAt: now.Add(-time.Minute + time.Second),
	}
	if err := validateGCSweepResult(result, request, now, now); !errors.Is(err, ErrRegistryGCSweepUnconfirmed) {
		t.Fatalf("non-replay stale receipt err = %v", err)
	}
	result.Replay = true
	if err := validateGCSweepResult(result, request, now, now); err != nil {
		t.Fatalf("durable replay receipt: %v", err)
	}
	request.Checkpoint.Blobs[0].Present = true
	if err := validateGCSweepResult(result, request, now, now); !errors.Is(err, ErrRegistryGCSweepUnconfirmed) {
		t.Fatalf("replay against reintroduced digest err = %v", err)
	}
}

func TestUnavailableMaintenanceAdapterIsExplicit(t *testing.T) {
	session, err := (UnavailableMaintenanceAdapter{}).Acquire(context.Background(), MaintenanceAcquireRequest{})
	if session != nil || !errors.Is(err, ErrRegistryMaintenanceUnavailable) {
		t.Fatalf("session=%v err=%v", session, err)
	}
	checkpoint, err := (UnavailableCheckpointProvider{}).Capture(context.Background(), ReachabilityCheckpointRequest{})
	if checkpoint.TargetID != "" || !errors.Is(err, ErrRegistryCheckpointIncomplete) {
		t.Fatalf("checkpoint=%+v err=%v", checkpoint, err)
	}
}
