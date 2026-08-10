package gitprojection

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitAskPassBrokerAuthenticatesPrivateHTTPSWithoutCredentialLeaks(t *testing.T) {
	root := t.TempDir()
	helper := buildTestAskPass(t, root)
	repositoryRoot := filepath.Join(root, "repositories")
	heads := map[string]string{
		"owner/alpha.git": seedDumbHTTPRepository(t, repositoryRoot, "owner/alpha.git", "alpha\n"),
		"owner/bravo.git": seedDumbHTTPRepository(t, repositoryRoot, "owner/bravo.git", "bravo\n"),
		"owner/slow.git":  seedDumbHTTPRepository(t, repositoryRoot, "owner/slow.git", "slow\n"),
	}
	tokens := map[string]string{
		"owner/alpha.git": "ghs_alpha_" + strings.Repeat("A", 32),
		"owner/bravo.git": "ghs_bravo_" + strings.Repeat("B", 32),
		"owner/slow.git":  "ghs_slow_" + strings.Repeat("S", 32),
	}
	endpoint := newPrivateGitHTTPSEndpoint(t, root, repositoryRoot, tokens)

	// Both requests must authenticate before either repository is served. This
	// keeps the two real Git/helper/broker paths live concurrently and detects a
	// socket or credential crossing between them.
	concurrentContext, cancelConcurrent := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelConcurrent()
	concurrentRepositories := []string{"owner/alpha.git", "owner/bravo.git"}
	var invocations []*testGitInvocation
	var sessions []*testCredentialSession
	for _, repository := range concurrentRepositories {
		session := newTestCredentialSession(t, root, tokens[repository])
		sessions = append(sessions, session)
		invocation := newTestGitInvocation(t, concurrentContext, root, helper, endpoint, session, repository)
		assertInvocationHasNoCredential(t, invocation, tokens)
		invocations = append(invocations, invocation)
	}
	if sessions[0].path == sessions[1].path || sessions[0].directory == sessions[1].directory {
		t.Fatal("concurrent credential sessions reused a broker capability")
	}

	type repositoryResult struct {
		repository string
		result     testGitResult
	}
	results := make(chan repositoryResult, len(invocations))
	for index, invocation := range invocations {
		repository := concurrentRepositories[index]
		go func() { results <- repositoryResult{repository: repository, result: invocation.run()} }()
	}
	for range invocations {
		completed := <-results
		result := completed.result
		assertResultHasNoCredential(t, result, tokens)
		if result.err != nil {
			t.Fatalf("authenticated Git request failed: %v: %s", result.err, strings.TrimSpace(result.stderr))
		}
		fields := strings.Fields(result.stdout)
		if len(fields) != 2 || fields[0] != heads[completed.repository] || fields[1] != "refs/heads/main" {
			t.Fatalf("unexpected ls-remote output: %q", result.stdout)
		}
	}
	for _, session := range sessions {
		session.closeAndAssert(t)
	}
	if endpoint.wrongCredential.Load() {
		t.Fatal("an HTTPS repository received another broker's credential")
	}

	// Hold the authenticated response open until Git's context deadline. The
	// broker must still close, erase its owned byte copy, and leave no socket.
	timeoutSession := newTestCredentialSession(t, root, tokens["owner/slow.git"])
	timeoutContext, cancelTimeout := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelTimeout()
	timeoutInvocation := newTestGitInvocation(t, timeoutContext, root, helper, endpoint, timeoutSession, "owner/slow.git")
	assertInvocationHasNoCredential(t, timeoutInvocation, tokens)
	timeoutResult := timeoutInvocation.run()
	assertResultHasNoCredential(t, timeoutResult, tokens)
	if timeoutResult.err == nil || timeoutContext.Err() != context.DeadlineExceeded {
		t.Fatalf("blocked Git request did not reach its deadline: context=%v error=%v", timeoutContext.Err(), timeoutResult.err)
	}
	select {
	case <-endpoint.slowAuthorized:
	default:
		t.Fatal("Git timed out before authenticating through askpass")
	}
	timeoutSession.closeAndAssert(t)
	endpoint.releaseBlockedRequest()

	if endpoint.proxyViolation.Load() || endpoint.wrongCredential.Load() {
		t.Fatal("the private HTTPS fixture observed an invalid proxy target or credential")
	}
	assertCredentialAbsentFromFiles(t, root, tokens)
}

type testCredentialSession struct {
	credential GitCredential
	password   []byte
	broker     *askPassBroker
	path       string
	directory  string
	closed     bool
}

func newTestCredentialSession(t *testing.T, root, token string) *testCredentialSession {
	t.Helper()
	password := []byte(token)
	session := &testCredentialSession{
		credential: GitCredential{
			Username:  []byte(gitHubAppUsername),
			Password:  password,
			ExpiresAt: time.Now().UTC().Add(time.Hour),
		},
		password: password,
	}
	broker, err := startAskPassBroker(root, &session.credential)
	if err != nil {
		t.Fatal(err)
	}
	session.broker, session.path, session.directory = broker, broker.path, broker.directory
	t.Cleanup(func() { session.closeAndAssert(t) })

	info, err := os.Stat(session.directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("credential broker directory is not private: mode=%v error=%v", infoMode(info), err)
	}
	entries, err := os.ReadDir(session.directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "broker.sock" || entries[0].Type()&os.ModeSocket == 0 {
		t.Fatalf("credential broker persisted something other than its socket: entries=%d error=%v", len(entries), err)
	}
	return session
}

func (s *testCredentialSession) closeAndAssert(t *testing.T) {
	t.Helper()
	if s == nil || s.closed {
		return
	}
	s.closed = true
	s.broker.close()
	s.credential.clear()
	if _, err := os.Lstat(s.path); !os.IsNotExist(err) {
		t.Errorf("credential broker socket survived close: %v", err)
	}
	if _, err := os.Lstat(s.directory); !os.IsNotExist(err) {
		t.Errorf("credential broker directory survived close: %v", err)
	}
	if len(s.password) == 0 || !allZero(s.password) || s.credential.Username != nil || s.credential.Password != nil || !s.credential.ExpiresAt.IsZero() {
		t.Error("credential broker retained its owned credential copy")
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode()
}

func allZero(value []byte) bool {
	for _, character := range value {
		if character != 0 {
			return false
		}
	}
	return true
}

type testGitInvocation struct {
	command *exec.Cmd
	stdout  bytes.Buffer
	stderr  bytes.Buffer
}

type testGitResult struct {
	stdout string
	stderr string
	err    error
}

func newTestGitInvocation(
	t *testing.T,
	ctx context.Context,
	root, helper string,
	endpoint *privateGitHTTPSEndpoint,
	session *testCredentialSession,
	repository string,
) *testGitInvocation {
	t.Helper()
	mirror := filepath.Join(root, "mirrors", strings.TrimSuffix(filepath.Base(repository), ".git")+".git")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mirror); os.IsNotExist(err) {
		runFixtureGit(t, root, "init", "--bare", mirror)
	} else if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--git-dir=" + mirror,
		"-c", "credential.helper=",
		"-c", "http.followRedirects=false",
		"-c", "http.proxy=" + endpoint.proxy.URL,
		"-c", "http.sslCAInfo=" + endpoint.caPath,
		"-c", "http.version=HTTP/1.1",
		"ls-remote", "--refs", "https://github.com/" + repository, "refs/heads/main",
	}
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = root
	command.WaitDelay = 2 * time.Second
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + root,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM=0",
		"GIT_ASKPASS=" + helper,
		"GIT_ASKPASS_REQUIRE=force",
		GitAskPassSocketEnv + "=" + session.path,
	}
	invocation := &testGitInvocation{command: command}
	command.Stdout, command.Stderr = &invocation.stdout, &invocation.stderr
	return invocation
}

func (i *testGitInvocation) run() testGitResult {
	err := i.command.Run()
	return testGitResult{stdout: i.stdout.String(), stderr: i.stderr.String(), err: err}
}

func assertInvocationHasNoCredential(t *testing.T, invocation *testGitInvocation, tokens map[string]string) {
	t.Helper()
	for _, value := range append(append([]string(nil), invocation.command.Args...), invocation.command.Env...) {
		assertStringHasNoCredential(t, "Git argv/environment", value, tokens)
	}
}

func assertResultHasNoCredential(t *testing.T, result testGitResult, tokens map[string]string) {
	t.Helper()
	assertStringHasNoCredential(t, "Git stdout", result.stdout, tokens)
	assertStringHasNoCredential(t, "Git stderr", result.stderr, tokens)
	if result.err != nil {
		assertStringHasNoCredential(t, "Git error", result.err.Error(), tokens)
	}
}

func assertStringHasNoCredential(t *testing.T, location, value string, tokens map[string]string) {
	t.Helper()
	for _, token := range tokens {
		if strings.Contains(value, token) {
			t.Errorf("credential appeared in %s", location)
		}
	}
}

func assertCredentialAbsentFromFiles(t *testing.T, root string, tokens map[string]string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		value, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, token := range tokens {
			if bytes.Contains(value, []byte(token)) {
				return fmt.Errorf("credential was persisted in regular file %s", path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type privateGitHTTPSEndpoint struct {
	repositoryRoot  string
	tokens          map[string]string
	server          *httptest.Server
	proxy           *httptest.Server
	caPath          string
	barrier         map[string]struct{}
	barrierSeen     map[string]struct{}
	barrierReady    chan struct{}
	barrierMu       sync.Mutex
	slowAuthorized  chan struct{}
	slowRelease     chan struct{}
	slowOnce        sync.Once
	slowReleaseOnce sync.Once
	wrongCredential atomic.Bool
	proxyViolation  atomic.Bool
}

func newPrivateGitHTTPSEndpoint(t *testing.T, root, repositoryRoot string, tokens map[string]string) *privateGitHTTPSEndpoint {
	t.Helper()
	certificate, certificateAuthority := testGitHubCertificate(t)
	caPath := filepath.Join(root, "github-test-ca.pem")
	if err := os.WriteFile(caPath, certificateAuthority, 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint := &privateGitHTTPSEndpoint{
		repositoryRoot: repositoryRoot,
		tokens:         tokens,
		caPath:         caPath,
		barrier: map[string]struct{}{
			"owner/alpha.git": {},
			"owner/bravo.git": {},
		},
		barrierSeen:    make(map[string]struct{}),
		barrierReady:   make(chan struct{}),
		slowAuthorized: make(chan struct{}),
		slowRelease:    make(chan struct{}),
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(endpoint.serveGit))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	endpoint.server = server
	endpoint.proxy = newGitConnectProxy(endpoint, server.Listener.Addr().String())
	t.Cleanup(func() {
		endpoint.releaseBlockedRequest()
		endpoint.proxy.Close()
		endpoint.server.Close()
	})
	return endpoint
}

func (e *privateGitHTTPSEndpoint) serveGit(response http.ResponseWriter, request *http.Request) {
	repository := repositoryFromGitPath(request.URL.Path)
	expected, known := e.tokens[repository]
	if !known {
		http.NotFound(response, request)
		return
	}
	username, password, authenticated := request.BasicAuth()
	if !authenticated {
		response.Header().Set("WWW-Authenticate", `Basic realm="kuberploy-test"`)
		response.WriteHeader(http.StatusUnauthorized)
		return
	}
	if username != gitHubAppUsername || password != expected {
		e.wrongCredential.Store(true)
		response.WriteHeader(http.StatusForbidden)
		return
	}
	if _, synchronized := e.barrier[repository]; synchronized {
		e.barrierMu.Lock()
		e.barrierSeen[repository] = struct{}{}
		if len(e.barrierSeen) == len(e.barrier) {
			select {
			case <-e.barrierReady:
			default:
				close(e.barrierReady)
			}
		}
		e.barrierMu.Unlock()
		select {
		case <-e.barrierReady:
		case <-request.Context().Done():
			return
		}
	}
	if repository == "owner/slow.git" {
		e.slowOnce.Do(func() { close(e.slowAuthorized) })
		select {
		case <-e.slowRelease:
		case <-request.Context().Done():
			return
		}
	}
	http.FileServer(http.Dir(e.repositoryRoot)).ServeHTTP(response, request)
}

func (e *privateGitHTTPSEndpoint) releaseBlockedRequest() {
	e.slowReleaseOnce.Do(func() { close(e.slowRelease) })
}

func repositoryFromGitPath(path string) string {
	relative := strings.TrimPrefix(path, "/")
	index := strings.Index(relative, ".git/")
	if index < 0 {
		return ""
	}
	return relative[:index+len(".git")]
}

func newGitConnectProxy(endpoint *privateGitHTTPSEndpoint, targetAddress string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect || request.Host != "github.com:443" {
			endpoint.proxyViolation.Store(true)
			response.WriteHeader(http.StatusForbidden)
			return
		}
		target, err := net.DialTimeout("tcp", targetAddress, time.Second)
		if err != nil {
			endpoint.proxyViolation.Store(true)
			response.WriteHeader(http.StatusBadGateway)
			return
		}
		hijacker, ok := response.(http.Hijacker)
		if !ok {
			endpoint.proxyViolation.Store(true)
			_ = target.Close()
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		client, buffered, err := hijacker.Hijack()
		if err != nil {
			endpoint.proxyViolation.Store(true)
			_ = target.Close()
			return
		}
		if _, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil || buffered.Flush() != nil {
			endpoint.proxyViolation.Store(true)
			_ = client.Close()
			_ = target.Close()
			return
		}
		requestDone := make(chan struct{})
		go func() {
			_, _ = io.Copy(target, buffered)
			_ = target.Close()
			close(requestDone)
		}()
		_, _ = io.Copy(client, target)
		_ = client.Close()
		_ = target.Close()
		<-requestDone
	}))
}

func testGitHubCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	now := time.Now().UTC()
	caPrivate, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Kuberploy test Git CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caPrivate.PublicKey, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	leafPrivate, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "github.com"},
		DNSNames:     []string{"github.com"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafPrivate.PublicKey, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := x509.MarshalPKCS8PrivateKey(leafPrivate)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: leafKey})
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func buildTestAskPass(t *testing.T, root string) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate Git credential integration test")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	binary := filepath.Join(root, "kuberploy-git-askpass")
	command := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/kuberploy-git-askpass")
	command.Dir = repositoryRoot
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build shipped Git askpass helper: %v: %s", err, output)
	}
	return binary
}

func seedDumbHTTPRepository(t *testing.T, root, repository, content string) string {
	t.Helper()
	worktree := t.TempDir()
	runFixtureGit(t, worktree, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, worktree, "add", "README.md")
	runFixtureGit(t, worktree, "commit", "-m", "seed private HTTPS fixture")
	head := runFixtureGit(t, worktree, "rev-parse", "HEAD")
	bare := filepath.Join(root, filepath.FromSlash(repository))
	if err := os.MkdirAll(filepath.Dir(bare), 0o750); err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, root, "clone", "--bare", worktree, bare)
	runFixtureGit(t, root, "--git-dir="+bare, "update-server-info")
	return head
}

func runFixtureGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + directory,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Kuberploy Test",
		"GIT_AUTHOR_EMAIL=test@kuberploy.invalid",
		"GIT_COMMITTER_NAME=Kuberploy Test",
		"GIT_COMMITTER_EMAIL=test@kuberploy.invalid",
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %s failed: %v: %s", args[0], err, output)
	}
	return strings.TrimSpace(string(output))
}
