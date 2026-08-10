package builder

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	MaxResultBytes            = 64 << 10
	MaxTerminationResultBytes = 4 << 10
)

type Warning string

const (
	WarningColdBuild     Warning = "ColdBuild"
	WarningCacheDegraded Warning = "CacheDegraded"
)

type BuildResult struct {
	APIVersion  string    `json:"apiVersion"`
	OperationID string    `json:"operationId"`
	Generation  int64     `json:"generation"`
	Status      string    `json:"status"`
	Image       Image     `json:"image"`
	Cache       *Cache    `json:"cache,omitempty"`
	Warnings    []Warning `json:"warnings"`
	StartedAt   time.Time `json:"startedAt"`
	CompletedAt time.Time `json:"completedAt"`
}

type Image struct {
	Reference string   `json:"reference"`
	Digest    string   `json:"digest"`
	Platforms []string `json:"platforms"`
}

// Cache identifies the immutable, generation-scoped cache reference that the
// trusted agent copied from its operation-scoped candidate. A nil Cache means
// promotion could not be confirmed and the candidate must never be imported.
type Cache struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

type CheckoutResult struct {
	APIVersion  string `json:"apiVersion"`
	OperationID string `json:"operationId"`
	Generation  int64  `json:"generation"`
	Status      string `json:"status"`
	Commit      string `json:"commit"`
}

func WriteResultAtomic(path string, result any) error {
	return writeResultAtomic(path, result, MaxResultBytes, false)
}

// WriteTerminationResultAtomic keeps the typed result within Kubernetes'
// per-container termination-message limit. The container runtime bind-mounts
// the pre-created termination file, so it cannot be replaced with rename(2).
// A single bounded write is synced instead; the adapter rejects partial or
// malformed JSON, so interruption can never be interpreted as success.
func WriteTerminationResultAtomic(path string, result any) error {
	return writeResultAtomic(path, result, MaxTerminationResultBytes, true)
}

func writeResultAtomic(path string, result any, maximum int, exclusive bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("result path must be clean and absolute")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if len(encoded) > maximum || exclusive && len(encoded) == maximum {
		return errors.New("result exceeds maximum size")
	}
	if exclusive {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("termination result must be a pre-created regular file")
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return fmt.Errorf("open termination result: %w", err)
		}
		if _, err = file.Write(encoded); err != nil {
			_ = file.Close()
			return fmt.Errorf("write termination result: %w", err)
		}
		if err = file.Sync(); err != nil {
			_ = file.Close()
			return fmt.Errorf("sync termination result: %w", err)
		}
		if err = file.Close(); err != nil {
			return fmt.Errorf("close termination result: %w", err)
		}
		return nil
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".result-*")
	if err != nil {
		return fmt.Errorf("create result: %w", err)
	}
	temporaryPath := temporary.Name()
	success := false
	defer func() {
		_ = temporary.Close()
		if !success {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure result: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync result: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close result: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish result: %w", err)
	}
	dir, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open result directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync result directory: %w", err)
	}
	success = true
	return nil
}

func addWarning(warnings []Warning, warning Warning) []Warning {
	if !slices.Contains(warnings, warning) {
		warnings = append(warnings, warning)
	}
	return warnings
}
