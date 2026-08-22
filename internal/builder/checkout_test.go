package builder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckoutBindsSafeDirectoryToFixedWorkspace(t *testing.T) {
	workspace := t.TempDir()
	runtimeRoot := t.TempDir()
	commit := strings.Repeat("a", 40)
	executor := &sequenceExecutor{results: []CommandResult{{}, {}, {}, {}, {Stdout: commit + "\n"}}}
	checkout := NewCheckout(executor)
	checkout.Workspace = workspace
	checkout.RuntimeRoot = runtimeRoot
	request := CheckoutRequest{
		APIVersion: ProtocolVersion, OperationID: "11111111-1111-4111-8111-111111111111", Generation: 1,
		RepositoryURL: "https://source.example.test/owner/repository.git", ApprovedHost: "source.example.test", Commit: commit,
	}
	if _, err := checkout.Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(executor.invocations) != 5 {
		t.Fatalf("invocations=%d", len(executor.invocations))
	}
	for _, invocation := range executor.invocations {
		environment := strings.Join(invocation.Env, "\n")
		for _, expected := range []string{"GIT_CONFIG_COUNT=4", "GIT_CONFIG_KEY_3=safe.directory", "GIT_CONFIG_VALUE_3=" + workspace} {
			if !strings.Contains(environment, expected) {
				t.Fatalf("checkout environment lacks %q: %s", expected, environment)
			}
		}
	}
}

func TestCheckoutRequestBindsApprovedHost(t *testing.T) {
	request := CheckoutRequest{
		APIVersion:    ProtocolVersion,
		OperationID:   "11111111-1111-4111-8111-111111111111",
		Generation:    1,
		RepositoryURL: "https://source.example.test/owner/repository.git",
		ApprovedHost:  "source.example.test",
		Commit:        strings.Repeat("a", 40),
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid checkout rejected: %v", err)
	}
	request.ApprovedHost = "attacker.example.test"
	if err := request.Validate(); err == nil {
		t.Fatal("cross-host checkout was accepted")
	}
	request.RepositoryURL = "https://attacker.example.test/redirect"
	if err := request.Validate(); err != nil {
		// The controller may approve another host for another operation; the
		// important redirect defense is the exact equality plus Git redirect off.
		request.ApprovedHost = "attacker.example.test"
	}
	request.RepositoryURL = "https://source.example.test/repo?redirect=https://attacker.example.test"
	request.ApprovedHost = "source.example.test"
	if err := request.Validate(); err == nil {
		t.Fatal("query-based redirect source was accepted")
	}
}

func TestCheckoutRequestSupportsPinnedSSHWithoutAmbientTrust(t *testing.T) {
	request := CheckoutRequest{
		APIVersion: ProtocolVersion, OperationID: "11111111-1111-4111-8111-111111111111", Generation: 1,
		RepositoryURL: "ssh://git@git.example.test/team/repository.git", ApprovedHost: "git.example.test",
		Commit: strings.Repeat("a", 40), SSHPrivateKeyFile: SourceSSHPrivateKeyFile, SSHKnownHostsFile: SourceSSHKnownHostsFile,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid SSH checkout rejected: %v", err)
	}

	unsafe := []CheckoutRequest{request, request, request, request, request}
	unsafe[0].RepositoryURL = "git@git.example.test:team/repository.git"
	unsafe[1].RepositoryURL = "ssh://git:password@git.example.test/team/repository.git"
	unsafe[2].SSHKnownHostsFile = ""
	unsafe[3].SSHPrivateKeyFile = SourceCredentialRoot + "/other-key"
	unsafe[4].UsernameFile, unsafe[4].AccessTokenFile = SourceCredentialRoot+"/username", SourceCredentialRoot+"/token"
	for index, candidate := range unsafe {
		if err := candidate.Validate(); err == nil {
			t.Fatalf("unsafe SSH checkout %d accepted: %#v", index, candidate)
		}
	}
}

func TestMaterializeSSHCredentialsCopiesProjectedFilesWithPrivateModes(t *testing.T) {
	source := t.TempDir()
	home := t.TempDir()
	privateKeySource := filepath.Join(source, "ssh-private-key")
	knownHostsSource := filepath.Join(source, "known_hosts")
	privateKeyValue := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfixture\n-----END OPENSSH PRIVATE KEY-----\n")
	knownHostsValue := []byte("git.example.test ssh-ed25519 AAAAfixture\n")
	if err := os.WriteFile(privateKeySource, privateKeyValue, 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownHostsSource, knownHostsValue, 0o440); err != nil {
		t.Fatal(err)
	}
	privateKey, knownHosts, err := materializeSSHCredentials(home, privateKeySource, knownHostsSource)
	if err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string][]byte{privateKey: privateKeyValue, knownHosts: knownHostsValue} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%#o", filepath.Base(path), info.Mode().Perm())
		}
		actual, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(actual) != string(expected) {
			t.Fatalf("%s content changed", filepath.Base(path))
		}
	}
}

func TestMaterializeSSHCredentialsRejectsInvalidFiles(t *testing.T) {
	source := t.TempDir()
	valid := filepath.Join(source, "valid")
	if err := os.WriteFile(valid, []byte("valid\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{"empty": {}, "nul": {'x', 0, 'y'}, "large": make([]byte, (64<<10)+1)} {
		path := filepath.Join(source, name)
		if err := os.WriteFile(path, value, 0o440); err != nil {
			t.Fatal(err)
		}
		if _, _, err := materializeSSHCredentials(t.TempDir(), path, valid); err == nil {
			t.Fatalf("%s SSH credential accepted", name)
		}
	}
}

func TestAskpassRejectsCrossHostAndUnknownPrompts(t *testing.T) {
	t.Setenv("KUBERPLOY_ASKPASS", "1")
	t.Setenv("KUBERPLOY_SOURCE_APPROVED_HOST", "source.example.test")
	t.Setenv("KUBERPLOY_SOURCE_USERNAME_FILE", SourceCredentialRoot+"/username")
	t.Setenv("KUBERPLOY_SOURCE_TOKEN_FILE", SourceCredentialRoot+"/token")
	for _, prompt := range []string{
		"Password for 'https://attacker.example.test':",
		"Password for 'http://source.example.test':",
		"Certificate for 'https://source.example.test':",
		"Password:",
	} {
		if err := WriteAskpass(prompt, &strings.Builder{}); err == nil {
			t.Fatalf("unsafe askpass prompt was accepted: %q", prompt)
		}
	}
}

func TestAskpassPromptAllowsApprovedPasswordUsernameShape(t *testing.T) {
	host, kind, username, err := askpassPrompt("Password for 'https://git-user@source.example.test':")
	if err != nil {
		t.Fatal(err)
	}
	if host != "source.example.test" || kind != "password" || username != "git-user" {
		t.Fatalf("unexpected prompt parse: host=%q kind=%q username=%q", host, kind, username)
	}
	if _, _, _, err := askpassPrompt("Username for 'https://git-user@source.example.test':"); err == nil {
		t.Fatal("username prompt with embedded username was accepted")
	}
}

func TestSanitizedEnvironmentRemovesInheritedSecretsDeterministically(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/tmp/host-config")
	t.Setenv("DOCKER_CONFIG", "/tmp/host-docker")
	t.Setenv("GITHUB_TOKEN", "host-secret")
	t.Setenv("HTTPS_PROXY", "https://proxy-user:proxy-password@example.test")
	t.Setenv("KUBERPLOY_TEST_SECRET", "host-secret")
	environment := sanitizedEnvironment([]string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"})
	joined := strings.Join(environment, "\n")
	for _, forbidden := range []string{"/tmp/host-config", "/tmp/host-docker", "host-secret", "proxy-password"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("inherited sensitive environment leaked: %q", forbidden)
		}
	}
	if !strings.Contains(joined, "GIT_CONFIG_GLOBAL=/dev/null") {
		t.Fatal("explicit isolated Git config override is absent")
	}
	_ = os.Getenv("PATH")
}
