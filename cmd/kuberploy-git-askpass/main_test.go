package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteResponseReadsOnlyBoundedProjectedCredentialFiles(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "..data")
	if err := os.Mkdir(data, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"username": "x-access-token",
		"password": strings.Repeat("a", 40),
	} {
		if err := os.WriteFile(filepath.Join(data, name), []byte(value), 0o440); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := writeResponse(root, "", "github.com", "Username for 'https://github.com': ", &output); err != nil || output.String() != "x-access-token\n" {
		t.Fatalf("username response=%q err=%v", output.String(), err)
	}
	output.Reset()
	if err := writeResponse(root, "", "github.com", "Password for 'https://x-access-token@github.com': ", &output); err != nil || output.String() != strings.Repeat("a", 40)+"\n" {
		t.Fatalf("password response length=%d err=%v", output.Len(), err)
	}
}

func TestWriteResponseRejectsEscapeInvalidContentAndUnknownPrompt(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(outside, []byte(strings.Repeat("b", 40)), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "password")); err != nil {
		t.Fatal(err)
	}
	if err := writeResponse(root, "", "github.com", "Password for 'https://x-access-token@github.com': ", ioDiscard{}); err == nil {
		t.Fatal("credential symlink escape was accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "username"), []byte("user\nname"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := writeResponse(root, "", "github.com", "Username for 'https://github.com': ", ioDiscard{}); err == nil {
		t.Fatal("credential containing control bytes was accepted")
	}
	if err := writeResponse(root, "", "github.com", "Token for 'https://github.com': ", ioDiscard{}); err == nil {
		t.Fatal("unknown Git credential prompt was accepted")
	}
	for _, prompt := range []string{
		"Username for 'http://github.com': ",
		"Username for 'https://github.com.evil.example': ",
		"Username for 'https://github.com/private/repository': ",
		"Username for 'https://attacker@github.com': ",
		"Password for 'https://github.com': ",
		"Password for 'https://x-access-token@github.com.evil.example': ",
		"Password for 'https://x-access-token@github.com/private/repository': ",
	} {
		if err := writeResponse(root, "", "github.com", prompt, ioDiscard{}); err == nil {
			t.Fatalf("unapproved Git credential prompt was accepted: %q", prompt)
		}
	}
}

func TestWriteResponseUsesBoundedUnixBrokerWithoutCredentialFiles(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "kpa-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "broker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	password := strings.Repeat("token", 8)
	done := make(chan error, 2)
	go func() {
		for range 2 {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				done <- acceptErr
				continue
			}
			var request [1]byte
			_, acceptErr = io.ReadFull(connection, request[:])
			if acceptErr == nil {
				response := "x-access-token\n"
				if request[0] == 2 {
					response = password + "\n"
				}
				_, acceptErr = io.WriteString(connection, response)
			}
			_ = connection.Close()
			done <- acceptErr
		}
	}()
	var output bytes.Buffer
	if err = writeResponse("/unavailable", path, "", "Username for 'https://github.com': ", &output); err != nil || output.String() != "x-access-token\n" {
		t.Fatalf("username output=%q err=%v", output.String(), err)
	}
	output.Reset()
	if err = writeResponse("/unavailable", path, "", "Password for 'https://x-access-token@github.com': ", &output); err != nil || output.String() != password+"\n" {
		t.Fatalf("password output length=%d err=%v", output.Len(), err)
	}
	for range 2 {
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	}
	if _, err = os.Stat(filepath.Join(directory, "password")); !os.IsNotExist(err) {
		t.Fatalf("broker unexpectedly persisted a credential: %v", err)
	}
}

func TestWriteResponseKeepsLegacyFileCredentialsBoundToOperatorApprovedHost(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "username"), []byte("deploy-user"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "password"), []byte(strings.Repeat("p", 40)), 0o440); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writeResponse(root, "", "github.example.test", "Username for 'https://github.example.test': ", &output); err != nil || output.String() != "deploy-user\n" {
		t.Fatalf("legacy username output=%q err=%v", output.String(), err)
	}
	output.Reset()
	if err := writeResponse(root, "", "github.example.test", "Password for 'https://deploy-user@github.example.test': ", &output); err != nil || output.String() != strings.Repeat("p", 40)+"\n" {
		t.Fatalf("legacy password length=%d err=%v", output.Len(), err)
	}
	for _, prompt := range []string{
		"Username for 'https://other.example.test': ",
		"Password for 'https://other-user@github.example.test': ",
		"Password for 'https://deploy-user@github.example.test/private/repo': ",
	} {
		if err := writeResponse(root, "", "github.example.test", prompt, ioDiscard{}); err == nil {
			t.Fatalf("legacy credential escaped its approved host/identity: %q", prompt)
		}
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(value []byte) (int, error) { return len(value), nil }
