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
	const baselineChecksum = "6839c3e1667c299b0ac941510567cdd9e2f0e13a89c166a20b1c08a1538209b2"
	if history[0].Checksum != baselineChecksum {
		t.Fatalf("001_initial checksum = %q, want baseline checksum %q", history[0].Checksum, baselineChecksum)
	}
}
