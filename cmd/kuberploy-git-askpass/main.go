package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	credentialRoot   = "/var/run/secrets/kuberploy/git"
	askPassSocketEnv = "KUBERPLOY_GIT_ASKPASS_SOCKET"
	askPassHostEnv   = "KUBERPLOY_GIT_ASKPASS_APPROVED_HOST"
	maximumSocket    = 100
)

func main() {
	if len(os.Args) != 2 || writeResponse(credentialRoot, os.Getenv(askPassSocketEnv), os.Getenv(askPassHostEnv), os.Args[1], os.Stdout) != nil {
		// Keep this deliberately generic: Git may include a remote URL in its
		// prompt, and credential bytes must never be copied into an error.
		_, _ = fmt.Fprintln(os.Stderr, "Git credential is unavailable")
		os.Exit(1)
	}
}

func writeResponse(root, socket, approvedHost, prompt string, output io.Writer) error {
	name, request, promptUsername, ok := parseCredentialPrompt(prompt, approvedHost, socket != "")
	if !ok {
		return errors.New("unsupported credential prompt")
	}
	var value []byte
	var err error
	if socket == "" {
		if name == "password" {
			username, usernameErr := readCredential(root, "username")
			if usernameErr != nil {
				return usernameErr
			}
			matches := string(username) == promptUsername
			zero(username)
			if !matches {
				return errors.New("credential prompt username does not match")
			}
		}
		value, err = readCredential(root, name)
	} else {
		value, err = readBrokerCredential(socket, request, name)
	}
	if err != nil {
		return err
	}
	defer zero(value)
	if _, err = output.Write(value); err != nil {
		return err
	}
	_, err = output.Write([]byte{'\n'})
	return err
}

func parseCredentialPrompt(prompt, approvedHost string, broker bool) (string, byte, string, bool) {
	name, request, prefix := "", byte(0), ""
	switch {
	case strings.HasPrefix(prompt, "Username for '"):
		name, request, prefix = "username", 1, "Username for '"
	case strings.HasPrefix(prompt, "Password for '"):
		name, request, prefix = "password", 2, "Password for '"
	default:
		return "", 0, "", false
	}
	if !strings.HasSuffix(prompt, "': ") {
		return "", 0, "", false
	}
	rawURL := strings.TrimSuffix(strings.TrimPrefix(prompt, prefix), "': ")
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", 0, "", false
	}
	if broker {
		if parsed.Host != "github.com" || parsed.Hostname() != "github.com" || parsed.Port() != "" {
			return "", 0, "", false
		}
	} else if !validApprovedHost(approvedHost) || !strings.EqualFold(parsed.Host, approvedHost) {
		return "", 0, "", false
	}
	_, hasPassword := "", false
	promptUsername := ""
	if parsed.User != nil {
		_, hasPassword = parsed.User.Password()
		promptUsername = parsed.User.Username()
	}
	if name == "username" && parsed.User != nil || name == "password" && (parsed.User == nil || promptUsername == "" || hasPassword) ||
		broker && name == "password" && promptUsername != "x-access-token" {
		return "", 0, "", false
	}
	return name, request, promptUsername, true
}

func validApprovedHost(host string) bool {
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "\x00\r\n/@?#") {
		return false
	}
	parsed, err := url.Parse("https://" + host)
	return err == nil && parsed.Host == host && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func readBrokerCredential(path string, request byte, name string) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > maximumSocket ||
		(request != 1 && request != 2) || (name != "username" && name != "password") {
		return nil, errors.New("invalid credential broker")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("credential broker is unavailable")
	}
	connection, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return nil, errors.New("credential broker is unavailable")
	}
	defer connection.Close()
	if deadlineErr := connection.SetDeadline(time.Now().Add(5 * time.Second)); deadlineErr != nil {
		return nil, errors.New("credential broker is unavailable")
	}
	if _, err = connection.Write([]byte{request}); err != nil {
		return nil, errors.New("credential broker is unavailable")
	}
	maximum := int64(16 << 10)
	if name == "username" {
		maximum = 128
	}
	value, err := io.ReadAll(io.LimitReader(connection, maximum+2))
	if err != nil || int64(len(value)) < 2 || int64(len(value)) > maximum+1 || value[len(value)-1] != '\n' {
		zero(value)
		return nil, errors.New("credential broker response is invalid")
	}
	value = value[:len(value)-1]
	minimum := 1
	if name == "password" {
		minimum = 16
	}
	if len(value) < minimum || !printableASCII(value) {
		zero(value)
		return nil, errors.New("credential broker response is invalid")
	}
	return value, nil
}

func readCredential(root, name string) ([]byte, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || (name != "username" && name != "password") {
		return nil, errors.New("invalid credential location")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, errors.New("credential root is unavailable")
	}
	path := filepath.Join(root, name)
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil || !within(realRoot, realPath) {
		return nil, errors.New("credential path is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("credential path is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("credential is not a regular file")
	}
	maximum := int64(16 << 10)
	if name == "username" {
		maximum = 128
	}
	if info.Size() < 1 || info.Size() > maximum {
		return nil, errors.New("credential size is invalid")
	}
	value, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(value)) != info.Size() || int64(len(value)) > maximum {
		zero(value)
		return nil, errors.New("credential read is invalid")
	}
	minimum := 1
	if name == "password" {
		minimum = 16
	}
	if len(value) < minimum || !printableASCII(value) {
		zero(value)
		return nil, errors.New("credential content is invalid")
	}
	return value, nil
}

func within(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	return path != root && strings.HasPrefix(path, root+string(os.PathSeparator))
}

func printableASCII(value []byte) bool {
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
