package gitssh

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

type remoteRefExecutorFunc func(context.Context, string, []string, []string) ([]byte, []byte, error)

func (f remoteRefExecutorFunc) Run(ctx context.Context, executable string, args, environment []string) ([]byte, []byte, error) {
	return f(ctx, executable, args, environment)
}

type remoteRefExitError int

func (e remoteRefExitError) Error() string { return "git exited" }
func (e remoteRefExitError) ExitCode() int { return int(e) }

type scrubEncryption struct {
	plaintext []byte
	decrypted []byte
}

func (e *scrubEncryption) Encrypt(_ context.Context, plaintext []byte) (PrivateKeyEnvelope, error) {
	e.plaintext = append([]byte(nil), plaintext...)
	return PrivateKeyEnvelope{KeyVersion: "test-v1", Ciphertext: []byte("ciphertext")}, nil
}

func (e *scrubEncryption) Decrypt(_ context.Context, envelope PrivateKeyEnvelope) ([]byte, error) {
	if envelope.KeyVersion != "test-v1" {
		return nil, ErrInvalidEnvelope
	}
	e.decrypted = append([]byte(nil), e.plaintext...)
	return e.decrypted, nil
}

func TestResolveRemoteRefUsesExactActiveKeyAndIsolatedGit(t *testing.T) {
	encryption := &scrubEncryption{}
	service, err := NewService(NewMemoryRepository(), encryption)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := service.Create(context.Background(), CreateRequest{Scope: ScopeApp, OwnerID: "app-1"})
	if err != nil {
		t.Fatal(err)
	}
	pin, err := NewHostKeyPin("git.example.test:22", string(ssh.MarshalAuthorizedKey(testSSHPublicKey(t))))
	if err != nil {
		t.Fatal(err)
	}
	knownHosts, err := KnownHosts([]HostKeyPin{pin})
	if err != nil {
		t.Fatal(err)
	}

	stdout := []byte(strings.Repeat("A", 40) + "\trefs/heads/main\n")
	var temporaryDirectory string
	service.remoteRefs = remoteRefExecutorFunc(func(_ context.Context, executable string, args, environment []string) ([]byte, []byte, error) {
		if executable != "git" {
			t.Fatalf("executable = %q", executable)
		}
		wantArgs := []string{"ls-remote", "--exit-code", "ssh://git@git.example.test/team/repository.git", "refs/heads/main"}
		if !reflect.DeepEqual(args, wantArgs) {
			t.Fatalf("args = %#v", args)
		}
		env := environmentMap(t, environment)
		temporaryDirectory = env["HOME"]
		if env["GIT_CONFIG_NOSYSTEM"] != "1" || env["GIT_CONFIG_GLOBAL"] != "/dev/null" || env["GIT_TERMINAL_PROMPT"] != "0" {
			t.Fatalf("Git isolation environment = %#v", env)
		}
		sshCommand := env["GIT_SSH_COMMAND"]
		for _, option := range []string{"-F /dev/null", "IdentitiesOnly=yes", "IdentityAgent=none", "BatchMode=yes", "StrictHostKeyChecking=yes", "GlobalKnownHostsFile=/dev/null"} {
			if !strings.Contains(sshCommand, option) {
				t.Fatalf("GIT_SSH_COMMAND missing %q: %q", option, sshCommand)
			}
		}
		assertPrivateRemoteRefFiles(t, temporaryDirectory, encryption.plaintext, knownHosts)
		return stdout, nil, nil
	})

	resolution, err := service.ResolveRemoteRef(context.Background(), RemoteRefRequest{
		Scope: ScopeApp, OwnerID: "app-1", KeyRevision: metadata.Revision,
		RepositoryURL: "ssh://git@git.example.test/team/repository.git", Ref: "refs/heads/main", KnownHosts: knownHosts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.CommitSHA != strings.Repeat("a", 40) || resolution.Ref != "refs/heads/main" || resolution.ObservedAt.IsZero() || resolution.ObservedAt.Location().String() != "UTC" {
		t.Fatalf("resolution = %#v", resolution)
	}
	if _, err = os.Stat(temporaryDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary directory remains: %v", err)
	}
	if !allZero(encryption.decrypted) || !allZero(stdout) {
		t.Fatal("decrypted key or command output was not scrubbed")
	}
}

func TestResolveRemoteRefPrefersPeeledAnnotatedTag(t *testing.T) {
	service, metadata, knownHosts := remoteRefTestFixture(t)
	objectCommit := strings.Repeat("1", 40)
	peeledCommit := strings.Repeat("2", 40)
	service.remoteRefs = remoteRefExecutorFunc(func(_ context.Context, _ string, args, _ []string) ([]byte, []byte, error) {
		want := []string{"ls-remote", "--exit-code", "ssh://git@git.example.test/team/repository.git", "refs/tags/v1.2.3", "refs/tags/v1.2.3^{}"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args = %#v", args)
		}
		return []byte(objectCommit + "\trefs/tags/v1.2.3\n" + peeledCommit + "\trefs/tags/v1.2.3^{}\n"), nil, nil
	})
	resolution, err := service.ResolveRemoteRef(context.Background(), RemoteRefRequest{
		Scope: ScopeProject, OwnerID: "project-1", KeyRevision: metadata.Revision,
		RepositoryURL: "ssh://git@git.example.test/team/repository.git", Ref: "refs/tags/v1.2.3", KnownHosts: knownHosts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.CommitSHA != peeledCommit || resolution.Ref != "refs/tags/v1.2.3" {
		t.Fatalf("resolution = %#v", resolution)
	}
}

func TestResolveRemoteRefRejectsInactiveRevisionBeforeDecryptOrGit(t *testing.T) {
	service, metadata, knownHosts := remoteRefTestFixture(t)
	encryption := service.encryption.(*scrubEncryption)
	called := false
	service.remoteRefs = remoteRefExecutorFunc(func(context.Context, string, []string, []string) ([]byte, []byte, error) {
		called = true
		return nil, nil, nil
	})
	_, err := service.ResolveRemoteRef(context.Background(), RemoteRefRequest{
		Scope: ScopeProject, OwnerID: "project-1", KeyRevision: metadata.Revision + 1,
		RepositoryURL: "ssh://git@git.example.test/team/repository.git", Ref: "refs/heads/main", KnownHosts: knownHosts,
	})
	if !errors.Is(err, ErrKeyRevisionInactive) || called || len(encryption.decrypted) != 0 {
		t.Fatalf("error=%v called=%v decrypted=%d", err, called, len(encryption.decrypted))
	}
}

func TestResolveRemoteRefRejectsInvalidInputsAndMissingRefs(t *testing.T) {
	service, metadata, knownHosts := remoteRefTestFixture(t)
	base := RemoteRefRequest{Scope: ScopeProject, OwnerID: "project-1", KeyRevision: metadata.Revision,
		RepositoryURL: "ssh://git@git.example.test/team/repository.git", Ref: "refs/heads/main", KnownHosts: knownHosts}
	for name, mutate := range map[string]func(*RemoteRefRequest){
		"HTTP repository": func(request *RemoteRefRequest) { request.RepositoryURL = "https://git.example.test/repository.git" },
		"password": func(request *RemoteRefRequest) {
			request.RepositoryURL = "ssh://git:secret@git.example.test/repository.git"
		},
		"short ref":  func(request *RemoteRefRequest) { request.Ref = "main" },
		"glob ref":   func(request *RemoteRefRequest) { request.Ref = "refs/heads/release/*" },
		"empty pins": func(request *RemoteRefRequest) { request.KnownHosts = nil },
		"invalid pins": func(request *RemoteRefRequest) {
			request.KnownHosts = []byte("not known_hosts data\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := base
			mutate(&request)
			if _, err := service.ResolveRemoteRef(context.Background(), request); !errors.Is(err, ErrInvalidRemoteRef) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	service.remoteRefs = remoteRefExecutorFunc(func(context.Context, string, []string, []string) ([]byte, []byte, error) {
		return nil, nil, remoteRefExitError(2)
	})
	if _, err := service.ResolveRemoteRef(context.Background(), base); !errors.Is(err, ErrRemoteRefNotFound) {
		t.Fatalf("missing ref error = %v", err)
	}
}

func remoteRefTestFixture(t *testing.T) (*Service, KeyMetadata, []byte) {
	t.Helper()
	service, err := NewService(NewMemoryRepository(), &scrubEncryption{})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := service.Create(context.Background(), CreateRequest{Scope: ScopeProject, OwnerID: "project-1"})
	if err != nil {
		t.Fatal(err)
	}
	pin, err := NewHostKeyPin("git.example.test:22", string(ssh.MarshalAuthorizedKey(testSSHPublicKey(t))))
	if err != nil {
		t.Fatal(err)
	}
	knownHosts, err := KnownHosts([]HostKeyPin{pin})
	if err != nil {
		t.Fatal(err)
	}
	return service, metadata, knownHosts
}

func environmentMap(t *testing.T, environment []string) map[string]string {
	t.Helper()
	result := make(map[string]string, len(environment))
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if !found || key == "" {
			t.Fatalf("invalid environment entry %q", entry)
		}
		if _, duplicate := result[key]; duplicate {
			t.Fatalf("duplicate environment key %q", key)
		}
		result[key] = value
	}
	return result
}

func assertPrivateRemoteRefFiles(t *testing.T, directory string, privateKey, knownHosts []byte) {
	t.Helper()
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("temporary directory mode=%v err=%v", info.Mode().Perm(), err)
	}
	for name, want := range map[string][]byte{"identity": privateKey, "known_hosts": knownHosts} {
		path := directory + string(os.PathSeparator) + name
		info, err = os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%v err=%v", name, info.Mode().Perm(), err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s contents differ", name)
		}
	}
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
