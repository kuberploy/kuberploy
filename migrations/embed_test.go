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
	const baselineChecksum = "ca8e0785834c44ea29fd318f548ebd543e0267e9efba6118296de5ea8f66264a"
	if history[0].Checksum != baselineChecksum {
		t.Fatalf("001_initial checksum = %q, want baseline checksum %q", history[0].Checksum, baselineChecksum)
	}
}
