package gitprojection

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAskPassBrokerServesOnlyBoundedCredentialRequestsAndCleansSocket(t *testing.T) {
	credential := GitCredential{Username: []byte(gitHubAppUsername), Password: []byte(strings.Repeat("p", 40)), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	broker, err := startAskPassBroker(t.TempDir(), &credential)
	if err != nil {
		t.Fatal(err)
	}
	path, directory := broker.path, broker.directory
	request := func(value byte) string {
		t.Helper()
		connection, dialErr := net.DialTimeout("unix", path, time.Second)
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		defer connection.Close()
		if _, dialErr = connection.Write([]byte{value}); dialErr != nil {
			t.Fatal(dialErr)
		}
		response, readErr := io.ReadAll(io.LimitReader(connection, maximumGitPasswordBytes+2))
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(response)
	}
	if got := request(requestUsername); got != gitHubAppUsername+"\n" {
		t.Fatalf("username=%q", got)
	}
	if got := request(requestPassword); got != strings.Repeat("p", 40)+"\n" {
		t.Fatalf("password response bytes=%d", len(got))
	}
	if got := request(99); got != "" {
		t.Fatalf("unknown request returned data bytes=%d", len(got))
	}
	broker.close()
	if _, err = os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("broker socket survived close: %v", err)
	}
	if _, err = os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("broker directory survived close: %v", err)
	}
}

func TestMirrorManagerRejectsMixedOrTestCredentialTransport(t *testing.T) {
	provider := staticGitCredentialProvider{}
	for name, manager := range map[string]*MirrorManager{
		"legacy plus ephemeral": {Root: "/tmp/kuberploy-test", UseCredentials: true, CredentialProvider: provider},
		"local plus ephemeral":  {Root: "/tmp/kuberploy-test", AllowLocalTests: true, LocalRemote: "/tmp/remote", CredentialProvider: provider},
	} {
		t.Run(name, func(t *testing.T) {
			if err := manager.validate(); err == nil {
				t.Fatal("ambiguous credential transport was accepted")
			}
		})
	}
}

func TestPreparedRepositoryCloseAlwaysClearsCredential(t *testing.T) {
	password := []byte(strings.Repeat("p", 40))
	credential := &GitCredential{Username: []byte(gitHubAppUsername), Password: password, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	prepared := &PreparedRepository{manager: &MirrorManager{credential: credential}}
	if err := prepared.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Trim(string(password), "\x00") != "" || prepared.manager.credential != nil {
		t.Fatal("prepared repository retained credential material")
	}
}

func TestPreparedRepositoryCloseUsesNoExpiredCredentialForLocalCleanup(t *testing.T) {
	root := t.TempDir()
	mirror := filepath.Join(root, "mirror.git")
	seed := filepath.Join(root, "seed")
	worktree := filepath.Join(root, "worktree")
	run := func(directory string, args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = directory
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	_ = run(root, "init", "--bare", mirror)
	_ = run(root, "init", "--initial-branch=main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("safe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = run(seed, "add", "README.md")
	_ = run(seed, "commit", "-m", "seed")
	head := run(seed, "rev-parse", "HEAD")
	_ = run(seed, "push", mirror, "HEAD:refs/heads/main")
	_ = run(root, "--git-dir="+mirror, "worktree", "add", "--detach", worktree, head)

	password := []byte(strings.Repeat("p", 40))
	credential := &GitCredential{Username: []byte(gitHubAppUsername), Password: password, ExpiresAt: time.Now().UTC().Add(-time.Minute)}
	prepared := &PreparedRepository{MirrorPath: mirror, WorktreePath: worktree, manager: &MirrorManager{Root: root, credential: credential}}
	if err := prepared.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(worktree); !os.IsNotExist(err) {
		t.Fatalf("local worktree survived cleanup: %v", err)
	}
	if strings.Trim(string(password), "\x00") != "" || prepared.manager.credential != nil {
		t.Fatal("prepared repository retained expired credential material")
	}
}

func TestGitCredentialFormattingIsAlwaysRedacted(t *testing.T) {
	credential := GitCredential{Username: []byte(gitHubAppUsername), Password: []byte("super-secret-token-material"), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	for _, formatted := range []string{fmt.Sprint(credential), fmt.Sprintf("%#v", credential), fmt.Sprintf("%+v", credential), fmt.Sprintf("%d", credential), fmt.Sprintf("%x", credential), fmt.Sprint(&credential)} {
		if strings.Contains(formatted, "super-secret") || strings.Contains(formatted, "120 45 97 99 99 101 115 115") || formatted != "GitCredential{redacted}" {
			t.Fatalf("credential formatting was not opaque: %q", formatted)
		}
	}
	var logOutput bytes.Buffer
	slog.New(slog.NewJSONHandler(&logOutput, nil)).Info("credential", "value", credential)
	if strings.Contains(logOutput.String(), "super-secret") || !strings.Contains(logOutput.String(), "GitCredential{redacted}") {
		t.Fatalf("structured log credential was not opaque: %q", logOutput.String())
	}
}

type staticGitCredentialProvider struct{}

func (staticGitCredentialProvider) AcquireGitCredential(_ context.Context, _ Binding) (GitCredential, error) {
	return GitCredential{}, nil
}
