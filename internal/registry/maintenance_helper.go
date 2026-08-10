package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	RegistryMaintenanceHelperModeEnv    = "KUBERPLOY_REGISTRY_MAINTENANCE_HELPER_MODE"
	RegistryMaintenanceHelperRequestEnv = "KUBERPLOY_REGISTRY_MAINTENANCE_HELPER_REQUEST"
	registryMaintenanceTerminationPath  = "/dev/termination-log"
	maximumMaintenanceCandidates        = 16
	maximumMaintenanceResultBytes       = 4096
	maximumRegistryStorageEntries       = 1_000_000
)

type maintenanceHelperRequest struct {
	Version            int       `json:"version"`
	Mode               string    `json:"mode"`
	TargetID           string    `json:"targetId"`
	PlanID             string    `json:"planId"`
	PlanDigest         string    `json:"planDigest"`
	ExecutionKey       string    `json:"executionKey"`
	CandidateSetDigest string    `json:"candidateSetDigest"`
	CandidateDigests   []string  `json:"candidateDigests"`
	CheckpointRevision string    `json:"checkpointRevision,omitempty"`
	NotBefore          time.Time `json:"notBefore"`
}

type physicalReachabilityCheckpoint struct {
	TargetID             string                     `json:"targetId"`
	PlanID               string                     `json:"planId"`
	PlanDigest           string                     `json:"planDigest"`
	ExecutionKey         string                     `json:"executionKey"`
	CandidateSetDigest   string                     `json:"candidateSetDigest"`
	Revision             string                     `json:"revision"`
	InventoryRevision    string                     `json:"inventoryRevision"`
	RegistryWide         bool                       `json:"registryWide"`
	InventoryComplete    bool                       `json:"inventoryComplete"`
	ReachabilityComplete bool                       `json:"reachabilityComplete"`
	StartedAt            time.Time                  `json:"startedAt"`
	ObservedAt           time.Time                  `json:"observedAt"`
	Blobs                []RegistryBlobReachability `json:"blobs"`
}

type maintenanceHelperResult struct {
	Version    int                             `json:"version"`
	Mode       string                          `json:"mode"`
	Checkpoint *physicalReachabilityCheckpoint `json:"checkpoint,omitempty"`
	Sweep      *GCSweepResult                  `json:"sweep,omitempty"`
}

func (r maintenanceHelperRequest) validate(expectedMode string) error {
	if r.Version != 1 || r.Mode != expectedMode || !validSafeIdentity(r.TargetID) || !validSafeIdentity(r.PlanID) ||
		!validDigest(r.PlanDigest) || !validDigest(r.ExecutionKey) || !validDigest(r.CandidateSetDigest) ||
		r.NotBefore.IsZero() || len(r.CandidateDigests) < 1 || len(r.CandidateDigests) > maximumMaintenanceCandidates {
		return ErrRegistryMaintenanceInvalid
	}
	digest, ordered, err := cleanupCandidateSetDigest(r.CandidateDigests)
	if err != nil || digest != r.CandidateSetDigest {
		return ErrRegistryMaintenanceInvalid
	}
	for index := range ordered {
		if ordered[index] != r.CandidateDigests[index] {
			return ErrRegistryMaintenanceInvalid
		}
	}
	if expectedMode == "gc" && !validSafeIdentity(r.CheckpointRevision) || expectedMode == "checkpoint" && r.CheckpointRevision != "" {
		return ErrRegistryMaintenanceInvalid
	}
	return nil
}

// RunMaintenanceHelper is the only alternate worker-image entrypoint. It is
// intentionally filesystem-only: no ServiceAccount token, registry
// credential, network endpoint, arbitrary path, or command comes from input.
func RunMaintenanceHelper(ctx context.Context) error {
	mode := os.Getenv(RegistryMaintenanceHelperModeEnv)
	if mode != "checkpoint" && mode != "gc" {
		return ErrRegistryMaintenanceInvalid
	}
	raw := os.Getenv(RegistryMaintenanceHelperRequestEnv)
	if raw == "" || len(raw) > maximumMaintenanceResultBytes {
		return ErrRegistryMaintenanceInvalid
	}
	var request maintenanceHelperRequest
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return ErrRegistryMaintenanceInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || request.validate(mode) != nil {
		return ErrRegistryMaintenanceInvalid
	}
	result := maintenanceHelperResult{Version: 1, Mode: mode}
	switch mode {
	case "checkpoint":
		checkpoint, err := scanRegistryStorage(ctx, managedRegistryStorageRoot, request)
		if err != nil {
			return err
		}
		result.Checkpoint = &checkpoint
	case "gc":
		startedAt := time.Now().UTC()
		if startedAt.Before(request.NotBefore) {
			return ErrRegistryMaintenanceInvalid
		}
		command := exec.CommandContext(ctx, "/usr/local/bin/registry", "garbage-collect", managedRegistryConfigPath)
		command.Stdin, command.Stdout, command.Stderr = nil, io.Discard, io.Discard
		if err := command.Run(); err != nil {
			return ErrRegistryGCSweepUnconfirmed
		}
		completedAt := time.Now().UTC()
		providerID := "gc-" + strings.TrimPrefix(request.ExecutionKey, "sha256:")[:24]
		result.Sweep = &GCSweepResult{TargetID: request.TargetID, ExecutionKey: request.ExecutionKey,
			CandidateSetDigest: request.CandidateSetDigest, CheckpointRevision: request.CheckpointRevision,
			ProviderSweepID: providerID, Complete: true, StartedAt: startedAt, CompletedAt: completedAt}
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumMaintenanceResultBytes {
		return ErrRegistryMaintenanceInvalid
	}
	if err = os.WriteFile(registryMaintenanceTerminationPath, encoded, 0o600); err != nil {
		return errors.New("write bounded registry maintenance result")
	}
	return nil
}

func scanRegistryStorage(ctx context.Context, root string, request maintenanceHelperRequest) (physicalReachabilityCheckpoint, error) {
	if root != managedRegistryStorageRoot {
		return physicalReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
	}
	return scanRegistryStorageAt(ctx, root, request)
}

func scanRegistryStorageAt(ctx context.Context, root string, request maintenanceHelperRequest) (physicalReachabilityCheckpoint, error) {
	startedAt := time.Now().UTC()
	if startedAt.Before(request.NotBefore) || root == "" || !filepath.IsAbs(root) {
		return physicalReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
	}
	base := filepath.Join(root, "docker", "registry", "v2")
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return physicalReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
	}
	manifestRoots := make(map[string]struct{})
	presentBlobs := make(map[string]int64)
	repositories := make(map[string]struct{})
	graphFacts := make([]string, 0)
	entries := 0
	err = filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || ctx.Err() != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrRegistryCheckpointIncomplete
		}
		entries++
		if entries > maximumRegistryStorageEntries || entry.Type()&os.ModeSymlink != 0 {
			return ErrRegistryCheckpointIncomplete
		}
		relative, relErr := filepath.Rel(base, path)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return ErrRegistryCheckpointIncomplete
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			return ErrRegistryCheckpointIncomplete
		}
		if len(parts) >= 7 && parts[0] == "repositories" && parts[len(parts)-5] == "_manifests" &&
			parts[len(parts)-4] == "revisions" && parts[len(parts)-3] == "sha256" && parts[len(parts)-1] == "link" {
			hash := parts[len(parts)-2]
			repository := strings.Join(parts[1:len(parts)-5], "/")
			digest := "sha256:" + hash
			if !validRepository(repository) || !validDigest(digest) {
				return ErrRegistryCheckpointIncomplete
			}
			link, readErr := readStrictRegistryFile(path, 128)
			if readErr != nil || strings.TrimSpace(string(link)) != digest {
				zeroBytes(link)
				return ErrRegistryCheckpointIncomplete
			}
			zeroBytes(link)
			manifestRoots[digest] = struct{}{}
			repositories[repository] = struct{}{}
			graphFacts = append(graphFacts, "manifest\x00"+repository+"\x00"+digest)
			return nil
		}
		if len(parts) == 5 && parts[0] == "blobs" && parts[1] == "sha256" && parts[4] == "data" {
			hash := parts[3]
			digest := "sha256:" + hash
			if len(hash) != 64 || parts[2] != hash[:2] || !validDigest(digest) || info.Size() < 0 {
				return ErrRegistryCheckpointIncomplete
			}
			presentBlobs[digest] = info.Size()
			graphFacts = append(graphFacts, "blob\x00"+digest+"\x00"+fmt.Sprint(info.Size()))
		}
		return nil
	})
	if err != nil {
		return physicalReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
	}
	reachable := make(map[string]struct{}, len(manifestRoots))
	queue := make([]string, 0, len(manifestRoots))
	for digest := range manifestRoots {
		queue = append(queue, digest)
	}
	sort.Strings(queue)
	for len(queue) > 0 {
		digest := queue[0]
		queue = queue[1:]
		if _, seen := reachable[digest]; seen {
			continue
		}
		if _, exists := presentBlobs[digest]; !exists {
			return physicalReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
		}
		bodyPath, pathErr := registryBlobDataPath(base, digest)
		if pathErr != nil {
			return physicalReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
		}
		body, readErr := readStrictRegistryFile(bodyPath, maximumObservedManifestBody)
		if readErr != nil || verifyDistributionDigest(body, digest) != nil {
			zeroBytes(body)
			return physicalReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
		}
		var document distributionManifestDocument
		if decodeBoundedDistributionJSON(body, &document) != nil || document.SchemaVersion != 2 {
			zeroBytes(body)
			return physicalReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
		}
		zeroBytes(body)
		reachable[digest] = struct{}{}
		switch document.MediaType {
		case ociIndexMediaType, dockerIndexMediaType:
			for _, child := range document.Manifests {
				if !validManifestDescriptor(child, false) {
					return physicalReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
				}
				queue = append(queue, child.Digest)
				graphFacts = append(graphFacts, "edge\x00"+digest+"\x00"+child.Digest)
			}
		case ociManifestMediaType, dockerManifestMediaType:
			descriptors := append([]distributionDescriptor{document.Config}, document.Layers...)
			for _, descriptor := range descriptors {
				if !validBlobDescriptor(descriptor) {
					return physicalReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
				}
				if size, exists := presentBlobs[descriptor.Digest]; !exists || size != descriptor.Size {
					return physicalReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
				}
				reachable[descriptor.Digest] = struct{}{}
				graphFacts = append(graphFacts, "edge\x00"+digest+"\x00"+descriptor.Digest)
			}
		default:
			return physicalReachabilityCheckpoint{}, ErrRegistryCheckpointIncomplete
		}
		sort.Strings(queue)
		queue = compactStrings(queue)
	}
	sort.Strings(graphFacts)
	graphSum := sha256.Sum256([]byte(strings.Join(graphFacts, "\n")))
	repositoryNames := make([]string, 0, len(repositories))
	for repository := range repositories {
		repositoryNames = append(repositoryNames, repository)
	}
	sort.Strings(repositoryNames)
	inventorySum := sha256.Sum256([]byte(strings.Join(repositoryNames, "\n")))
	rows := make([]RegistryBlobReachability, 0, len(request.CandidateDigests))
	for _, digest := range request.CandidateDigests {
		_, present := presentBlobs[digest]
		_, isReachable := reachable[digest]
		rows = append(rows, RegistryBlobReachability{Digest: digest, Present: present, Reachable: isReachable})
	}
	observedAt := time.Now().UTC()
	return physicalReachabilityCheckpoint{
		TargetID: request.TargetID, PlanID: request.PlanID, PlanDigest: request.PlanDigest,
		ExecutionKey: request.ExecutionKey, CandidateSetDigest: request.CandidateSetDigest,
		Revision: "physical-" + hex.EncodeToString(graphSum[:12]), InventoryRevision: "inventory-" + hex.EncodeToString(inventorySum[:12]),
		RegistryWide: true, InventoryComplete: true, ReachabilityComplete: true,
		StartedAt: startedAt, ObservedAt: observedAt, Blobs: rows,
	}, nil
}

func registryBlobDataPath(base, digest string) (string, error) {
	if !validDigest(digest) {
		return "", ErrRegistryCheckpointIncomplete
	}
	hash := strings.TrimPrefix(digest, "sha256:")
	path := filepath.Join(base, "blobs", "sha256", hash[:2], hash, "data")
	relative, err := filepath.Rel(base, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", ErrRegistryCheckpointIncomplete
	}
	return path, nil
}

func readStrictRegistryFile(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > maximum {
		return nil, ErrRegistryCheckpointIncomplete
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrRegistryCheckpointIncomplete
	}
	value, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(value)) > maximum {
		zeroBytes(value)
		return nil, ErrRegistryCheckpointIncomplete
	}
	return value, nil
}
