package certissuers

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

const (
	protectedTestBinding                = "11111111-1111-4111-8111-111111111111"
	protectedTestObserverNamespace      = "kuberploy-system"
	protectedTestObserverServiceAccount = "kuberploy-certissuer-observer"
)

var protectedTestObserver = ProtectedObserverSubject{Namespace: protectedTestObserverNamespace, ServiceAccount: protectedTestObserverServiceAccount}

type protectedHeadVerifier struct {
	remote string
	now    time.Time
	calls  int
}

func (v *protectedHeadVerifier) VerifyTargetHead(ctx context.Context, binding gitprojection.Binding,
	source gitprojection.ObservationSource) (gitprojection.VerifiedHead, error) {
	v.calls++
	if source != gitprojection.ObservationWrite {
		return gitprojection.VerifiedHead{}, errors.New("wrong source")
	}
	command := exec.CommandContext(ctx, "git", "--git-dir", v.remote, "rev-parse", binding.TargetRef)
	output, err := command.Output()
	if err != nil {
		return gitprojection.VerifiedHead{}, err
	}
	return gitprojection.VerifiedHead{BindingID: binding.ID, Repository: binding.Repository,
		TargetRef: binding.TargetRef, Commit: strings.TrimSpace(string(output)), Source: source,
		ProviderRequest: "github:certissuer-test", ObservedAt: v.now}, nil
}

type failFinalizeOnce struct {
	gitprojection.Store
	fail bool
}

func (s *failFinalizeOnce) FinalizePath(ctx context.Context, bindingID, targetRef, documentPath,
	operationID, committedRevision string, now time.Time) (gitprojection.PathReservation, error) {
	if s.fail {
		s.fail = false
		return gitprojection.PathReservation{}, errors.New("simulated lost database receipt")
	}
	return s.Store.FinalizePath(ctx, bindingID, targetRef, documentPath, operationID, committedRevision, now)
}

type protectedFixture struct {
	remote, seed, base string
	now                time.Time
	binding            gitprojection.Binding
	store              *gitprojection.MemoryStore
	verifier           *protectedHeadVerifier
	manager            *gitprojection.MirrorManager
	desired            Desired
}

func newProtectedFixture(t *testing.T, initialPath string, initial []byte) protectedFixture {
	t.Helper()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	remote, seed := filepath.Join(t.TempDir(), "remote.git"), filepath.Join(t.TempDir(), "seed")
	runProtectedGit(t, "", "init", "--bare", remote)
	runProtectedGit(t, "", "init", seed)
	runProtectedGit(t, seed, "config", "user.name", "Kuberploy Test")
	runProtectedGit(t, seed, "config", "user.email", "test@kuberploy.invalid")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("platform\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if initialPath != "" {
		full := filepath.Join(seed, filepath.FromSlash(initialPath))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, initial, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runProtectedGit(t, seed, "add", ".")
	runProtectedGit(t, seed, "commit", "-m", "seed")
	runProtectedGit(t, seed, "branch", "-M", "main")
	runProtectedGit(t, seed, "remote", "add", "origin", remote)
	runProtectedGit(t, seed, "push", "origin", "main")
	base := strings.TrimSpace(runProtectedGit(t, seed, "rev-parse", "HEAD"))
	repository := gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 1, RepositoryID: 2, Owner: "kuberploy", Name: "platform"}
	binding, err := gitprojection.NewGitHubPlatformBinding(protectedTestBinding, repository, "refs/heads/main", now)
	if err != nil {
		t.Fatal(err)
	}
	binding.State, binding.TargetHeadRevision, binding.IndexedRevision, binding.ProjectionGeneration =
		gitprojection.BindingReady, base, base, 1
	binding.TargetHeadObservedAt, binding.IndexedAt, binding.UpdatedAt = now.Add(time.Second), now.Add(time.Second), now.Add(time.Second)
	store := gitprojection.NewMemoryStore()
	if err = store.PutBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "git-cache")
	if err = os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := httpSpec()
	clean, solver, digest, err := normalizeSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	desired := Desired{ProfileID: "33333333-3333-4333-8333-333333333333", Name: "letsencrypt-http",
		SpecDigest: digest, Revision: 1, Solver: solver, Spec: clean}
	return protectedFixture{remote, seed, base, now, binding, store,
		&protectedHeadVerifier{remote: remote, now: now.Add(2 * time.Second)},
		&gitprojection.MirrorManager{Root: root, AllowLocalTests: true, LocalRemote: remote}, desired}
}

func (f protectedFixture) publisher(t *testing.T, store gitprojection.Store) *ProtectedGitPublisher {
	t.Helper()
	publisher, err := NewProtectedGitPublisher(store, f.verifier, f.manager,
		ProtectedGitConfig{BindingID: protectedTestBinding, Owner: "certissuer-worker:test",
			ObserverNamespace: protectedTestObserverNamespace, ObserverServiceAccount: protectedTestObserverServiceAccount},
		func() time.Time { return f.now.Add(10 * time.Second) })
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}

func TestRenderClusterIssuerIsClosedAndDeterministic(t *testing.T) {
	fixture := newProtectedFixture(t, "", nil)
	first, err := RenderClusterIssuer(fixture.desired)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := RenderClusterIssuer(fixture.desired)
	if string(first) != string(second) {
		t.Fatal("render is nondeterministic")
	}
	for _, required := range []string{
		"kind: ClusterIssuer", `server: "` + LetsEncryptProduction + `"`, `ingressClassName: "traefik"`,
		`external-dns.alpha.kubernetes.io/exclude: "true"`,
		`external-dns.alpha.kubernetes.io/ingress-hostname-source: "annotation-only"`, fixture.desired.SpecDigest,
	} {
		if !strings.Contains(string(first), required) {
			t.Fatalf("render missing %q:\n%s", required, first)
		}
	}
	for _, forbidden := range []string{"webhook:", "route53:", "ingressClassName: nginx"} {
		if strings.Contains(string(first), forbidden) {
			t.Fatalf("render contains forbidden %q", forbidden)
		}
	}
	bundle, err := RenderProtectedClusterIssuerBundle(fixture.desired, protectedTestObserver)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"kind: ClusterRole\n", "kind: ClusterRoleBinding\n", `- "clusterissuers"`,
		`- "` + fixture.desired.Name + `"`, `- "get"`, `name: "` + protectedTestObserverServiceAccount + `"`,
		`namespace: "` + protectedTestObserverNamespace + `"`} {
		if !strings.Contains(string(bundle), required) {
			t.Fatalf("bundle missing %q:\n%s", required, bundle)
		}
	}
	for _, forbidden := range []string{`- "list"`, `- "watch"`, `- "create"`, `- "patch"`, `- "delete"`, `- "secrets"`} {
		if strings.Contains(string(bundle), forbidden) {
			t.Fatalf("bundle contains forbidden RBAC %q:\n%s", forbidden, bundle)
		}
	}
	otherObserver := ProtectedObserverSubject{Namespace: "other-system", ServiceAccount: protectedTestObserverServiceAccount}
	otherBundle, err := RenderProtectedClusterIssuerBundle(fixture.desired, otherObserver)
	if err != nil || protectedDigest(bundle) == protectedDigest(otherBundle) {
		t.Fatalf("observer subject is not covered by bundle digest: err=%v", err)
	}
	baseConfig := ProtectedGitConfig{BindingID: protectedTestBinding, Owner: "certissuer-worker:test",
		ObserverNamespace: protectedTestObserver.Namespace, ObserverServiceAccount: protectedTestObserver.ServiceAccount}
	otherConfig := baseConfig
	otherConfig.ObserverNamespace = otherObserver.Namespace
	if protectedOperationID(baseConfig, fixture.desired, gitprojection.MutationUpsert, protectedDigest(bundle)) ==
		protectedOperationID(otherConfig, fixture.desired, gitprojection.MutationUpsert, protectedDigest(otherBundle)) {
		t.Fatal("observer subject is not covered by operation identity")
	}

	dns := dnsSpec("Corp.Example.net", "example.com")
	clean, solver, digest, _ := normalizeSpec(dns)
	desired := Desired{Name: "letsencrypt-cloudflare", Revision: 7, Solver: solver, SpecDigest: digest, Spec: clean}
	raw, err := RenderClusterIssuer(desired)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(raw), `"corp.example.net"`) > strings.Index(string(raw), `"example.com"`) ||
		!strings.Contains(string(raw), "dns01:\n          cloudflare:") || strings.Contains(string(raw), "http01:") {
		t.Fatalf("unexpected DNS-01 render:\n%s", raw)
	}

	bad := fixture.desired
	bad.SpecDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err = RenderClusterIssuer(bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("substituted digest accepted: %v", err)
	}
}

func TestProtectedGitConfigDerivesExactObserverSubject(t *testing.T) {
	observer := ObserverConfig{Enabled: true, BindingID: protectedTestBinding, Namespace: protectedTestObserverNamespace, ServiceAccount: protectedTestObserverServiceAccount,
		PollInterval: 30 * time.Second, RequestTimeout: 10 * time.Second, MaximumAge: 2 * time.Minute, ReadinessLease: 3 * time.Minute}
	config, err := ProtectedGitConfigForObserver("certissuer-worker:test", observer)
	if err != nil || config.BindingID != observer.BindingID ||
		config.ObserverNamespace != observer.Namespace || config.ObserverServiceAccount != observer.ServiceAccount || config.Validate() != nil {
		t.Fatalf("config=%#v err=%v", config, err)
	}
	observer.Enabled = false
	if _, err = ProtectedGitConfigForObserver("certissuer-worker:test", observer); !errors.Is(err, ErrInvalid) {
		t.Fatalf("disabled observer accepted: %v", err)
	}
}

func TestProtectedPublisherPersistsCASRecoversPushAndRereadsProvider(t *testing.T) {
	fixture := newProtectedFixture(t, "", nil)
	crashing := &failFinalizeOnce{Store: fixture.store, fail: true}
	publisher := fixture.publisher(t, crashing)
	if _, err := publisher.Materialize(t.Context(), fixture.desired); !errors.Is(err, ErrMaterializationUnavailable) {
		t.Fatalf("lost DB receipt error=%v", err)
	}
	path := protectedIssuerPath(fixture.desired.Name)
	written := runProtectedGit(t, fixture.seed, "--git-dir", fixture.remote, "show", "refs/heads/main:"+path)
	want, _ := RenderProtectedClusterIssuerBundle(fixture.desired, protectedTestObserver)
	if written != string(want) {
		t.Fatalf("published bytes differ\nwant:\n%s\ngot:\n%s", want, written)
	}
	receipt, err := publisher.Materialize(t.Context(), fixture.desired)
	if err != nil || !receipt.Changed || receipt.ParentRevision != fixture.base || receipt.CommittedRevision == fixture.base ||
		receipt.ProviderHead != receipt.CommittedRevision || fixture.verifier.calls < 4 {
		t.Fatalf("recovery receipt=%#v calls=%d err=%v", receipt, fixture.verifier.calls, err)
	}
	message := runProtectedGit(t, "", "--git-dir", fixture.remote, "show", "-s", "--format=%B", receipt.CommittedRevision)
	for _, trailer := range []string{"Kuberploy-Operation: " + receipt.OperationID, "Kuberploy-Certificate-Issuer-Intent: " + receipt.OperationID} {
		if strings.Count(message, trailer) != 1 {
			t.Fatalf("missing/duplicate trailer %q:\n%s", trailer, message)
		}
	}
}

func TestProtectedPublisherRejectsDescendantPathSubstitutionDuringRecovery(t *testing.T) {
	fixture := newProtectedFixture(t, "", nil)
	crashing := &failFinalizeOnce{Store: fixture.store, fail: true}
	publisher := fixture.publisher(t, crashing)
	if _, err := publisher.Materialize(t.Context(), fixture.desired); !errors.Is(err, ErrMaterializationUnavailable) {
		t.Fatalf("lost DB receipt error=%v", err)
	}
	documentPath := protectedIssuerPath(fixture.desired.Name)
	appendProtectedRemote(t, fixture.remote, documentPath, []byte("apiVersion: attacker.invalid/v1\nkind: ClusterIssuer\n"))
	if _, err := publisher.Materialize(t.Context(), fixture.desired); !errors.Is(err, ErrConflict) {
		t.Fatalf("descendant substitution err=%v", err)
	}
}

func TestProtectedControllerDematerializesOnlyExactCatalogPreimage(t *testing.T) {
	desiredFixture := newProtectedFixture(t, "", nil)
	documentPath := protectedIssuerPath(desiredFixture.desired.Name)

	t.Run("exact delete", func(t *testing.T) {
		catalog := NewMemoryStore()
		created, err := catalog.Create(t.Context(), command("delete-create", desiredFixture.now), desiredFixture.desired.Name, desiredFixture.desired.Spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = catalog.Deactivate(t.Context(), command("delete-deactivate", desiredFixture.now.Add(time.Second)), Ref{ProfileID: created.Profile.ID, Revision: 1}); err != nil {
			t.Fatal(err)
		}
		pending, _ := catalog.PendingDematerialization(t.Context(), 20)
		rendered, _ := RenderProtectedClusterIssuerBundle(pending[0], protectedTestObserver)
		fixture := newProtectedFixture(t, documentPath, rendered)
		controller := ProtectedController{Store: catalog, Publisher: fixture.publisher(t, fixture.store)}
		result, err := controller.Reconcile(t.Context(), 20)
		if err != nil || len(result.Materialized) != 0 || len(result.Dematerialized) != 1 || !result.Dematerialized[0].Changed {
			t.Fatalf("result=%#v err=%v", result, err)
		}
		if command := exec.Command("git", "--git-dir", fixture.remote, "cat-file", "-e", "refs/heads/main:"+documentPath); command.Run() == nil {
			t.Fatal("ClusterIssuer path still exists")
		}
	})

	t.Run("substituted preimage", func(t *testing.T) {
		catalog := NewMemoryStore()
		created, _ := catalog.Create(t.Context(), command("sub-create", desiredFixture.now), desiredFixture.desired.Name, desiredFixture.desired.Spec)
		_, _ = catalog.Deactivate(t.Context(), command("sub-deactivate", desiredFixture.now.Add(time.Second)), Ref{ProfileID: created.Profile.ID, Revision: 1})
		pending, _ := catalog.PendingDematerialization(t.Context(), 20)
		rendered, _ := RenderProtectedClusterIssuerBundle(pending[0], protectedTestObserver)
		fixture := newProtectedFixture(t, documentPath, append(rendered, []byte("# attacker\n")...))
		controller := ProtectedController{Store: catalog, Publisher: fixture.publisher(t, fixture.store)}
		if _, err := controller.Reconcile(t.Context(), 20); !errors.Is(err, ErrConflict) {
			t.Fatalf("substituted preimage err=%v", err)
		}
		if command := exec.Command("git", "--git-dir", fixture.remote, "cat-file", "-e", "refs/heads/main:"+documentPath); command.Run() != nil {
			t.Fatal("substituted path was deleted")
		}
	})
}

func runProtectedGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func appendProtectedRemote(t *testing.T, remote, documentPath string, content []byte) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "append")
	runProtectedGit(t, "", "clone", "--branch", "main", remote, work)
	runProtectedGit(t, work, "config", "user.name", "Kuberploy Test")
	runProtectedGit(t, work, "config", "user.email", "test@kuberploy.invalid")
	full := filepath.Join(work, filepath.FromSlash(documentPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o600); err != nil {
		t.Fatal(err)
	}
	runProtectedGit(t, work, "add", "--", documentPath)
	runProtectedGit(t, work, "commit", "-m", "substitute protected path")
	runProtectedGit(t, work, "push", "origin", "main")
}
