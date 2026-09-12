package migrations

import "testing"

func TestPreStableMigrationHistoryIsSingleBaseline(t *testing.T) {
	t.Parallel()

	history, err := History()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("migration count = %d, want exactly 1", len(history))
	}
	if history[0].Name != "001_initial" {
		t.Fatalf("first migration = %q, want 001_initial", history[0].Name)
	}
	const baselineChecksum = "979c30f015b081fcaaa918f0a34dc976737c767d7c484d95e0db5d8533c4f0eb"
	if history[0].Checksum != baselineChecksum {
		t.Fatalf("001_initial checksum = %q, want baseline checksum %q", history[0].Checksum, baselineChecksum)
	}
}
