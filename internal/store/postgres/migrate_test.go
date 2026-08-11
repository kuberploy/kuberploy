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
	if len(history) != 1 {
		t.Fatalf("stable baseline must contain exactly one migration, got %d: %#v", len(history), history)
	}
	if history[0].Name != migrations.CurrentSchema {
		t.Fatalf("latest migration = %q, CurrentSchema = %q", history[0].Name, migrations.CurrentSchema)
	}
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(history[0].Checksum) {
		t.Fatalf("migration checksum is not canonical SHA-256 text: %q", history[0].Checksum)
	}
}
