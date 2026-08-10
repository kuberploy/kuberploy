package gitops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
)

const DefaultAskPassExecutable = "/usr/local/bin/kuberploy-git-askpass"
const askPassApprovedHostEnv = "KUBERPLOY_GIT_ASKPASS_APPROVED_HOST"

const trustedLocalConfig = `[core]
	repositoryformatversion = 0
	filemode = true
	bare = false
	logallrefupdates = true
	ignorecase = false
[http]
	followRedirects = false
`

type Writer struct {
	Root        string
	Remote      string
	Branch      string
	AuthorName  string
	AuthorEmail string
	SyncTimeout time.Duration
	// AllowLocalTransport exists only for hermetic tests that use a temporary
	// bare repository. Production writers accept credential-free HTTPS remotes.
	AllowLocalTransport bool
	// UseCredentialFiles enables the fixed-path, release-image askpass helper.
	// The username and password remain in the projected Secret volume; neither
	// value is placed in a URL, process argument, Git config, or environment.
	UseCredentialFiles bool
	// DefaultIngressClass is written into every generated route so Git stays
	// explicit when a cluster does not use the conventional "traefik" name.
	DefaultIngressClass string
	mu                  sync.Mutex
}

func (w *Writer) Ensure(ctx context.Context) error {
	if w.Root == "" || !filepath.IsAbs(w.Root) || filepath.Clean(w.Root) != w.Root || w.Root == string(os.PathSeparator) {
		return errors.New("git working repository path must be a clean non-root absolute path")
	}
	if w.Remote != "" {
		if err := w.validateRemote(); err != nil {
			return err
		}
	}
	if w.UseCredentialFiles {
		if w.Remote == "" || w.AllowLocalTransport {
			return errors.New("Git credential files require a production HTTPS remote")
		}
		info, err := os.Stat(DefaultAskPassExecutable)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			return errors.New("Git credential askpass executable is unavailable")
		}
	}
	if _, _, err := w.authorIdentity(); err != nil {
		return err
	}
	if err := os.MkdirAll(w.Root, 0o750); err != nil {
		return fmt.Errorf("create Git root: %w", err)
	}
	branch := w.Branch
	if branch == "" {
		branch = "main"
	}
	gitDirectory := filepath.Join(w.Root, ".git")
	gitInfo, err := os.Lstat(gitDirectory)
	if errors.Is(err, os.ErrNotExist) {
		if _, err = w.git(ctx, "init", "--initial-branch="+branch); err != nil {
			return err
		}
		gitInfo, err = os.Lstat(gitDirectory)
	} else if err != nil {
		return fmt.Errorf("inspect Git root: %w", err)
	}
	if err != nil || gitInfo.Mode()&os.ModeSymlink != 0 || !gitInfo.IsDir() {
		return errors.New("Git metadata path must be a real directory")
	}
	if err = w.sanitizeLocalConfig(); err != nil {
		return err
	}
	if w.Remote != "" {
		if err := w.syncRemote(ctx); err != nil {
			return err
		}
	}
	return nil
}

// sanitizeLocalConfig removes every repository-local transport, credential,
// hook, filter, include, signing, or URL-rewrite setting. This worktree is an
// internal disposable mirror; no local Git configuration is user state.
func (w *Writer) sanitizeLocalConfig() error {
	gitDirectory := filepath.Join(w.Root, ".git")
	gitInfo, err := os.Lstat(gitDirectory)
	if err != nil || gitInfo.Mode()&os.ModeSymlink != 0 || !gitInfo.IsDir() {
		return errors.New("Git metadata path must be a real directory")
	}
	configPath := filepath.Join(gitDirectory, "config")
	if info, inspectErr := os.Lstat(configPath); inspectErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("Git local config path must be a regular file")
		}
	} else if !errors.Is(inspectErr, os.ErrNotExist) {
		return fmt.Errorf("inspect Git local config: %w", inspectErr)
	}
	temporary, err := os.CreateTemp(gitDirectory, ".config-*")
	if err != nil {
		return fmt.Errorf("create trusted Git local config: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName) //nolint:errcheck
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.WriteString(trustedLocalConfig)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write trusted Git local config: %w", err)
	}
	if err = os.Rename(temporaryName, configPath); err != nil {
		return fmt.Errorf("replace Git local config: %w", err)
	}
	return nil
}

func (w *Writer) authorIdentity() (string, string, error) {
	name, email := w.AuthorName, w.AuthorEmail
	if name == "" {
		name = "Kuberploy"
	}
	if email == "" {
		email = "gitops@kuberploy.local"
	}
	valid := func(value string, maximum int) bool {
		return value != "" && len(value) <= maximum && utf8.ValidString(value) && strings.TrimSpace(value) == value &&
			strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
	}
	if !valid(name, 128) || !valid(email, 254) || strings.Count(email, "@") != 1 || strings.ContainsAny(email, " <>\"") {
		return "", "", errors.New("Git author identity is invalid")
	}
	return name, email, nil
}

func (w *Writer) validateRemote() error {
	if len(w.Remote) > 2048 || strings.TrimSpace(w.Remote) != w.Remote || strings.IndexFunc(w.Remote, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return errors.New("Git remote is empty, oversized, or contains control characters")
	}
	if w.AllowLocalTransport {
		if !filepath.IsAbs(w.Remote) || filepath.Clean(w.Remote) != w.Remote || w.Remote == string(os.PathSeparator) {
			return errors.New("test-only local Git remote must be a clean non-root absolute path")
		}
		return nil
	}
	u, err := url.Parse(w.Remote)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path == "" || u.Path == "/" {
		return errors.New("production Git remote must be a credential-free HTTPS repository URL")
	}
	for _, segment := range strings.Split(u.EscapedPath(), "/") {
		if segment == "." || segment == ".." || strings.EqualFold(segment, "%2e") || strings.EqualFold(segment, "%2e%2e") {
			return errors.New("production Git remote path is not canonical")
		}
	}
	return nil
}

// Write commits the canonical AppConfig. The operation trailer makes a retry
// after a process crash discover the already-created commit.
func (w *Writer) Write(ctx context.Context, op domain.Operation, p domain.Project, e domain.Environment, a domain.Application, d domain.Deployment) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	content := append([]byte(nil), d.ConfigRaw...)
	var err error
	if len(content) == 0 {
		content, err = renderAppConfig(p, e, a, d, w.DefaultIngressClass)
		if err != nil {
			return "", err
		}
	}
	if diagnostics := appconfig.ValidateBinding(content, p, e, a, d); len(diagnostics) > 0 {
		return "", fmt.Errorf("refusing AppConfig operation snapshot: %s at %s", diagnostics[0].Detail, diagnostics[0].Pointer)
	}
	if err := w.Ensure(ctx); err != nil {
		return "", err
	}
	rel := filepath.Join("environments", e.ID, "apps", a.ID, "app.yaml")
	relSlash := filepath.ToSlash(rel)
	marker := "Kuberploy-Operation: " + op.ID
	if revision, err := w.operationRevision(ctx, marker); err == nil && revision != "" {
		if err = w.verifyRevisionContent(ctx, revision, relSlash, content); err != nil {
			return "", fmt.Errorf("operation marker does not identify the accepted AppConfig: %w", err)
		}
		if err = w.push(ctx); err != nil {
			return "", err
		}
		revision, err = w.operationRevision(ctx, marker)
		if err != nil || revision == "" {
			return "", errors.New("pushed operation commit is no longer reachable")
		}
		if err = w.verifyRevisionContent(ctx, revision, relSlash, content); err != nil {
			return "", fmt.Errorf("pushed operation content verification failed: %w", err)
		}
		return revision, nil
	}
	abs := filepath.Join(w.Root, rel)
	rootClean := filepath.Clean(w.Root) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(abs)+string(os.PathSeparator), rootClean) {
		return "", errors.New("refusing Git path outside working repository")
	}
	if err = ensureRealDirectoryBelow(w.Root, filepath.Dir(rel)); err != nil {
		return "", fmt.Errorf("create AppConfig path: %w", err)
	}
	if info, statErr := os.Lstat(abs); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", errors.New("AppConfig worktree path must be a regular file")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("inspect AppConfig: %w", statErr)
	}
	current, readErr := os.ReadFile(abs)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return "", fmt.Errorf("read AppConfig: %w", readErr)
	}
	if !bytes.Equal(current, content) {
		tmp, err := os.CreateTemp(filepath.Dir(abs), ".app-*.yaml")
		if err != nil {
			return "", fmt.Errorf("create AppConfig temporary file: %w", err)
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName) //nolint:errcheck
		if err = tmp.Chmod(0o640); err == nil {
			_, err = tmp.Write(content)
		}
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return "", fmt.Errorf("write AppConfig: %w", err)
		}
		if err = os.Rename(tmpName, abs); err != nil {
			return "", fmt.Errorf("replace AppConfig: %w", err)
		}
	}
	// The worktree is a disposable writer-owned cache. Rebuild the index from
	// HEAD before staging so a crashed or tampered prior process cannot smuggle
	// an unrelated staged path into this operation's commit.
	if _, code, _ := w.gitStatus(ctx, "rev-parse", "--verify", "HEAD"); code == 0 {
		if _, err = w.git(ctx, "read-tree", "HEAD"); err != nil {
			return "", err
		}
	} else if _, err = w.git(ctx, "read-tree", "--empty"); err != nil {
		return "", err
	}
	blob, err := w.git(ctx, "hash-object", "-w", "--no-filters", "--", abs)
	if err != nil {
		return "", err
	}
	blob = strings.TrimSpace(blob)
	if blob == "" {
		return "", errors.New("Git did not return the staged AppConfig blob id")
	}
	if _, err = w.git(ctx, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+relSlash); err != nil {
		return "", err
	}
	_, diffCode, diffErr := w.gitStatus(ctx, "diff", "--cached", "--quiet", "--", relSlash)
	if diffCode != 0 && diffCode != 1 {
		return "", diffErr
	}
	if diffCode == 0 { // No diff means another commit already contains this exact config.
		rev, revErr := w.git(ctx, "log", "-1", "--format=%H", "--", relSlash)
		if revErr != nil {
			return "", errors.New("AppConfig unchanged but has no Git commit")
		}
		rev = strings.TrimSpace(rev)
		if revErr = w.verifyRevisionContent(ctx, rev, relSlash, content); revErr != nil {
			return "", fmt.Errorf("unchanged AppConfig blob verification failed: %w", revErr)
		}
		if err = w.push(ctx); err != nil {
			return "", err
		}
		rev, revErr = w.git(ctx, "log", "-1", "--format=%H", "--", relSlash)
		rev = strings.TrimSpace(rev)
		if revErr == nil {
			revErr = w.verifyRevisionContent(ctx, rev, relSlash, content)
		}
		return rev, revErr
	}
	message := fmt.Sprintf("deploy(%s): update %s\n\n%s", a.ID, a.Name, marker)
	tree, err := w.git(ctx, "write-tree")
	if err != nil {
		return "", err
	}
	tree = strings.TrimSpace(tree)
	commitArgs := []string{"commit-tree", tree, "-m", message}
	parent := ""
	if head, code, _ := w.gitStatus(ctx, "rev-parse", "--verify", "HEAD"); code == 0 {
		parent = strings.TrimSpace(head)
		commitArgs = append(commitArgs, "-p", parent)
	}
	commit, err := w.git(ctx, commitArgs...)
	if err != nil {
		return "", err
	}
	commit = strings.TrimSpace(commit)
	updateArgs := []string{"update-ref", "HEAD", commit}
	if parent != "" {
		updateArgs = append(updateArgs, parent)
	}
	if _, err = w.git(ctx, updateArgs...); err != nil {
		return "", err
	}
	if err = w.push(ctx); err != nil {
		return "", err
	}
	revision, err := w.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	revision = strings.TrimSpace(revision)
	if err = w.verifyRevisionContent(ctx, revision, relSlash, content); err != nil {
		return "", fmt.Errorf("committed AppConfig blob verification failed: %w", err)
	}
	return revision, nil
}

func ensureRealDirectoryBelow(root, relative string) error {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Git worktree root must be a real directory")
	}
	if relative == "." || filepath.IsAbs(relative) {
		return errors.New("Git subdirectory must be a non-empty relative path")
	}
	cleaned := filepath.Clean(relative)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return errors.New("Git subdirectory escapes the worktree")
	}
	current := root
	for _, component := range strings.Split(cleaned, string(os.PathSeparator)) {
		if component == "" || component == "." || component == ".." {
			return errors.New("Git subdirectory contains an invalid component")
		}
		current = filepath.Join(current, component)
		info, err = os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(current, 0o750); err != nil {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("Git subdirectory component must be a real directory")
		}
	}
	return nil
}

func (w *Writer) push(ctx context.Context) error {
	if w.Remote == "" {
		return nil
	}
	branch := w.Branch
	if branch == "" {
		branch = "main"
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := w.git(ctx, "push", w.Remote, "HEAD:refs/heads/"+branch); err == nil {
			return nil
		} else {
			last = err
		}
		if attempt < 2 {
			if err := w.syncRemote(ctx); err != nil {
				return fmt.Errorf("normal Git push rejected (%v) and safe synchronization failed: %w", last, err)
			}
		}
	}
	return fmt.Errorf("normal Git push rejected after bounded retries: %w", last)
}

func (w *Writer) operationRevision(ctx context.Context, marker string) (string, error) {
	if _, code, _ := w.gitStatus(ctx, "rev-parse", "--verify", "HEAD"); code != 0 {
		return "", nil
	}
	revision, err := w.git(ctx, "log", "-1", "--format=%H", "--fixed-strings", "--grep="+marker)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(revision), nil
}

func (w *Writer) verifyRevisionContent(ctx context.Context, revision, path string, expected []byte) error {
	if len(revision) != 40 && len(revision) != 64 {
		return errors.New("revision is not an exact object id")
	}
	for _, r := range revision {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return errors.New("revision is not a lowercase hexadecimal object id")
		}
	}
	if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "..") || strings.ContainsAny(path, "\x00\r\n") {
		return errors.New("AppConfig path is invalid")
	}
	actual, err := w.git(ctx, "cat-file", "blob", revision+":"+path)
	if err != nil {
		return err
	}
	if !bytes.Equal([]byte(actual), expected) {
		return errors.New("committed AppConfig bytes differ from the accepted operation snapshot")
	}
	return nil
}

func (w *Writer) syncRemote(ctx context.Context) error {
	if w.Remote == "" {
		return nil
	}
	timeout := w.SyncTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	syncCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	branch := w.Branch
	if branch == "" {
		branch = "main"
	}
	refs, err := w.git(syncCtx, "ls-remote", "--heads", w.Remote, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if strings.TrimSpace(refs) == "" {
		return nil
	}
	if _, err = w.git(syncCtx, "fetch", "--no-tags", w.Remote, "refs/heads/"+branch); err != nil {
		return err
	}
	head, headCode, _ := w.gitStatus(syncCtx, "rev-parse", "--verify", "HEAD")
	if headCode != 0 || strings.TrimSpace(head) == "" {
		_, err = w.git(syncCtx, "checkout", "-B", branch, "FETCH_HEAD")
		return err
	}
	_, remoteAncestor, ancestorErr := w.gitStatus(syncCtx, "merge-base", "--is-ancestor", "FETCH_HEAD", "HEAD")
	if remoteAncestor == 0 {
		return nil
	}
	if remoteAncestor != 1 {
		return ancestorErr
	}
	_, localAncestor, localErr := w.gitStatus(syncCtx, "merge-base", "--is-ancestor", "HEAD", "FETCH_HEAD")
	if localAncestor == 0 {
		_, err = w.git(syncCtx, "merge", "--ff-only", "FETCH_HEAD")
		return err
	}
	if localAncestor != 1 {
		return localErr
	}
	if _, err = w.git(syncCtx, "rebase", "FETCH_HEAD"); err != nil {
		_, abortErr := w.git(syncCtx, "rebase", "--abort")
		if abortErr != nil {
			return fmt.Errorf("safe rebase failed (%v) and abort failed: %w", err, abortErr)
		}
		return fmt.Errorf("safe rebase conflicted; worktree restored and no force push attempted: %w", err)
	}
	return nil
}

func (w *Writer) git(ctx context.Context, args ...string) (string, error) {
	stdout, code, err := w.gitStatus(ctx, args...)
	if code != 0 {
		return stdout, err
	}
	return stdout, nil
}

func (w *Writer) gitStatus(ctx context.Context, args ...string) (string, int, error) {
	if _, err := os.Lstat(filepath.Join(w.Root, ".git")); err == nil {
		if err = w.sanitizeLocalConfig(); err != nil {
			return "", -1, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", -1, fmt.Errorf("inspect Git metadata path: %w", err)
	}
	protocol := "never"
	if w.AllowLocalTransport {
		protocol = "always"
	}
	secureArgs := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "credential.helper=",
		"-c", "commit.gpgSign=false",
		"-c", "tag.gpgSign=false",
		"-c", "protocol.file.allow=" + protocol,
		"-c", "http.followRedirects=false",
	}
	secureArgs = append(secureArgs, args...)
	cmd := exec.CommandContext(ctx, "git", secureArgs...)
	cmd.Dir = w.Root
	authorName, authorEmail, _ := w.authorIdentity()
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + w.Root,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM=0",
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(w.Root),
		"GIT_AUTHOR_NAME=" + authorName,
		"GIT_AUTHOR_EMAIL=" + authorEmail,
		"GIT_COMMITTER_NAME=" + authorName,
		"GIT_COMMITTER_EMAIL=" + authorEmail,
	}
	if w.UseCredentialFiles {
		remote, parseErr := url.Parse(w.Remote)
		if parseErr != nil || remote.Host == "" {
			return "", -1, errors.New("Git credential host is invalid")
		}
		cmd.Env = append(cmd.Env,
			"GIT_ASKPASS="+DefaultAskPassExecutable,
			"GIT_ASKPASS_REQUIRE=force",
			askPassApprovedHostEnv+"="+remote.Host,
		)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		code := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		return stdout.String(), code, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), 0, nil
}

func RenderAppConfig(p domain.Project, e domain.Environment, a domain.Application, d domain.Deployment) ([]byte, error) {
	return renderAppConfig(p, e, a, d, "traefik")
}

func renderAppConfig(p domain.Project, e domain.Environment, a domain.Application, d domain.Deployment, ingressClass string) ([]byte, error) {
	if a.ID == "" || e.ID == "" || p.ID == "" {
		return nil, errors.New("AppConfig requires immutable project, environment and application IDs")
	}
	parts := strings.Split(d.Image, "@")
	if len(parts) != 2 || !strings.HasPrefix(parts[1], "sha256:") {
		return nil, errors.New("image must be an immutable repository@sha256 digest")
	}
	var b strings.Builder
	line := func(indent int, key, value string) {
		b.WriteString(strings.Repeat(" ", indent))
		b.WriteString(key)
		b.WriteString(": ")
		b.WriteString(yq(value))
		b.WriteByte('\n')
	}
	b.WriteString("apiVersion: config.kuberploy.io/v1alpha1\nkind: AppConfig\nmetadata:\n")
	line(2, "id", a.ID)
	line(2, "name", a.Slug)
	b.WriteString("spec:\n")
	line(2, "projectId", p.ID)
	line(2, "applicationId", a.ID)
	line(2, "environmentId", e.ID)
	b.WriteString("  delivery:\n    mode: image\n")
	if d.RegistryPull != nil {
		if !d.RegistryPull.Valid() {
			return nil, errors.New("registry pull reference is invalid")
		}
		b.WriteString("    registryPull:\n")
		line(6, "targetId", d.RegistryPull.TargetID)
		line(6, "profileName", d.RegistryPull.ProfileName)
		b.WriteString("      profileRevision: ")
		b.WriteString(strconv.FormatInt(d.RegistryPull.ProfileRevision, 10))
		b.WriteByte('\n')
	}
	b.WriteString("    release:\n")
	line(6, "repository", parts[0])
	line(6, "digest", parts[1])
	runtime := domain.RuntimeForCreateDeployment(domain.CreateDeployment{Replicas: d.Replicas, Port: d.Port, Environment: d.Environment, Runtime: d.Runtime})
	if problems := domain.ValidateWorkloadRuntime(runtime); len(problems) > 0 {
		return nil, fmt.Errorf("invalid workload runtime at %s: %s", problems[0].Pointer, problems[0].Detail)
	}
	runtimeJSON, err := json.Marshal(runtime)
	if err != nil {
		return nil, fmt.Errorf("encode workload runtime: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(runtimeJSON))
	decoder.UseNumber()
	var runtimeValue map[string]any
	if err = decoder.Decode(&runtimeValue); err != nil {
		return nil, fmt.Errorf("decode workload runtime: %w", err)
	}
	b.WriteString("  runtime:\n")
	writeYAMLMap(&b, 4, runtimeValue)
	if d.Route != nil {
		routePort := ""
		for _, port := range runtime.Ports {
			if port.Protocol == "TCP" {
				routePort = port.Name
				break
			}
		}
		if routePort == "" {
			return nil, errors.New("public route requires at least one TCP runtime port")
		}
		ingressClass = strings.TrimSpace(ingressClass)
		if ingressClass == "" {
			ingressClass = "traefik"
		}
		sum := sha256.Sum256([]byte(strings.ToLower(d.Route.Hostname) + "\x00" + d.Route.PathPrefix))
		b.WriteString("  routes:\n    - id: route-")
		b.WriteString(fmt.Sprintf("%x", sum[:5]))
		b.WriteByte('\n')
		line(6, "host", strings.ToLower(d.Route.Hostname))
		line(6, "path", d.Route.PathPrefix)
		line(6, "ingressClassName", ingressClass)
		line(6, "port", routePort)
		if d.Route.DNSMode == "sslip" {
			b.WriteString("      dns:\n        mode: sslip\n")
		} else if d.Route.DNSMode != "" && d.Route.DNSMode != "manual" {
			return nil, errors.New("route DNS mode is invalid")
		}
		b.WriteString("      tls:\n        mode: httpOnly\n")
	}
	return []byte(b.String()), nil
}

func yq(v string) string { return fmt.Sprintf("%q", v) }

func writeYAMLMap(b *strings.Builder, indent int, values map[string]any) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		b.WriteString(strings.Repeat(" ", indent))
		b.WriteString(key)
		b.WriteByte(':')
		writeYAMLChild(b, indent, values[key])
	}
}

func writeYAMLChild(b *strings.Builder, indent int, value any) {
	switch typed := value.(type) {
	case map[string]any:
		b.WriteByte('\n')
		writeYAMLMap(b, indent+2, typed)
	case []any:
		b.WriteByte('\n')
		for _, item := range typed {
			b.WriteString(strings.Repeat(" ", indent+2))
			b.WriteByte('-')
			switch nested := item.(type) {
			case map[string]any:
				b.WriteByte('\n')
				writeYAMLMap(b, indent+4, nested)
			case []any:
				writeYAMLChild(b, indent+2, nested)
			default:
				b.WriteByte(' ')
				writeYAMLScalar(b, nested)
				b.WriteByte('\n')
			}
		}
	default:
		b.WriteByte(' ')
		writeYAMLScalar(b, typed)
		b.WriteByte('\n')
	}
}

func writeYAMLScalar(b *strings.Builder, value any) {
	switch typed := value.(type) {
	case string:
		b.WriteString(yq(typed))
	case json.Number:
		b.WriteString(typed.String())
	case bool:
		b.WriteString(fmt.Sprintf("%t", typed))
	case nil:
		b.WriteString("null")
	default:
		b.WriteString(yq(fmt.Sprint(typed)))
	}
}
