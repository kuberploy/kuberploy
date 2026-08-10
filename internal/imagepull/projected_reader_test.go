package imagepull

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectedMaterialReaderAcceptsOnlyConfinedKubeletProjection(t *testing.T) {
	root := t.TempDir()
	reader, err := newProjectedMaterialReaderAt(root, MaximumDockerConfigBytes)
	if err != nil {
		t.Fatal(err)
	}
	profile := testRuntimeConfig().Profiles[0]
	dataDir := filepath.Join(root, "..2026_08_09")
	if err = os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	value := []byte(`{"auths":{"registry.example.test:5000":{"auth":"private"}}}`)
	if err = os.WriteFile(filepath.Join(dataDir, "dockerconfigjson"), value, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("..2026_08_09", filepath.Join(root, "..data")); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(root, profile.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join("..", "..data", "dockerconfigjson"), filepath.Join(root, profile.Name, "dockerconfigjson")); err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadDockerConfig(t.Context(), profile)
	if err != nil || !bytes.Equal(got, value) {
		t.Fatalf("read=%q err=%v", got, err)
	}
	clearBytes(got)
}

func TestProjectedMaterialReaderRejectsEscapesRootLinksAndSpecialFiles(t *testing.T) {
	profile := testRuntimeConfig().Profiles[0]
	for name, setup := range map[string]func(*testing.T, string) string{
		"escaped key": func(t *testing.T, root string) string {
			outside := filepath.Join(t.TempDir(), "private")
			if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, profile.Name), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, profile.Name, "dockerconfigjson")); err != nil {
				t.Fatal(err)
			}
			return root
		},
		"directory key": func(t *testing.T, root string) string {
			if err := os.MkdirAll(filepath.Join(root, profile.Name, "dockerconfigjson"), 0o700); err != nil {
				t.Fatal(err)
			}
			return root
		},
		"linked root": func(t *testing.T, root string) string {
			realRoot := t.TempDir()
			linked := filepath.Join(root, "linked")
			if err := os.Symlink(realRoot, linked); err != nil {
				t.Fatal(err)
			}
			return linked
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := setup(t, t.TempDir())
			reader, err := newProjectedMaterialReaderAt(root, MaximumDockerConfigBytes)
			if err != nil && name == "linked root" {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = reader.ReadDockerConfig(t.Context(), profile); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("unsafe projection read: %v", err)
			}
		})
	}
}

func TestProjectedMaterialReaderRejectsOversizeAndCancellation(t *testing.T) {
	root := t.TempDir()
	reader, err := newProjectedMaterialReaderAt(root, 64)
	if err != nil {
		t.Fatal(err)
	}
	profile := testRuntimeConfig().Profiles[0]
	if err = os.Mkdir(filepath.Join(root, profile.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, profile.Name, "dockerconfigjson"), bytes.Repeat([]byte{'a'}, 65), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = reader.ReadDockerConfig(t.Context(), profile); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("oversize read=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = reader.ReadDockerConfig(ctx, profile); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read=%v", err)
	}
}

func TestProjectedMaterialReaderRejectsUnsafePermissions(t *testing.T) {
	profile := testRuntimeConfig().Profiles[0]
	for _, mode := range []os.FileMode{0o660, 0o604, 0o444, 0o550} {
		t.Run(mode.String(), func(t *testing.T) {
			root := t.TempDir()
			reader, err := newProjectedMaterialReaderAt(root, 64)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Mkdir(filepath.Join(root, profile.Name), 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, profile.Name, "dockerconfigjson")
			if err = os.WriteFile(path, []byte(`{"auths":{}}`), mode); err != nil {
				t.Fatal(err)
			}
			if err = os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if _, err = reader.ReadDockerConfig(t.Context(), profile); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("unsafe mode %v read=%v", mode, err)
			}
		})
	}
}
