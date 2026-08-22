package gitssh

import (
	"bytes"
	"crypto/subtle"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

var sshDNSHostRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)

// HostKeyPin binds one exact SSH dial endpoint to one public host key.
type HostKeyPin struct {
	Endpoint    string `json:"endpoint"`
	PublicKey   string `json:"publicKey"`
	Fingerprint string `json:"fingerprint"`
}

// KnownHosts renders exact OpenSSH known_hosts input for the checkout
// container. No network discovery or trust-on-first-use occurs here.
func KnownHosts(pins []HostKeyPin) ([]byte, error) {
	if len(pins) == 0 || len(pins) > 16 {
		return nil, ErrInvalidHostKeyPin
	}
	lines := make([]string, 0, len(pins))
	seen := make(map[string]string, len(pins))
	for _, pin := range pins {
		parsed, err := NewHostKeyPin(pin.Endpoint, pin.PublicKey)
		if err != nil || pin.Fingerprint != "" && pin.Fingerprint != parsed.Fingerprint {
			return nil, ErrInvalidHostKeyPin
		}
		if fingerprint, exists := seen[parsed.Endpoint]; exists {
			if fingerprint != parsed.Fingerprint {
				return nil, ErrInvalidHostKeyPin
			}
			continue
		}
		publicKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(parsed.PublicKey))
		if err != nil {
			return nil, ErrInvalidHostKeyPin
		}
		seen[parsed.Endpoint] = parsed.Fingerprint
		hostPattern := parsed.Endpoint
		if host, port, splitErr := net.SplitHostPort(parsed.Endpoint); splitErr == nil && port == "22" {
			hostPattern = host
		}
		lines = append(lines, knownhosts.Line([]string{hostPattern}, publicKey))
	}
	result := []byte(strings.Join(lines, "\n") + "\n")
	if len(result) > 16<<10 || bytes.ContainsAny(result, "\x00\r") {
		clear(result)
		return nil, ErrInvalidHostKeyPin
	}
	return result, nil
}

func NewHostKeyPin(endpoint, authorizedKey string) (HostKeyPin, error) {
	endpoint = strings.TrimSpace(endpoint)
	host, portText, splitErr := net.SplitHostPort(endpoint)
	port, portErr := strconv.Atoi(portText)
	if endpoint == "" || strings.ContainsAny(endpoint, " \t\r\n") || splitErr != nil || portErr != nil || port < 1 || port > 65535 ||
		host == "" || len(host) > 253 || net.JoinHostPort(host, strconv.Itoa(port)) != endpoint ||
		!validSSHHost(host) {
		return HostKeyPin{}, fmt.Errorf("endpoint is required and cannot contain whitespace: %w", ErrInvalidHostKeyPin)
	}
	publicKey, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	_ = comment
	_ = options
	if err != nil || len(strings.TrimSpace(string(rest))) != 0 {
		return HostKeyPin{}, fmt.Errorf("parse authorized host key: %w", ErrInvalidHostKeyPin)
	}
	return HostKeyPin{
		Endpoint:    endpoint,
		PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey))),
		Fingerprint: ssh.FingerprintSHA256(publicKey),
	}, nil
}

func validSSHHost(host string) bool {
	if address, err := netip.ParseAddr(host); err == nil {
		return address.String() == host
	}
	return host == strings.ToLower(host) && sshDNSHostRE.MatchString(host) && !strings.Contains(host, "..")
}

type StrictHostKeyVerifier struct {
	fingerprints map[string]string
}

func NewStrictHostKeyVerifier(pins []HostKeyPin) (*StrictHostKeyVerifier, error) {
	verifier := &StrictHostKeyVerifier{fingerprints: make(map[string]string, len(pins))}
	for _, pin := range pins {
		parsed, err := NewHostKeyPin(pin.Endpoint, pin.PublicKey)
		if err != nil {
			return nil, err
		}
		if pin.Fingerprint != "" && pin.Fingerprint != parsed.Fingerprint {
			return nil, fmt.Errorf("fingerprint does not match public key for %q: %w", pin.Endpoint, ErrInvalidHostKeyPin)
		}
		if existing, found := verifier.fingerprints[parsed.Endpoint]; found && existing != parsed.Fingerprint {
			return nil, fmt.Errorf("conflicting pins for %q: %w", parsed.Endpoint, ErrInvalidHostKeyPin)
		}
		verifier.fingerprints[parsed.Endpoint] = parsed.Fingerprint
	}
	return verifier, nil
}

func (v *StrictHostKeyVerifier) Verify(endpoint string, presented ssh.PublicKey) error {
	if v == nil || presented == nil {
		return ErrHostKeyNotPinned
	}
	expected, found := v.fingerprints[endpoint]
	if !found {
		return fmt.Errorf("%q: %w", endpoint, ErrHostKeyNotPinned)
	}
	actual := ssh.FingerprintSHA256(presented)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) != 1 {
		return fmt.Errorf("%q: %w", endpoint, ErrHostKeyChanged)
	}
	return nil
}

func (v *StrictHostKeyVerifier) HostKeyCallback() ssh.HostKeyCallback {
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		return v.Verify(hostname, key)
	}
}
