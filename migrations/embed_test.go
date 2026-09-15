package migrations

import "testing"

func TestMigrationHistoryMatchesPublishedBaseline(t *testing.T) {
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
	const baselineChecksum = "aad19c2e589bb0992597707308b84653a51a8cde95f4a456f4d411c6bb9beda2"
	if history[0].Checksum != baselineChecksum {
		t.Fatalf("001_initial checksum = %q, want baseline checksum %q", history[0].Checksum, baselineChecksum)
	}
}
