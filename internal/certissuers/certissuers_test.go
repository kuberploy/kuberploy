package certissuers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const actor = "6ba7b810-9dad-41d1-80b4-00c04fd430c8"

func command(key string, at time.Time) Command {
	return Command{ActorID: actor, IdempotencyKey: key, RequestID: "request-" + key, Now: at}
}
func httpSpec() Spec {
	return Spec{ACME: ACME{Email: "Admin@Example.com", Server: LetsEncryptProduction, AccountPrivateKeySecretName: "letsencrypt-account"}, HTTP01: &HTTP01Spec{}}
}
func dnsSpec(zones ...string) Spec {
	return Spec{ACME: ACME{Email: "admin@example.com", Server: LetsEncryptStaging, AccountPrivateKeySecretName: "letsencrypt-dns-account"}, Cloudflare: &CloudflareDNS01Spec{APITokenSecretName: "cloudflare-token", APITokenSecretKey: "api-token", DNSZones: zones}}
}

func TestCatalogIsReadyGatedHostnameAwareAndRedacted(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()
	store := NewMemoryStore()
	http, err := store.Create(ctx, command("create-http", now), "letsencrypt-production-http", httpSpec())
	if err != nil {
		t.Fatal(err)
	}
	dns, err := store.Create(ctx, command("create-dns", now), "letsencrypt-staging-cloudflare", dnsSpec("Example.COM", "corp.example.net"))
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := NewCatalog(store)
	if got, err := catalog.ForHostname(ctx, "api.127-0-0-1.sslip.io", now, 5*time.Minute, 20); err != nil || len(got) != 0 {
		t.Fatalf("pending catalog=%v err=%v", got, err)
	}
	observed := now.Add(time.Minute)
	queryNow := observed.Add(10 * time.Second)
	for _, created := range []MutationResult{http, dns} {
		if err := store.RecordObservation(ctx, Observation{ProfileID: created.Profile.ID, Revision: 1, State: Ready, ObservedSpecDigest: created.Revision.SpecDigest, ObservedGeneration: 1, ObservedAt: &observed, UpdatedAt: observed}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := catalog.ForHostname(ctx, "api.127-0-0-1.sslip.io", queryNow, 5*time.Minute, 20)
	if err != nil || len(got) != 1 || got[0].Solver != HTTP01 {
		t.Fatalf("sslip catalog=%v err=%v", got, err)
	}
	if got[0].Environment != Production {
		t.Fatalf("HTTP environment=%q", got[0].Environment)
	}
	got, err = catalog.ForHostname(ctx, "app.example.com", queryNow, 5*time.Minute, 20)
	if err != nil || len(got) != 2 {
		t.Fatalf("covered catalog=%v err=%v", got, err)
	}
	got, err = catalog.ForHostname(ctx, "*.example.com", queryNow, 5*time.Minute, 20)
	if err != nil || len(got) != 1 || got[0].Solver != DNS01Cloudflare {
		t.Fatalf("wildcard catalog=%v err=%v", got, err)
	}
	if got[0].Environment != Staging {
		t.Fatalf("DNS environment=%q", got[0].Environment)
	}
	observation, err := store.Observation(ctx, dns.Profile.ID, 1)
	if err != nil || observation.State != Ready {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
	got, err = catalog.ForHostname(ctx, "notexample.com", queryNow, 5*time.Minute, 20)
	if err != nil || len(got) != 1 || got[0].Solver != HTTP01 {
		t.Fatalf("zone boundary catalog=%v err=%v", got, err)
	}
	raw, _ := json.Marshal(got)
	for _, forbidden := range []string{"admin@example.com", "cloudflare-token", "api-token", "example.com"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("tenant DTO leaked %q: %s", forbidden, raw)
		}
	}
	if stale, err := catalog.ForHostname(ctx, "app.example.com", observed.Add(6*time.Minute), 5*time.Minute, 20); err != nil || len(stale) != 0 {
		t.Fatalf("stale catalog=%v err=%v", stale, err)
	}
	if future, err := catalog.ForHostname(ctx, "app.example.com", observed.Add(-31*time.Second), 5*time.Minute, 20); err != nil || len(future) != 0 {
		t.Fatalf("future catalog=%v err=%v", future, err)
	}
	entry, _ := store.Current(ctx, dns.Profile.ID)
	entry.Revision.Spec.Cloudflare.DNSZones[0] = "attacker.example"
	again, _ := store.Current(ctx, dns.Profile.ID)
	if again.Revision.Spec.Cloudflare.DNSZones[0] == "attacker.example" {
		t.Fatal("Current leaked mutable spec slice")
	}
}

func TestClosedSpecsIdempotencyAndExactObservation(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()
	store := NewMemoryStore()
	bad := httpSpec()
	bad.Cloudflare = dnsSpec("example.com").Cloudflare
	if _, err := store.Create(ctx, command("bad-mixed", now), "mixed", bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mixed solver err=%v", err)
	}
	bad = httpSpec()
	bad.ACME.Server = "https://attacker.invalid/acme"
	if _, err := store.Create(ctx, command("bad-server", now), "bad-server", bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("arbitrary server err=%v", err)
	}
	first, err := store.Create(ctx, command("same-key", now), "http-one", httpSpec())
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.Create(ctx, command("same-key", now), "http-one", httpSpec())
	if err != nil || !replay.Replay || replay.Profile.ID != first.Profile.ID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err = store.Create(ctx, command("same-key", now), "substitution", httpSpec()); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency substitution err=%v", err)
	}
	observed := now.Add(time.Second)
	if err = store.RecordObservation(ctx, Observation{ProfileID: first.Profile.ID, Revision: 1, State: Ready, ObservedSpecDigest: "sha256:" + strings.Repeat("0", 64), ObservedGeneration: 1, ObservedAt: &observed, UpdatedAt: observed}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched ready digest err=%v", err)
	}
	pending, err := store.PendingMaterialization(ctx, 20)
	if err != nil || len(pending) != 1 || pending[0].Spec.ACME.Email == "" {
		t.Fatalf("worker desired=%v err=%v", pending, err)
	}
}
