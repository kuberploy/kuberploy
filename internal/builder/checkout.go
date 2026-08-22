package builder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Checkout struct {
	Executor    CommandExecutor
	GitBinary   string
	AgentPath   string
	Workspace   string
	RuntimeRoot string
}

func NewCheckout(executor CommandExecutor) *Checkout {
	return &Checkout{
		Executor:    executor,
		GitBinary:   "git",
		AgentPath:   "/usr/local/bin/kuberploy-build-agent",
		Workspace:   DefaultCheckoutRoot,
		RuntimeRoot: "/result",
	}
}

func (c *Checkout) Run(ctx context.Context, request CheckoutRequest) (CheckoutResult, error) {
	if err := request.Validate(); err != nil {
		return CheckoutResult{}, err
	}
	if c.Executor == nil {
		return CheckoutResult{}, errors.New("command executor is required")
	}
	if !filepath.IsAbs(c.Workspace) || filepath.Clean(c.Workspace) != c.Workspace || c.Workspace == "/" {
		return CheckoutResult{}, errors.New("workspace must be a clean non-root absolute path")
	}
	entries, err := os.ReadDir(c.Workspace)
	if err != nil {
		return CheckoutResult{}, fmt.Errorf("read checkout workspace: %w", err)
	}
	if len(entries) != 0 {
		return CheckoutResult{}, errors.New("checkout workspace must be empty")
	}
	home, err := os.MkdirTemp(c.RuntimeRoot, ".checkout-home-*")
	if err != nil {
		return CheckoutResult{}, errors.New("create isolated Git home")
	}
	if err := os.Chmod(home, 0o700); err != nil {
		_ = os.RemoveAll(home)
		return CheckoutResult{}, errors.New("secure isolated Git home")
	}
	defer os.RemoveAll(home)
	environment := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "xdg"),
		"GIT_CONFIG_COUNT=4",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=http.followRedirects",
		"GIT_CONFIG_VALUE_1=false",
		"GIT_CONFIG_KEY_2=protocol.file.allow",
		"GIT_CONFIG_VALUE_2=never",
		"GIT_CONFIG_KEY_3=safe.directory",
		"GIT_CONFIG_VALUE_3=" + c.Workspace,
	}
	if request.AccessTokenFile != "" {
		environment = append(environment,
			"GIT_ASKPASS="+c.AgentPath,
			"KUBERPLOY_ASKPASS=1",
			"KUBERPLOY_SOURCE_USERNAME_FILE="+request.UsernameFile,
			"KUBERPLOY_SOURCE_TOKEN_FILE="+request.AccessTokenFile,
			"KUBERPLOY_SOURCE_APPROVED_HOST="+request.ApprovedHost,
		)
	}
	if request.SSHPrivateKeyFile != "" {
		privateKey, knownHosts, err := materializeSSHCredentials(home, request.SSHPrivateKeyFile, request.SSHKnownHostsFile)
		if err != nil {
			return CheckoutResult{}, err
		}
		environment = append(environment,
			"GIT_SSH_COMMAND=ssh -F /dev/null -i "+privateKey+" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile="+knownHosts+" -o GlobalKnownHostsFile=/dev/null",
			"GIT_SSH_VARIANT=ssh",
		)
	}
	commands := [][]string{
		{c.GitBinary, "init", "--quiet", c.Workspace},
		{c.GitBinary, "-C", c.Workspace, "remote", "add", "origin", request.RepositoryURL},
		{c.GitBinary, "-C", c.Workspace, "fetch", "--quiet", "--no-tags", "--depth=1", "origin", request.Commit},
		{c.GitBinary, "-C", c.Workspace, "checkout", "--quiet", "--detach", "FETCH_HEAD"},
	}
	for index, argv := range commands {
		if _, err := c.Executor.Execute(ctx, Invocation{Argv: argv, Env: environment}); err != nil {
			return CheckoutResult{}, commandError(fmt.Sprintf("checkout step %d", index+1), err)
		}
	}
	verified, err := c.Executor.Execute(ctx, Invocation{
		Argv: []string{c.GitBinary, "-C", c.Workspace, "rev-parse", "--verify", "HEAD^{commit}"},
		Env:  environment,
	})
	if err != nil {
		return CheckoutResult{}, commandError("verify checkout commit", err)
	}
	if strings.TrimSpace(verified.Stdout) != request.Commit {
		return CheckoutResult{}, errors.New("checkout commit verification failed")
	}
	gitDirectory := filepath.Join(c.Workspace, ".git")
	if err := os.RemoveAll(gitDirectory); err != nil {
		return CheckoutResult{}, fmt.Errorf("scrub checkout metadata: %w", err)
	}
	return CheckoutResult{
		APIVersion:  ProtocolVersion,
		OperationID: request.OperationID,
		Generation:  request.Generation,
		Status:      "Succeeded",
		Commit:      request.Commit,
	}, nil
}

func materializeSSHCredentials(home, privateKeySource, knownHostsSource string) (string, string, error) {
	privateKey, err := readSSHCredentialFile(privateKeySource)
	if err != nil {
		return "", "", errors.New("read SSH private key")
	}
	defer zeroBytes(privateKey)
	knownHosts, err := readSSHCredentialFile(knownHostsSource)
	if err != nil {
		return "", "", errors.New("read SSH known-hosts file")
	}
	defer zeroBytes(knownHosts)
	privateKeyPath := filepath.Join(home, "ssh-private-key")
	knownHostsPath := filepath.Join(home, "known_hosts")
	if err := writePrivateFile(privateKeyPath, privateKey); err != nil {
		return "", "", errors.New("secure SSH private key")
	}
	if err := writePrivateFile(knownHostsPath, knownHosts); err != nil {
		_ = os.Remove(privateKeyPath)
		return "", "", errors.New("secure SSH known-hosts file")
	}
	return privateKeyPath, knownHostsPath, nil
}

func readSSHCredentialFile(path string) ([]byte, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(value) == 0 || len(value) > 64<<10 || bytes.IndexByte(value, 0) >= 0 {
		zeroBytes(value)
		return nil, errors.New("SSH credential file is empty, too large, or contains NUL")
	}
	return value, nil
}

func writePrivateFile(path string, value []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(value); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err = file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// WriteAskpass is called only when Git starts the agent through GIT_ASKPASS.
// The credential itself is read from its mounted file and written to stdout;
// it is never copied into an argv entry or environment value.
func WriteAskpass(prompt string, output io.Writer) error {
	if os.Getenv("KUBERPLOY_ASKPASS") != "1" {
		return errors.New("askpass mode is not enabled")
	}
	host, promptKind, promptUsername, err := askpassPrompt(prompt)
	if err != nil || !strings.EqualFold(host, os.Getenv("KUBERPLOY_SOURCE_APPROVED_HOST")) {
		return errors.New("askpass prompt is not for the approved source host")
	}
	path := os.Getenv("KUBERPLOY_SOURCE_TOKEN_FILE")
	if promptKind == "username" {
		path = os.Getenv("KUBERPLOY_SOURCE_USERNAME_FILE")
	} else if promptUsername != "" {
		expectedUsername, err := readCredentialFile(os.Getenv("KUBERPLOY_SOURCE_USERNAME_FILE"))
		if err != nil {
			return errors.New("askpass username could not be verified")
		}
		matches := bytes.Equal(expectedUsername, []byte(promptUsername))
		zeroBytes(expectedUsername)
		if !matches {
			return errors.New("askpass prompt username does not match the approved source credential")
		}
	}
	if err := validateConfinedAbsolute(SourceCredentialRoot, path); err != nil {
		return errors.New("askpass credential path is invalid")
	}
	value, err := readCredentialFile(path)
	if err != nil {
		return errors.New("askpass credential could not be read")
	}
	defer zeroBytes(value)
	if _, err := output.Write(append(value, '\n')); err != nil {
		return errors.New("askpass credential could not be written")
	}
	return nil
}

func askpassPrompt(prompt string) (string, string, string, error) {
	lower := strings.ToLower(prompt)
	kind := ""
	if strings.HasPrefix(lower, "username for ") {
		kind = "username"
	} else if strings.HasPrefix(lower, "password for ") {
		kind = "password"
	} else {
		return "", "", "", errors.New("unsupported askpass prompt")
	}
	start := strings.Index(lower, "https://")
	if start < 0 {
		return "", "", "", errors.New("askpass prompt has no HTTPS origin")
	}
	address := prompt[start:]
	if end := strings.IndexAny(address, "'\" \t\r\n"); end >= 0 {
		address = address[:end]
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.User != nil && kind != "password") {
		return "", "", "", errors.New("askpass prompt origin is invalid")
	}
	username := ""
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			return "", "", "", errors.New("askpass prompt contains embedded password")
		}
		username = u.User.Username()
	}
	return u.Host, kind, username, nil
}
