package githubapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testProjectedReader(t *testing.T, maximum int64) (*ProjectedSecretReader, string) {
	t.Helper()
	root := t.TempDir()
	reader, err := newProjectedSecretReaderAt(root, maximum)
	if err != nil {
		t.Fatalf("new projected reader: %v", err)
	}
	return reader, root
}

func writeProjectedFixture(t *testing.T, root string, ref SecretRef, value []byte) string {
	t.Helper()
	directory := filepath.Join(root, ref.Name)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, ref.Key)
	if err := os.WriteFile(path, value, 0o440); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProjectedSecretReaderReturnsFreshExactBytes(t *testing.T) {
	reader, root := testProjectedReader(t, 4096)
	ref := SecretRef{Name: "github-app", Key: "webhook"}
	original := []byte("exact-secret\nbytes")
	writeProjectedFixture(t, root, ref, original)

	first, err := reader.ReadSecret(t.Context(), ref)
	if err != nil || string(first) != string(original) {
		t.Fatalf("first read=%q err=%v", first, err)
	}
	first[0] = 'X'
	second, err := reader.ReadSecret(t.Context(), ref)
	if err != nil || string(second) != string(original) {
		t.Fatalf("reader cached caller-owned bytes: %q err=%v", second, err)
	}
}

func TestProjectedSecretReaderAcceptsOnlyConfinedKubeletStyleSymlink(t *testing.T) {
	reader, root := testProjectedReader(t, 4096)
	ref := SecretRef{Name: "github-app", Key: "private-key"}
	dataDirectory := filepath.Join(root, "..2026_08_09")
	if err := os.MkdirAll(filepath.Join(dataDirectory, ref.Name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDirectory, ref.Name, ref.Key), []byte("projected-value"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(dataDirectory), filepath.Join(root, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ref.Name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..data", ref.Name, ref.Key), filepath.Join(root, ref.Name, ref.Key)); err != nil {
		t.Fatal(err)
	}

	value, err := reader.ReadSecret(t.Context(), ref)
	if err != nil || string(value) != "projected-value" {
		t.Fatalf("projected read=%q err=%v", value, err)
	}
}

func TestProjectedSecretReaderAcceptsKubeletNestedItemDirectorySymlink(t *testing.T) {
	reader, root := testProjectedReader(t, 4096)
	ref := SecretRef{Name: "runtime", Key: "private-key.pem"}
	dataDirectory := filepath.Join(root, "..2026_08_30")
	if err := os.MkdirAll(filepath.Join(dataDirectory, ref.Name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDirectory, ref.Name, ref.Key), []byte("projected-value"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(dataDirectory), filepath.Join(root, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", ref.Name), filepath.Join(root, ref.Name)); err != nil {
		t.Fatal(err)
	}

	value, err := reader.ReadSecret(t.Context(), ref)
	if err != nil || string(value) != "projected-value" {
		t.Fatalf("nested projected read=%q err=%v", value, err)
	}
}

func TestProjectedSecretReaderRejectsEscapesSpecialFilesAndOversize(t *testing.T) {
	ref := SecretRef{Name: "github-app", Key: "state"}
	for name, setup := range map[string]func(*testing.T, string, *ProjectedSecretReader){
		"escaped symlink": func(t *testing.T, root string, _ *ProjectedSecretReader) {
			outside := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, ref.Name), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, ref.Name, ref.Key)); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, root string, _ *ProjectedSecretReader) {
			if err := os.MkdirAll(filepath.Join(root, ref.Name, ref.Key), 0o750); err != nil {
				t.Fatal(err)
			}
		},
		"oversize": func(t *testing.T, root string, _ *ProjectedSecretReader) {
			writeProjectedFixture(t, root, ref, []byte(strings.Repeat("x", 65)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			reader, root := testProjectedReader(t, 64)
			setup(t, root, reader)
			if _, err := reader.ReadSecret(t.Context(), ref); !errors.Is(err, ErrSecretUnavailable) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestProjectedSecretReaderRejectsSymlinkRootInvalidRefAndCancellation(t *testing.T) {
	realRoot := t.TempDir()
	linkedRoot := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	reader, err := newProjectedSecretReaderAt(linkedRoot, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reader.ReadSecret(t.Context(), SecretRef{Name: "github-app", Key: "state"}); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("symlink root err=%v", err)
	}

	reader, root := testProjectedReader(t, 4096)
	writeProjectedFixture(t, root, SecretRef{Name: "github-app", Key: "state"}, []byte("value"))
	if _, err = reader.ReadSecret(t.Context(), SecretRef{Name: "../escape", Key: "state"}); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("invalid ref err=%v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = reader.ReadSecret(cancelled, SecretRef{Name: "github-app", Key: "state"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err=%v", err)
	}
}
