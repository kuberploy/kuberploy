package migrations

import "testing"

func TestPublishedInitialMigrationChecksumIsFrozen(t *testing.T) {
	t.Parallel()

	history, err := History()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) < 3 {
		t.Fatalf("migration count = %d, want at least 3", len(history))
	}
	if history[0].Name != "001_initial" {
		t.Fatalf("first migration = %q, want 001_initial", history[0].Name)
	}
	const frozenChecksum = "1aa6590b46d37e6a71dfdc85df7a7d8b7376b41e18deb02cab6b16e52e4cad79"
	if history[0].Checksum != frozenChecksum {
		t.Fatalf("001_initial checksum = %q, want frozen checksum %q", history[0].Checksum, frozenChecksum)
	}
	if history[1].Name != "002_auto_deploy_policy_cleanup" {
		t.Fatalf("second migration = %q, want 002_auto_deploy_policy_cleanup", history[1].Name)
	}
	if history[2].Name != "003_auto_deploy_disable_after_drift" {
		t.Fatalf("third migration = %q, want 003_auto_deploy_disable_after_drift", history[2].Name)
	}
}
