package migrations

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
)

// FS contains the immutable SQL history shipped in the migration image and
// embedded in Go only so API and worker can verify the exact applied history.
// Long-running processes never execute these files.
//
//go:embed prisma/migrations/*/migration.sql
var FS embed.FS

// CurrentSchema is bumped with every ordered migration and is compared with
// the verified release manifest before accepting a platform upgrade.
const CurrentSchema = "006_remove_platform_self_upgrade"

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
