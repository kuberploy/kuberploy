package gitssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh/knownhosts"
)

const remoteRefOutputLimit = 64 << 10

var remoteCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// RemoteRefRequest identifies one configured SSH source and its exact key
// revision. KnownHosts must be operator-approved OpenSSH known_hosts content.
type RemoteRefRequest struct {
	Scope         Scope
	OwnerID       string
	KeyRevision   uint64
	RepositoryURL string
	Ref           string
	KnownHosts    []byte
}

// RemoteRefResolution is the immutable source identity observed from the Git
// server. Ref remains the requested ref when an annotated tag is peeled.
type RemoteRefResolution struct {
	CommitSHA  string
	Ref        string
	ObservedAt time.Time
}

type remoteRefExecutor interface {
	Run(context.Context, string, []string, []string) ([]byte, []byte, error)
}

type osRemoteRefExecutor struct{}

func (osRemoteRefExecutor) Run(ctx context.Context, executable string, args, environment []string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = environment
	var stdout, stderr limitedRemoteRefBuffer
	stdout.limit, stderr.limit = remoteRefOutputLimit, remoteRefOutputLimit
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if stdout.overflow || stderr.overflow {
		return stdout.Bytes(), stderr.Bytes(), errors.New("git ls-remote output exceeded limit")
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

type limitedRemoteRefBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedRemoteRefBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining < len(value) {
		b.overflow = true
		if remaining > 0 {
			_, _ = b.buffer.Write(value[:remaining])
		}
		return original, nil
	}
	_, _ = b.buffer.Write(value)
	return original, nil
}

func (b *limitedRemoteRefBuffer) Bytes() []byte { return b.buffer.Bytes() }

// ResolveRemoteRef resolves one exact branch or tag through the active exact
// key revision. It never discovers host keys or consults ambient Git config.
func (s *Service) ResolveRemoteRef(ctx context.Context, request RemoteRefRequest) (RemoteRefResolution, error) {
	request.OwnerID = strings.TrimSpace(request.OwnerID)
	request.RepositoryURL = strings.TrimSpace(request.RepositoryURL)
	request.Ref = strings.TrimSpace(request.Ref)
	if err := validateRemoteRefRequest(request); err != nil {
		return RemoteRefResolution{}, err
	}
	record, err := s.repository.active(ctx, request.Scope, request.OwnerID)
	if err != nil {
		return RemoteRefResolution{}, err
	}
	if record.metadata.Status != StatusActive || record.metadata.Revision != request.KeyRevision {
		return RemoteRefResolution{}, ErrKeyRevisionInactive
	}

	privateKey, err := s.encryption.Decrypt(ctx, record.envelope)
	if err != nil {
		return RemoteRefResolution{}, err
	}
	defer zero(privateKey)
	knownHosts := append([]byte(nil), request.KnownHosts...)
	defer zero(knownHosts)

	temporaryDirectory, err := os.MkdirTemp("", "kuberploy-gitssh-ref-*")
	if err != nil {
		return RemoteRefResolution{}, fmt.Errorf("create Git SSH remote-ref directory: %w", err)
	}
	defer os.RemoveAll(temporaryDirectory)
	if err = os.Chmod(temporaryDirectory, 0o700); err != nil {
		return RemoteRefResolution{}, fmt.Errorf("secure Git SSH remote-ref directory: %w", err)
	}
	privateKeyPath := filepath.Join(temporaryDirectory, "identity")
	knownHostsPath := filepath.Join(temporaryDirectory, "known_hosts")
	if err = writePrivateFile(privateKeyPath, privateKey); err != nil {
		return RemoteRefResolution{}, fmt.Errorf("write Git SSH remote-ref identity: %w", err)
	}
	if err = writePrivateFile(knownHostsPath, knownHosts); err != nil {
		return RemoteRefResolution{}, fmt.Errorf("write Git SSH remote-ref known_hosts: %w", err)
	}
	if _, err = knownhosts.New(knownHostsPath); err != nil {
		return RemoteRefResolution{}, ErrInvalidRemoteRef
	}

	patterns := []string{request.Ref}
	if strings.HasPrefix(request.Ref, "refs/tags/") {
		patterns = append(patterns, request.Ref+"^{}")
	}
	args := append([]string{"ls-remote", "--exit-code", request.RepositoryURL}, patterns...)
	environment := remoteRefEnvironment(temporaryDirectory, privateKeyPath, knownHostsPath)
	stdout, stderr, commandErr := s.remoteRefExecutor().Run(ctx, "git", args, environment)
	defer zero(stdout)
	defer zero(stderr)
	if commandErr != nil {
		var exitError interface{ ExitCode() int }
		if errors.As(commandErr, &exitError) && exitError.ExitCode() == 2 {
			return RemoteRefResolution{}, ErrRemoteRefNotFound
		}
		message := strings.TrimSpace(string(stderr))
		if message == "" {
			return RemoteRefResolution{}, fmt.Errorf("git ls-remote failed: %w", commandErr)
		}
		return RemoteRefResolution{}, fmt.Errorf("git ls-remote failed: %w: %s", commandErr, message)
	}
	commit, err := parseRemoteRef(stdout, request.Ref)
	if err != nil {
		return RemoteRefResolution{}, err
	}
	return RemoteRefResolution{CommitSHA: commit, Ref: request.Ref, ObservedAt: time.Now().UTC()}, nil
}

func (s *Service) remoteRefExecutor() remoteRefExecutor {
	if s.remoteRefs != nil {
		return s.remoteRefs
	}
	return osRemoteRefExecutor{}
}

func validateRemoteRefRequest(request RemoteRefRequest) error {
	if err := validateIdentity(request.Scope, request.OwnerID); err != nil {
		return err
	}
	parsed, err := url.Parse(request.RepositoryURL)
	if err != nil || parsed == nil || len(request.RepositoryURL) > 2048 || parsed.Scheme != "ssh" || parsed.Hostname() == "" ||
		parsed.User == nil || parsed.User.Username() == "" || parsed.Path == "" || parsed.Path == "/" || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || strings.ContainsAny(request.RepositoryURL, "\x00\r\n") {
		return ErrInvalidRemoteRef
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return ErrInvalidRemoteRef
	}
	if request.KeyRevision < 1 || !validRemoteRef(request.Ref) || len(request.KnownHosts) == 0 || len(request.KnownHosts) > 64<<10 ||
		bytes.ContainsAny(request.KnownHosts, "\x00\r") {
		return ErrInvalidRemoteRef
	}
	return nil
}

func validRemoteRef(ref string) bool {
	name := ""
	if strings.HasPrefix(ref, "refs/heads/") {
		name = strings.TrimPrefix(ref, "refs/heads/")
	} else if strings.HasPrefix(ref, "refs/tags/") {
		name = strings.TrimPrefix(ref, "refs/tags/")
	} else {
		return false
	}
	if name == "" || name == "@" || len(name) > 255 || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.Contains(name, "//") || strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.HasSuffix(name, ".") ||
		strings.HasSuffix(strings.ToLower(name), ".lock") || strings.ContainsAny(name, " ~^:?*[\\\x00\r\n\t") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

func writePrivateFile(path string, value []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(value)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func remoteRefEnvironment(home, privateKeyPath, knownHostsPath string) []string {
	sshCommand := strings.Join([]string{
		"ssh", "-F", "/dev/null", "-i", shellQuote(privateKeyPath),
		"-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes",
		"-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + shellQuote(knownHostsPath),
		"-o", "GlobalKnownHostsFile=/dev/null", "-o", "UpdateHostKeys=no",
	}, " ")
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "xdg"),
		"LC_ALL=C",
		"LANG=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
		"GIT_SSH_VARIANT=ssh",
		"GIT_SSH_COMMAND=" + sshCommand,
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func parseRemoteRef(output []byte, ref string) (string, error) {
	values := make(map[string]string, 2)
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		parts := bytes.Split(line, []byte{'\t'})
		if len(parts) != 2 {
			return "", ErrInvalidRemoteRef
		}
		commit := strings.ToLower(string(parts[0]))
		resolvedRef := string(parts[1])
		if !remoteCommitRE.MatchString(commit) || resolvedRef != ref && resolvedRef != ref+"^{}" {
			return "", ErrInvalidRemoteRef
		}
		if _, exists := values[resolvedRef]; exists {
			return "", ErrInvalidRemoteRef
		}
		values[resolvedRef] = commit
	}
	if peeled := values[ref+"^{}"]; peeled != "" && strings.HasPrefix(ref, "refs/tags/") {
		return peeled, nil
	}
	if commit := values[ref]; commit != "" {
		return commit, nil
	}
	return "", ErrRemoteRefNotFound
}

var _ io.Writer = (*limitedRemoteRefBuffer)(nil)
