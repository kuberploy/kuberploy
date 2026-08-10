package gitprojection

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	GitAskPassSocketEnv = "KUBERPLOY_GIT_ASKPASS_SOCKET"
	requestUsername     = byte(1)
	requestPassword     = byte(2)
	maximumSocketPath   = 100
	credentialBrokerDir = "/tmp"
)

// askPassBroker exposes a credential only over a randomly named, mode-0700
// Unix socket directory for the lifetime of one Git child process. Git sees
// only the socket path; the token never enters argv, a URL, Git config, a file,
// or the child environment.
type askPassBroker struct {
	directory string
	path      string
	listener  *net.UnixListener
	done      chan struct{}
	value     *GitCredential
}

func startAskPassBroker(root string, value *GitCredential) (*askPassBroker, error) {
	if value == nil || value.validate(time.Now().UTC()) != nil || root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrInvalid
	}
	// /tmp is a dedicated emptyDir in the worker Pod. Keeping the socket name
	// here also stays below the small sockaddr_un limits on macOS and Linux.
	directory, err := os.MkdirTemp(credentialBrokerDir, "kpa-")
	if err != nil {
		return nil, errors.New("create Git credential broker")
	}
	path := filepath.Join(directory, "broker.sock")
	if len(path) > maximumSocketPath {
		_ = os.RemoveAll(directory)
		return nil, errors.New("Git credential broker path is too long")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, errors.New("start Git credential broker")
	}
	listener.SetUnlinkOnClose(true)
	broker := &askPassBroker{directory: directory, path: path, listener: listener, done: make(chan struct{}), value: value}
	go broker.serve()
	return broker, nil
}

func (b *askPassBroker) serve() {
	defer close(b.done)
	for {
		connection, err := b.listener.AcceptUnix()
		if err != nil {
			return
		}
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		var request [1]byte
		_, readErr := io.ReadFull(connection, request[:])
		var response []byte
		if readErr == nil && b.value != nil {
			switch request[0] {
			case requestUsername:
				response = b.value.Username
			case requestPassword:
				response = b.value.Password
			}
		}
		if len(response) != 0 {
			_, _ = connection.Write(response)
			_, _ = connection.Write([]byte{'\n'})
		}
		_ = connection.Close()
	}
}

func (b *askPassBroker) close() {
	if b == nil {
		return
	}
	if b.listener != nil {
		_ = b.listener.Close()
	}
	if b.done != nil {
		<-b.done
	}
	if b.directory != "" && filepath.Clean(filepath.Dir(b.directory)) == credentialBrokerDir && strings.HasPrefix(filepath.Base(b.directory), "kpa-") {
		_ = os.RemoveAll(b.directory)
	}
}
