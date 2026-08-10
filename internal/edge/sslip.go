package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"slices"
	"strings"
	"time"
)

const SSLIPZone = "sslip.io"

type SSLIPSelectionMode string

const (
	SSLIPAutoFirstIP      SSLIPSelectionMode = "auto-first-ip"
	SSLIPVerifiedStaticIP SSLIPSelectionMode = "verified-static-ip"

	SSLIPSourceServiceIP        = "service-ip"
	SSLIPSourceVerifiedStaticIP = "verified-static-ip"
)

// SSLIPProfile is operator-owned edge policy. Tenant AppConfig never accepts
// an IP address or an arbitrary sslip.io hostname. Auto mode selects the first
// canonical public IPv4 directly reported by Kubernetes. Hostname-based load
// balancers require an operator-attested stable IPv4 that is re-resolved every
// poll.
type SSLIPProfile struct {
	Mode             SSLIPSelectionMode `json:"mode"`
	StaticPublicIPv4 string             `json:"staticPublicIPv4,omitempty"`
}

func (p SSLIPProfile) Validate() error {
	switch p.Mode {
	case SSLIPAutoFirstIP:
		if p.StaticPublicIPv4 != "" {
			return ErrInvalid
		}
	case SSLIPVerifiedStaticIP:
		if !validPublicIPv4String(p.StaticPublicIPv4) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type LoadBalancerIngress struct {
	IP       string
	Hostname string
}

func (i LoadBalancerIngress) validate() error {
	if (i.IP == "") == (i.Hostname == "") {
		return ErrInvalid
	}
	if i.IP != "" {
		address, err := netip.ParseAddr(i.IP)
		if err != nil || address.String() != i.IP {
			return ErrInvalid
		}
		return nil
	}
	if i.Hostname != strings.ToLower(i.Hostname) || strings.HasSuffix(i.Hostname, ".") || !validDNSName(i.Hostname) {
		return ErrInvalid
	}
	return nil
}

type HostnameResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type SSLIPIngressEndpoint struct {
	PublicIPv4             string
	Source                 string
	ServiceUID             string
	ServiceResourceVersion string
}

func (e SSLIPIngressEndpoint) Validate() error {
	if !validPublicIPv4String(e.PublicIPv4) || (e.Source != SSLIPSourceServiceIP && e.Source != SSLIPSourceVerifiedStaticIP) ||
		!uuidPattern.MatchString(e.ServiceUID) || !resourceVersionPattern.MatchString(e.ServiceResourceVersion) {
		return ErrInvalid
	}
	return nil
}

type SSLIPIngressObservation struct {
	TargetKey           string
	ProfileRevision     int64
	DesiredDigest       string
	RuntimeConfigDigest string
	Endpoint            SSLIPIngressEndpoint
	ObservedAt          time.Time
}

func (o SSLIPIngressObservation) Validate(target DesiredTarget) error {
	if target.Validate() != nil || target.Kind != KindTraefik || o.TargetKey != target.Key || o.ProfileRevision != target.Revision ||
		o.DesiredDigest != target.DesiredDigest || o.RuntimeConfigDigest != target.RuntimeConfigDigest || o.Endpoint.Validate() != nil || o.ObservedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

// SSLIPHostname derives the only hostname tenants may use for sslip mode. It
// contains no caller-controlled label and is stable for one application and
// environment while the observed public address remains stable.
func SSLIPHostname(applicationID, environmentID, publicIPv4 string) (string, error) {
	if !uuidPattern.MatchString(applicationID) || !uuidPattern.MatchString(environmentID) || !validPublicIPv4String(publicIPv4) {
		return "", ErrInvalid
	}
	digest := sha256.Sum256([]byte("kuberploy-sslip-host-v1:" + applicationID + ":" + environmentID))
	label := "kp-" + hex.EncodeToString(digest[:10])
	host := label + "." + strings.ReplaceAll(publicIPv4, ".", "-") + "." + SSLIPZone
	if !validDNSName(host) {
		return "", ErrInvalid
	}
	return host, nil
}

var forbiddenPublicIPv4Prefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

func validPublicIPv4String(value string) bool {
	address, err := netip.ParseAddr(value)
	if err != nil || !address.Is4() || address.String() != value || !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range forbiddenPublicIPv4Prefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func selectSSLIPIngress(
	ctx context.Context,
	profile SSLIPProfile,
	service ServiceSnapshot,
	resolver HostnameResolver,
	callTimeout time.Duration,
) (SSLIPIngressEndpoint, error) {
	if profile.Validate() != nil || service.ObjectSnapshot.validate(service.Name, service.Namespace, service.SpecDigest) != nil ||
		service.Type != "LoadBalancer" || len(service.LoadBalancerIngress) < 1 || len(service.LoadBalancerIngress) > 16 ||
		callTimeout < time.Second || callTimeout > 30*time.Second {
		return SSLIPIngressEndpoint{}, ErrInvalid
	}
	for _, ingress := range service.LoadBalancerIngress {
		if ingress.validate() != nil {
			return SSLIPIngressEndpoint{}, mismatch("sslip-endpoint-invalid")
		}
	}
	endpoint := func(address, source string) SSLIPIngressEndpoint {
		return SSLIPIngressEndpoint{PublicIPv4: address, Source: source, ServiceUID: service.UID, ServiceResourceVersion: service.ResourceVersion}
	}
	if profile.Mode == SSLIPAutoFirstIP {
		addresses := make([]netip.Addr, 0, len(service.LoadBalancerIngress))
		for _, ingress := range service.LoadBalancerIngress {
			if validPublicIPv4String(ingress.IP) {
				addresses = append(addresses, netip.MustParseAddr(ingress.IP))
			}
		}
		slices.SortFunc(addresses, func(left, right netip.Addr) int { return left.Compare(right) })
		addresses = slices.Compact(addresses)
		if len(addresses) == 0 {
			return SSLIPIngressEndpoint{}, mismatch("sslip-public-ip-unavailable")
		}
		return endpoint(addresses[0].String(), SSLIPSourceServiceIP), nil
	}
	for _, ingress := range service.LoadBalancerIngress {
		if ingress.IP == profile.StaticPublicIPv4 {
			return endpoint(profile.StaticPublicIPv4, SSLIPSourceServiceIP), nil
		}
	}
	if resolver == nil {
		return SSLIPIngressEndpoint{}, mismatch("sslip-static-ip-unverified")
	}
	hostnames := make([]string, 0, len(service.LoadBalancerIngress))
	for _, ingress := range service.LoadBalancerIngress {
		if ingress.Hostname != "" {
			hostnames = append(hostnames, ingress.Hostname)
		}
	}
	slices.Sort(hostnames)
	hostnames = slices.Compact(hostnames)
	for _, hostname := range hostnames {
		addresses, err := observeCall(ctx, callTimeout, func(callContext context.Context) ([]netip.Addr, error) {
			return resolver.LookupNetIP(callContext, "ip4", hostname)
		})
		if err != nil {
			return SSLIPIngressEndpoint{}, ErrUnavailable
		}
		if len(addresses) > 32 {
			return SSLIPIngressEndpoint{}, mismatch("sslip-hostname-answers-invalid")
		}
		for _, address := range addresses {
			if address.Is4() && address.String() == profile.StaticPublicIPv4 {
				return endpoint(profile.StaticPublicIPv4, SSLIPSourceVerifiedStaticIP), nil
			}
		}
	}
	return SSLIPIngressEndpoint{}, mismatch("sslip-static-ip-unverified")
}
