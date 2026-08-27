package environmentfoundation

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	testEnvironmentID = "11111111-1111-4111-8111-111111111111"
	testProjectID     = "22222222-2222-4222-8222-222222222222"
	testBindingID     = "33333333-3333-4333-8333-333333333333"
	testIntentID      = "55555555-5555-4555-8555-555555555555"
	testIntentID2     = "66666666-6666-4666-8666-666666666666"
	testWorker1       = "foundation-worker-test-0001"
	testWorker2       = "foundation-worker-test-0002"
)

func testDigest(label string) string { return digest([]byte(label)) }
func testIdentity() EnvironmentIdentity {
	return EnvironmentIdentity{testEnvironmentID, testProjectID, "kp-demo-dev", "kp-demo"}
}
func testAuthority() GitAuthority {
	return GitAuthority{testBindingID, "refs/heads/main", strings.Repeat("a", 40), 7}
}
func testProfile() Profile {
	return DefaultProfile(testBindingID, testDigest("publisher"), "v1.31")
}

func TestRenderFoundationIsDeterministicAndClosed(t *testing.T) {
	identity, profile := testIdentity(), testProfile()
	first, firstDigest, err := Render(identity, profile)
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigest, err := Render(identity, profile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || firstDigest != secondDigest || firstDigest != digest(first) {
		t.Fatal("render was not byte deterministic")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(first))
	kinds := map[string]int{}
	waves := map[string]int{}
	var observerRules []policyRule
	var http01Policy networkPolicySpec
	documents := 0
	for {
		var raw struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name        string            `yaml:"name"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
			Spec  networkPolicySpec `yaml:"spec"`
			Rules []policyRule      `yaml:"rules"`
		}
		err = decoder.Decode(&raw)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			break
		}
		if raw.Kind == "" {
			continue
		}
		documents++
		kinds[raw.Kind]++
		waves[raw.Metadata.Annotations["argocd.argoproj.io/sync-wave"]]++
		if raw.Kind == "Role" {
			observerRules = raw.Rules
		}
		if raw.Kind == "NetworkPolicy" && raw.Metadata.Name == "kuberploy-http01-solver-ingress" {
			http01Policy = raw.Spec
		}
	}
	if documents != FoundationResourceCount || kinds["Namespace"] != 1 || kinds["ResourceQuota"] != 1 || kinds["LimitRange"] != 1 || kinds["NetworkPolicy"] != 3 || kinds["Role"] != 1 || kinds["RoleBinding"] != 1 {
		t.Fatalf("unexpected inventory docs=%d kinds=%#v", documents, kinds)
	}
	if waves["-30"] != 1 || waves["-20"] != FoundationResourceCount-1 {
		t.Fatalf("foundation sync-wave ordering changed: %#v", waves)
	}
	logRules := 0
	for _, rule := range observerRules {
		for _, resource := range rule.Resources {
			if resource == "pods/log" {
				logRules++
				if len(rule.Resources) != 1 || len(rule.Verbs) != 1 || rule.Verbs[0] != "get" {
					t.Fatalf("pods/log authority widened: %#v", rule)
				}
			}
		}
	}
	if logRules != 1 {
		t.Fatalf("expected one exact pods/log rule, got %#v", observerRules)
	}
	if labels, ok := http01Policy.PodSelector["matchLabels"].(map[string]any); !ok || labels["acme.cert-manager.io/http01-solver"] != "true" {
		t.Fatalf("HTTP-01 solver selector widened or missing: %#v", http01Policy.PodSelector)
	}
	if len(http01Policy.PolicyTypes) != 1 || http01Policy.PolicyTypes[0] != "Ingress" || len(http01Policy.Ingress) != 1 ||
		len(http01Policy.Ingress[0].From) != 1 || len(http01Policy.Ingress[0].Ports) != 1 || http01Policy.Ingress[0].Ports[0] != (networkPort{"TCP", 8089}) ||
		http01Policy.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kuberploy-system" ||
		http01Policy.Ingress[0].From[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != "traefik" {
		t.Fatalf("HTTP-01 solver ingress authority widened or missing: %#v", http01Policy)
	}
	content := string(first)
	for _, required := range []string{"kuberploy.io/foundation-contract: v3", "kuberploy.io/runtime-namespace: \"true\"", "pod-security.kubernetes.io/enforce: baseline", "pod-security.kubernetes.io/enforce-version: v1.31", "pod-security.kubernetes.io/audit: restricted", "pod-security.kubernetes.io/warn: restricted", "name: kuberploy-default-deny", "name: kuberploy-dns-egress", "name: kuberploy-http01-solver-ingress", "port: 53", "resources:", "pods/log", "kind: ServiceAccount", "name: kuberploy-api", "namespace: kuberploy-system"} {
		if !strings.Contains(content, required) {
			t.Fatalf("manifest omitted %q\n%s", required, content)
		}
	}
	for _, forbidden := range []string{"create\n", "delete\n", "patch\n", "secrets/status"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("manifest granted forbidden capability %q", forbidden)
		}
	}
	for _, forbidden := range []string{"kuberploy-edge-ingress", "kuberploy-monitoring-ingress", "kuberploy-monitoring-disabled", "pods/exec", "pods/attach", "pods/portforward"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("foundation rendered broad workload authority %q", forbidden)
		}
	}
	changed := profile
	changed.Quota.Pods++
	other, otherDigest, err := Render(identity, changed)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, other) || firstDigest == otherDigest {
		t.Fatal("bounded profile change did not alter exact manifest")
	}
}

func TestFoundationPathIsInsideOnlyBootstrapArgoRoot(t *testing.T) {
	root := "platform/argocd"
	foundation := ManifestPath(testEnvironmentID)
	helmPayload := "platform/helm-manifests/environments/" + testEnvironmentID + "/releases/x/y/z/values.yaml"
	if !strings.HasPrefix(foundation, root+"/") || strings.HasPrefix(helmPayload, root+"/") {
		t.Fatalf("bootstrap ownership escaped root=%q foundation=%q helm=%q", root, foundation, helmPayload)
	}
}

func TestFoundationClaimsAreExclusivePerPlatformBinding(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	secondEnvironment := EnvironmentIdentity{"77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888", "kp-demo-two", "kp-demo-two"}
	store, err := NewMemoryStore([]AuthorityRecord{{testIdentity(), testAuthority()}, {secondEnvironment, testAuthority()}})
	if err != nil {
		t.Fatal(err)
	}
	profile := testProfile()
	profileDigest, _ := profile.Digest()
	firstIntent, err := store.EnsureIntent(ctx, EnsureRequest{testIntentID, testEnvironmentID, profile, now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.EnsureIntent(ctx, EnsureRequest{testIntentID2, secondEnvironment.EnvironmentID, profile, now}); err != nil {
		t.Fatal(err)
	}
	first, found, err := store.ClaimIntent(ctx, testWorker1, profileDigest, profile.PublisherConfigDigest, now.Add(time.Second), time.Minute)
	if err != nil || !found || first.Intent.ID != firstIntent.ID {
		t.Fatalf("first binding claim failed: %#v %v", first, err)
	}
	if _, found, err = store.ClaimIntent(ctx, testWorker2, profileDigest, profile.PublisherConfigDigest, now.Add(2*time.Second), time.Minute); err != nil || found {
		t.Fatalf("second worker entered the same binding lane: found=%v err=%v", found, err)
	}
	bound, err := store.BindWriteBase(ctx, first, first.Intent.Authority.PlannedHead, now.Add(2*time.Second), now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	first.Intent = bound
	receipt := PublicationReceipt{IntentID: first.Intent.ID, BindingID: testBindingID, TargetRef: first.Intent.Authority.TargetRef,
		Path: first.Intent.Path, ContentDigest: first.Intent.ManifestDigest, ParentRevision: first.Intent.WriteBaseRevision,
		CommittedRevision: strings.Repeat("b", 40), ProviderRequest: "github:claim-serial", ObservedAt: now.Add(2 * time.Second)}
	if _, err = store.RecordReady(ctx, first, receipt, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	second, found, err := store.ClaimIntent(ctx, testWorker2, profileDigest, profile.PublisherConfigDigest, now.Add(4*time.Second), time.Minute)
	if err != nil || !found || second.Intent.ID != testIntentID2 {
		t.Fatalf("second intent did not enter after lane release: %#v %v", second, err)
	}
}

func TestMemoryStoreDerivesIdentityAndFencesRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	store, err := NewMemoryStore([]AuthorityRecord{{testIdentity(), testAuthority()}})
	if err != nil {
		t.Fatal(err)
	}
	profile := testProfile()
	profileDigest, _ := profile.Digest()
	intent, err := store.EnsureIntent(ctx, EnsureRequest{testIntentID, testEnvironmentID, profile, now})
	if err != nil {
		t.Fatal(err)
	}
	if intent.ProjectID != testProjectID || intent.Namespace != "kp-demo-dev" || intent.Path != ManifestPath(testEnvironmentID) {
		t.Fatalf("identity was not server-derived: %#v", intent)
	}
	idempotent, err := store.EnsureIntent(ctx, EnsureRequest{testIntentID, testEnvironmentID, profile, now.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if idempotent.ID != intent.ID {
		t.Fatalf("same immutable intent duplicated: %s", idempotent.ID)
	}
	first, found, err := store.ClaimIntent(ctx, testWorker1, profileDigest, profile.PublisherConfigDigest, now.Add(2*time.Second), MinimumLease)
	if err != nil || !found {
		t.Fatalf("claim: %#v %v", first, err)
	}
	second, found, err := store.ClaimIntent(ctx, testWorker2, profileDigest, profile.PublisherConfigDigest, first.Until, MinimumLease)
	if err != nil || !found || second.Epoch != first.Epoch+1 {
		t.Fatalf("recovery claim: %#v %v", second, err)
	}
	if _, err = store.RecordRetry(ctx, first, "stale-worker", false, now.Add(time.Minute), now.Add(3*time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale lease finalized: %v", err)
	}
	bound, err := store.BindWriteBase(ctx, second, strings.Repeat("a", 40), first.Until.Add(time.Second), first.Until.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	second.Intent = bound
	receipt := PublicationReceipt{IntentID: intent.ID, BindingID: testBindingID, TargetRef: "refs/heads/main", Path: intent.Path, ContentDigest: intent.ManifestDigest, ParentRevision: strings.Repeat("a", 40), CommittedRevision: strings.Repeat("b", 40), ProviderRequest: "github:req-1", ObservedAt: first.Until.Add(time.Second)}
	ready, err := store.RecordReady(ctx, second, receipt, first.Until.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != StateReady || ready.LeaseOwner != "" {
		t.Fatalf("not finalized: %#v", ready)
	}
	r := Readiness{testWorker2, 1, Contract, profileDigest, profile.PublisherConfigDigest, 1, now, first.Until.Add(2 * time.Second), first.Until.Add(time.Minute)}
	if err = store.RecordReadiness(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err = store.ExactReady(ctx, profileDigest, profile.PublisherConfigDigest, 1, first.Until.Add(3*time.Second)); err != nil {
		t.Fatalf("exact ready rejected: %v", err)
	}
	if err = store.ExactReady(ctx, profileDigest, testDigest("wrong"), 1, first.Until.Add(3*time.Second)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wrong publisher accepted: %v", err)
	}
	changed := profile
	changed.Quota.Pods++
	replacement, err := store.EnsureIntent(ctx, EnsureRequest{testIntentID2, testEnvironmentID, changed, first.Until.Add(4 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID != testIntentID2 {
		t.Fatalf("replacement missing: %#v", replacement)
	}
	old, err := store.Intent(ctx, testIntentID)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != StateSuperseded || old.Active || old.CommittedRevision == "" {
		t.Fatalf("verified receipt was not preserved on supersede: %#v", old)
	}
}

func TestProfileRotationRecoversNonterminalPredecessorBeforeReplacement(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	store, err := NewMemoryStore([]AuthorityRecord{{testIdentity(), testAuthority()}})
	if err != nil {
		t.Fatal(err)
	}
	oldProfile := testProfile()
	oldIntent, err := store.EnsureIntent(ctx, EnsureRequest{testIntentID, testEnvironmentID, oldProfile, now})
	if err != nil {
		t.Fatal(err)
	}
	newProfile := oldProfile
	newProfile.ObserverServiceAccount = "kuberploy-api-rotated"
	deferred, err := store.EnsureIntent(ctx, EnsureRequest{testIntentID2, testEnvironmentID, newProfile, now.Add(time.Second)})
	if err != nil || deferred.ID != oldIntent.ID {
		t.Fatalf("nonterminal predecessor was not preserved: %#v err=%v", deferred, err)
	}
	newDigest, _ := newProfile.Digest()
	lease, found, err := store.ClaimIntent(ctx, testWorker1, newDigest, newProfile.PublisherConfigDigest, now.Add(2*time.Second), MinimumLease)
	if err != nil || !found || lease.Intent.ID != oldIntent.ID || lease.Intent.ProfileDigest == newDigest {
		t.Fatalf("new runtime did not claim the exact legacy predecessor: %#v found=%v err=%v", lease, found, err)
	}
}

type fakePublisher struct {
	identity PublisherIdentity
	store    Store
	request  PublicationRequest
	err      error
	now      time.Time
}

func (f *fakePublisher) Identity() PublisherIdentity { return f.identity }
func (f *fakePublisher) Publish(ctx context.Context, lease Lease, r PublicationRequest) (PublicationReceipt, error) {
	f.request = r
	if f.err != nil {
		return PublicationReceipt{}, f.err
	}
	intent, err := f.store.BindWriteBase(ctx, lease, r.PlannedHead, f.now, f.now)
	if err != nil {
		return PublicationReceipt{}, err
	}
	return PublicationReceipt{IntentID: r.IntentID, BindingID: r.BindingID, TargetRef: r.TargetRef, Path: r.Path, ContentDigest: r.ContentDigest, ParentRevision: intent.WriteBaseRevision, CommittedRevision: strings.Repeat("c", 40), ProviderRequest: "github:req-controller", ObservedAt: f.now}, nil
}

func TestControllerPublishesOnlyImmutableRequest(t *testing.T) {
	now := time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC)
	store, _ := NewMemoryStore([]AuthorityRecord{{testIdentity(), testAuthority()}})
	profile := testProfile()
	intent, err := store.EnsureIntent(context.Background(), EnsureRequest{testIntentID, testEnvironmentID, profile, now})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &fakePublisher{identity: PublisherIdentity{PublisherContract, ProtectedGitPolicy, profile.PublisherConfigDigest}, store: store, now: now.Add(time.Second)}
	controller := Controller{Store: store, Publisher: publisher, Profile: profile, WorkerID: testWorker1, WorkerEpoch: 1, WorkLease: time.Minute, MinimumBackoff: time.Second, MaximumBackoff: time.Minute, Now: func() time.Time { return now.Add(time.Second) }}
	didWork, err := controller.Reconcile(context.Background())
	if err != nil || !didWork {
		t.Fatalf("reconcile=%v %v", didWork, err)
	}
	if publisher.request.Validate(Intent{ /* validation uses durable state below */ }, publisher.identity) == nil {
		t.Fatal("request validated without durable intent")
	}
	got, err := store.Intent(context.Background(), intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateReady || publisher.request.Path != intent.Path || !bytes.Equal(publisher.request.Content, intent.Manifest) || publisher.request.CommitTrailer != intent.CommitTrailer {
		t.Fatalf("publisher escaped immutable intent: request=%#v state=%#v", publisher.request, got)
	}
}
