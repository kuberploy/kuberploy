package postgres

import (
	"sort"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/migrations"
)

func TestLoadMigrationFilename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		load bool
		err  bool
	}{
		{name: "001_initial.sql", load: true},
		{name: "044_outbox_valkey_dataset_replay.sql", load: true},
		{name: "045_project_registry_pull_credentials.sql", load: true},
		{name: "._001_initial.sql"},
		{name: ".DS_Store"},
		{name: "README.md"},
		{name: "1_initial.sql", err: true},
		{name: "046-UPPER.sql", err: true},
		{name: "046_bad-name.sql", err: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			load, err := loadMigrationFilename(test.name)
			if (err != nil) != test.err {
				t.Fatalf("loadMigrationFilename(%q) error = %v, want error %v", test.name, err, test.err)
			}
			if load != test.load {
				t.Fatalf("loadMigrationFilename(%q) = %v, want %v", test.name, load, test.load)
			}
		})
	}
}

func TestEmbeddedMigrationNamesAreCanonicalAndCurrent(t *testing.T) {
	t.Parallel()

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		load, err := loadMigrationFilename(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if load {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("no embedded migrations")
	}
	sort.Strings(names)
	latest := strings.TrimSuffix(names[len(names)-1], ".sql")
	if latest != migrations.CurrentSchema {
		t.Fatalf("latest embedded migration = %q, CurrentSchema = %q", latest, migrations.CurrentSchema)
	}
}
