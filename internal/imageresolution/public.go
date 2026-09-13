package imageresolution

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// Only anonymous public resolution follows redirects. The transport validates
// and pins public DNS addresses for every new connection, including each hop.
func publicRegistryRedirect(request *http.Request, via []*http.Request) error {
	if len(via) > 5 || len(request.URL.String()) > 8192 {
		return http.ErrUseLastResponse
	}
	// Public registries can redirect to signed URLs. Query parameters belong to
	// that destination; the origin, HTTPS and credential checks still apply.
	destination := *request.URL
	destination.RawQuery = ""
	if !publicHTTPSURL(destination.String(), true) {
		return http.ErrUseLastResponse
	}
	for _, previous := range via {
		if previous.URL.Host != request.URL.Host {
			// net/http otherwise forwards Authorization to subdomains. Do not
			// restore it if a later redirect returns to the initial host.
			request.Header.Del("Authorization")
			break
		}
	}
	return nil
}

// Default anonymous resolution accepts public HTTPS endpoints. Operator-bound
// registries retain their existing explicit endpoint and credential policy.
func publicHTTPSURL(raw string, requirePath bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.Contains(u.Host, ".") || u.Host != strings.ToLower(u.Host) || !hostPattern.MatchString(u.Host) ||
		u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || requirePath && u.Path == "" {
		return false
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		return publicRegistryAddress(ip)
	}
	return true
}

func publicRegistryAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	// Shared carrier space and special-purpose IPv4 networks are not public
	// registry destinations, even though netip calls them global unicast.
	for _, prefix := range []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("240.0.0.0/4"),
	} {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func publicRegistryDialer(lookup func(context.Context, string, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" {
			return nil, ErrConflict
		}
		addresses, err := lookup(ctx, "ip", host)
		if err != nil || len(addresses) == 0 || len(addresses) > 64 {
			return nil, ErrUnavailable
		}
		for _, ip := range addresses {
			if !publicRegistryAddress(ip) {
				return nil, ErrConflict
			}
		}
		for _, ip := range addresses {
			// Dial the validated address, not the hostname: DNS cannot change
			// the destination between validation and connection. HTTP keeps
			// the original hostname for TLS certificate verification and SNI.
			connection, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return connection, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, ErrUnavailable
	}
}
