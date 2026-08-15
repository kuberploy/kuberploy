package migrations

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// FS contains the immutable SQL history shipped in the migration image and
// embedded in Go only so API and worker can verify the exact applied history.
// Long-running processes never execute these files.
//
//go:embed prisma/migrations/*/migration.sql
var FS embed.FS

// CurrentSchema is bumped with every ordered migration and is published in the
// immutable release manifest for Helm-driven install and upgrade qualification.
const CurrentSchema = "014_helm_cascade_path_absence_receipt"

// RecoverableRC171Migration identifies the one published migration failure
// whose rolled-back Prisma evidence may coexist with the canonical successful
// history. Migration 012 repairs this exact failure without rewriting 011.
const RecoverableRC171Migration = "011_helm_application_cascade_preflight"
const RecoverableRC171Checksum = "666baa4526942038b2a01ea91ffbbeda201ae9485035063b4aed21b83cc286ad"
const RecoverableRC171CleanupMigration = "012_recover_rc171_cascade_preflight"

var recoverableRC171LogFragments = [...]string{
	"Migration name: 011_helm_application_cascade_preflight",
	"Database error code: 23514",
	"Terminal Helm protected Application intents are immutable",
	"PL/pgSQL function public.validate_helm_protected_application_intent()",
}

// IsRecoverableRolledBackMigration rejects every noncanonical Prisma history
// row except the exact transactionally rolled-back RC171 failure.
func IsRecoverableRolledBackMigration(name, checksum, logs string, appliedSteps int) bool {
	if name == RecoverableRC171CleanupMigration && logs == "" && (appliedSteps == 0 || appliedSteps == 1) {
		body, err := FS.ReadFile("prisma/migrations/" + name + "/migration.sql")
		if err != nil {
			return false
		}
		digest := sha256.Sum256(body)
		return checksum == hex.EncodeToString(digest[:])
	}
	if appliedSteps != 0 {
		return false
	}
	if name != RecoverableRC171Migration || checksum != RecoverableRC171Checksum {
		return false
	}
	// SIGTERM/SIGKILL can stop Prisma after it inserts the migration attempt but
	// before it persists an error report. Before resolving that empty-log row,
	// the runner proves either a full rollback or the exact committed 011
	// authority postimage and bounded shim.
	if logs == "" {
		return true
	}
	for _, fragment := range recoverableRC171LogFragments {
		if !strings.Contains(logs, fragment) {
			return false
		}
	}
	return true
}

var namePattern = regexp.MustCompile(`^[0-9]{3}_[a-z0-9]+(?:_[a-z0-9]+)*$`)

// Migration is the immutable identity Prisma records in _prisma_migrations.
type Migration struct {
	Name     string
	Checksum string
}

// History returns the canonical ordered migration names and Prisma-compatible
// SHA-256 checksums derived from the same SQL bytes copied into the migration
// container.
func History() ([]Migration, error) {
	entries, err := FS.ReadDir("prisma/migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded Prisma migrations: %w", err)
	}
	history := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !namePattern.MatchString(name) {
			return nil, fmt.Errorf("Prisma migration has noncanonical name %q", name)
		}
		body, readErr := FS.ReadFile("prisma/migrations/" + name + "/migration.sql")
		if readErr != nil {
			return nil, fmt.Errorf("read Prisma migration %s: %w", name, readErr)
		}
		digest := sha256.Sum256(body)
		history = append(history, Migration{Name: name, Checksum: hex.EncodeToString(digest[:])})
	}
	sort.Slice(history, func(i, j int) bool { return history[i].Name < history[j].Name })
	if len(history) == 0 {
		return nil, fmt.Errorf("no embedded Prisma migrations")
	}
	if history[len(history)-1].Name != CurrentSchema {
		return nil, fmt.Errorf("current schema %q does not match latest Prisma migration %q", CurrentSchema, history[len(history)-1].Name)
	}
	return history, nil
}
