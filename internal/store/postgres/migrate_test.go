package postgres

import (
	"regexp"
	"testing"

	"github.com/kuberploy/kuberploy/migrations"
)

func TestEmbeddedPrismaHistoryIsCanonicalAndCurrent(t *testing.T) {
	t.Parallel()
	history, err := migrations.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 {
		t.Fatal("embedded Prisma history is empty")
	}
	namePattern := regexp.MustCompile(`^[0-9]{3}_[a-z0-9_]+$`)
	checksumPattern := regexp.MustCompile(`^[a-f0-9]{64}$`)
	for index, migration := range history {
		if !namePattern.MatchString(migration.Name) {
			t.Fatalf("migration %d name is not canonical: %q", index, migration.Name)
		}
		if index > 0 && history[index-1].Name >= migration.Name {
			t.Fatalf("migration history is not strictly ordered: %q then %q", history[index-1].Name, migration.Name)
		}
		if !checksumPattern.MatchString(migration.Checksum) {
			t.Fatalf("migration %q checksum is not canonical SHA-256 text: %q", migration.Name, migration.Checksum)
		}
	}
	if latest := history[len(history)-1].Name; latest != migrations.CurrentSchema {
		t.Fatalf("latest migration = %q, CurrentSchema = %q", latest, migrations.CurrentSchema)
	}
}
