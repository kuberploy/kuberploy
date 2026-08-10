package edge

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type fakeHostnameResolver struct {
	answers map[string][]netip.Addr
	err     error
	calls   int
}

func (r *fakeHostnameResolver) LookupNetIP(_ context.Context, network, hostname string) ([]netip.Addr, error) {
	r.calls++
	if network != "ip4" {
		return nil, errors.New("unexpected network")
	}
	if r.err != nil {
		return nil, r.err
	}
	return append([]netip.Addr(nil), r.answers[hostname]...), nil
}

func TestSSLIPHostnameIsServerDerivedAndRejectsNonPublicAddresses(t *testing.T) {
	applicationID := "11111111-1111-4111-8111-111111111111"
	environmentID := "22222222-2222-4222-8222-222222222222"
	host, err := SSLIPHostname(applicationID, environmentID, "8.8.8.8")
	if err != nil || host != "kp-32f2a03ad59c10f96a1a.8-8-8-8.sslip.io" {
		t.Fatalf("host=%q err=%v", host, err)
	}
	if second, secondErr := SSLIPHostname(applicationID, "33333333-3333-4333-8333-333333333333", "8.8.8.8"); secondErr != nil || second == host {
		t.Fatalf("second=%q err=%v", second, secondErr)
	}
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.1.1", "192.0.2.1", "198.51.100.2", "203.0.113.3", "224.0.0.1", "2001:4860:4860::8888", "8.8.8.8/32"} {
		if _, err = SSLIPHostname(applicationID, environmentID, address); err == nil {
			t.Fatalf("accepted non-public/canonical IPv4 %q", address)
		}
	}
}

func TestTraefikObservationSelectsOnlyApprovedSSLIPIngress(t *testing.T) {
	config := testRuntimeConfig()
	config.Profiles.Traefik.SSLIP = &SSLIPProfile{Mode: SSLIPAutoFirstIP}
	reader := newFakeKubernetesReader(config)
	key := config.Profiles.Traefik.Namespace + "/" + config.Profiles.Traefik.Service.Name
	service := reader.services[key]
	service.LoadBalancerIngress = []LoadBalancerIngress{{IP: "8.8.8.8"}, {IP: "1.1.1.1"}}
	reader.services[key] = service
	observer := &KubernetesTargetObserver{Reader: reader}
	receipt, err := observer.ObserveTraefik(t.Context(), *config.Profiles.Traefik)
	if err != nil || receipt.SSLIP == nil || receipt.SSLIP.PublicIPv4 != "1.1.1.1" || receipt.SSLIP.Source != SSLIPSourceServiceIP {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}

	service.LoadBalancerIngress = []LoadBalancerIngress{{Hostname: "dynamic.example.test"}}
	reader.services[key] = service
	if _, err = observer.ObserveTraefik(t.Context(), *config.Profiles.Traefik); !errors.Is(err, ErrObservation) {
		t.Fatalf("auto hostname err=%v", err)
	}

	config.Profiles.Traefik.SSLIP = &SSLIPProfile{Mode: SSLIPVerifiedStaticIP, StaticPublicIPv4: "1.1.1.1"}
	reader = newFakeKubernetesReader(config)
	service = reader.services[key]
	service.LoadBalancerIngress = []LoadBalancerIngress{{Hostname: "lb.example.test"}}
	reader.services[key] = service
	resolver := &fakeHostnameResolver{answers: map[string][]netip.Addr{"lb.example.test": {netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")}}}
	observer = &KubernetesTargetObserver{Reader: reader, Resolver: resolver}
	receipt, err = observer.ObserveTraefik(t.Context(), *config.Profiles.Traefik)
	if err != nil || receipt.SSLIP == nil || receipt.SSLIP.PublicIPv4 != "1.1.1.1" || receipt.SSLIP.Source != SSLIPSourceVerifiedStaticIP || resolver.calls != 1 {
		t.Fatalf("static receipt=%+v calls=%d err=%v", receipt, resolver.calls, err)
	}
	resolver.answers["lb.example.test"] = []netip.Addr{netip.MustParseAddr("8.8.8.8")}
	if _, err = observer.ObserveTraefik(t.Context(), *config.Profiles.Traefik); !errors.Is(err, ErrObservation) {
		t.Fatalf("missing static address err=%v", err)
	}
}

func TestMemoryStorePersistsFencedSSLIPObservationAndRejectsIdentityDrift(t *testing.T) {
	config := testRuntimeConfig()
	config.Profiles.Traefik.SSLIP = &SSLIPProfile{Mode: SSLIPAutoFirstIP}
	digest, err := config.Digest()
	if err != nil {
		t.Fatal(err)
	}
	targets, err := config.DesiredTargets()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	if err = store.SynchronizeTargets(t.Context(), digest, targets, now); err != nil {
		t.Fatal(err)
	}
	lease, found, err := store.ClaimTarget(t.Context(), "edge-worker:test:0001", RuntimeContract, digest, now, 2*time.Minute)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	for lease.Target.Kind != KindTraefik {
		receipt := ObservationReceipt{TargetKey: lease.Target.Key, DesiredDigest: lease.Target.DesiredDigest,
			IdentityDigest: testDigest("identity/" + lease.Target.Key), ResourceVersionDigest: testDigest("versions/" + lease.Target.Key)}
		if _, err = store.RecordTargetReady(t.Context(), lease, receipt, now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		lease, found, err = store.ClaimTarget(t.Context(), "edge-worker:test:0001", RuntimeContract, digest, now, 2*time.Minute)
		if err != nil || !found {
			t.Fatalf("claim Traefik found=%v err=%v", found, err)
		}
	}
	endpoint := &SSLIPIngressEndpoint{PublicIPv4: "8.8.8.8", Source: SSLIPSourceServiceIP,
		ServiceUID: "44444444-4444-4444-8444-444444444444", ServiceResourceVersion: "rv-sslip-1"}
	receipt := ObservationReceipt{TargetKey: lease.Target.Key, DesiredDigest: lease.Target.DesiredDigest,
		IdentityDigest: testDigest("identity/sslip/8.8.8.8"), ResourceVersionDigest: testDigest("versions/sslip/1"), SSLIP: endpoint}
	if _, err = store.RecordTargetReady(t.Context(), lease, receipt, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	observation, err := store.SSLIPIngressObservation(t.Context(), "traefik", lease.Target.Revision)
	if err != nil || observation.Endpoint.PublicIPv4 != "8.8.8.8" || !observation.ObservedAt.Equal(now) {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
	if !strings.HasPrefix(receipt.IdentityDigest, "sha256:") {
		t.Fatal("identity digest is missing")
	}
}
