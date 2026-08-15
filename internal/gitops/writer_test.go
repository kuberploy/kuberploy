package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func fixture() (domain.Operation, domain.Project, domain.Environment, domain.Application, domain.Deployment) {
	now := time.Now()
	p := domain.Project{ID: "11111111-1111-4111-8111-111111111111", Slug: "demo"}
	e := domain.Environment{ID: "22222222-2222-4222-8222-222222222222", ProjectID: p.ID, Slug: "dev"}
	a := domain.Application{ID: "33333333-3333-4333-8333-333333333333", ProjectID: p.ID, Name: "Hello", Slug: "hello"}
	op := domain.Operation{ID: "44444444-4444-4444-8444-444444444444", Generation: 1, CreatedAt: now}
	d := domain.Deployment{ID: "55555555-5555-4555-8555-555555555555", EnvironmentID: e.ID, ApplicationID: a.ID, Image: "registry.test/hello@sha256:" + strings.Repeat("a", 64), Replicas: 2, Port: 8080, Environment: map[string]string{"Z_LAST": "z", "A_FIRST": "a"}, Route: &domain.Route{Hostname: "hello.example.com", PathPrefix: "/", TLSMode: "httpOnly"}}
	return op, p, e, a, d
}
func TestRenderAppConfigIsCanonicalAndIdentityBound(t *testing.T) {
	_, p, e, a, d := fixture()
	body, err := RenderAppConfig(p, e, a, d)
	if err != nil {
		t.Fatal(err)
	}
	v := string(body)
	for _, want := range []string{"id: \"" + a.ID + "\"", "projectId: \"" + p.ID + "\"", "environmentId: \"" + e.ID + "\"", "repository: \"registry.test/hello\"", "digest: \"sha256:", "ingressClassName: \"traefik\"", "mode: httpOnly"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in:\n%s", want, v)
		}
	}
	if strings.Index(v, "A_FIRST") > strings.Index(v, "Z_LAST") {
		t.Fatal("environment variables are not deterministic")
	}
}

func TestRenderAppConfigIncludesOnlyLockedRegistryPullMetadata(t *testing.T) {
	_, p, e, a, d := fixture()
	d.RegistryPull = &domain.RegistryPullReference{
		TargetID:        "77777777-7777-4777-8777-777777777777",
		ProfileName:     "managed-main",
		ProfileRevision: 7,
	}
	body, err := RenderAppConfig(p, e, a, d)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"    registryPull:\n",
		`      targetId: "77777777-7777-4777-8777-777777777777"`,
		`      profileName: "managed-main"`,
		"      profileRevision: 7\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("locked registry pull metadata %q missing:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"credentialRef", "sourceSecret", "secretName", ".dockerconfigjson"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("credential-bearing registry metadata %q entered AppConfig:\n%s", forbidden, text)
		}
	}
	if strings.Index(text, "    registryPull:") > strings.Index(text, "    release:") {
		t.Fatalf("delivery fields are not rendered canonically:\n%s", text)
	}
}

func TestRenderAppConfigRejectsInvalidRegistryPullMetadata(t *testing.T) {
	_, p, e, a, d := fixture()
	for name, reference := range map[string]domain.RegistryPullReference{
		"invalid target":  {TargetID: "not-a-uuid", ProfileName: "managed-main", ProfileRevision: 1},
		"invalid profile": {TargetID: "77777777-7777-4777-8777-777777777777", ProfileName: "Managed_Main", ProfileRevision: 1},
		"zero revision":   {TargetID: "77777777-7777-4777-8777-777777777777", ProfileName: "managed-main"},
	} {
		t.Run(name, func(t *testing.T) {
			d.RegistryPull = &reference
			if _, err := RenderAppConfig(p, e, a, d); err == nil || !strings.Contains(err.Error(), "registry pull reference") {
				t.Fatalf("invalid registry pull reference accepted: %#v err=%v", reference, err)
			}
		})
	}
}

func TestRenderAppConfigUsesConfiguredIngressClass(t *testing.T) {
	_, p, e, a, d := fixture()
	body, err := renderAppConfig(p, e, a, d, "kp-e2e-run-42")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `ingressClassName: "kp-e2e-run-42"`) {
		t.Fatalf("configured ingress class missing:\n%s", body)
	}
}

func TestRenderAppConfigCarriesOnlyClosedSSLIPIntent(t *testing.T) {
	_, p, e, a, d := fixture()
	d.Route = &domain.Route{Hostname: "kp-32f2a03ad59c10f96a1a.8-8-8-8.sslip.io", PathPrefix: "/", TLSMode: "httpOnly", DNSMode: "sslip"}
	body, err := RenderAppConfig(p, e, a, d)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "      dns:\n        mode: sslip\n") || strings.Contains(text, "integrationRef") || strings.Contains(text, "publicIPv4") {
		t.Fatalf("unsafe sslip AppConfig:\n%s", text)
	}
	d.Route.DNSMode = "caller-mode"
	if _, err = RenderAppConfig(p, e, a, d); err == nil {
		t.Fatal("unknown DNS mode was accepted")
	}
}

func TestRenderAppConfigRoutesToFirstTCPPortByName(t *testing.T) {
	_, p, e, a, d := fixture()
	d.Runtime = domain.NormalizeWorkloadRuntime(domain.WorkloadRuntime{
		Replicas: 1,
		Ports: []domain.WorkloadPort{
			{Name: "metrics", ContainerPort: 9090, Protocol: "UDP"},
			{Name: "web", ContainerPort: 8080, Protocol: "TCP"},
		},
	})
	body, err := RenderAppConfig(p, e, a, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "port: \"web\"") {
		t.Fatalf("route did not select first TCP runtime port:\n%s", body)
	}
}

func TestRenderAppConfigRejectsPublicRouteForUDPOnlyRuntime(t *testing.T) {
	_, p, e, a, d := fixture()
	d.Runtime = domain.NormalizeWorkloadRuntime(domain.WorkloadRuntime{
		Replicas: 1,
		Ports:    []domain.WorkloadPort{{Name: "dns", ContainerPort: 5353, Protocol: "UDP"}},
	})
	if _, err := RenderAppConfig(p, e, a, d); err == nil || !strings.Contains(err.Error(), "TCP") {
		t.Fatalf("expected UDP-only route rejection, got %v", err)
	}
}

func TestWriterRejectsSchemaValidButTamperedOperationBindingsBeforeGit(t *testing.T) {
	op, p, e, a, d := fixture()
	original, err := RenderAppConfig(p, e, a, d)
	if err != nil {
		t.Fatal(err)
	}
	for name, tampered := range map[string][]byte{
		"digest":   []byte(strings.Replace(string(original), strings.Repeat("a", 64), strings.Repeat("b", 64), 1)),
		"identity": []byte(strings.Replace(string(original), a.ID, "99999999-9999-4999-8999-999999999999", 1)),
	} {
		t.Run(name, func(t *testing.T) {
			candidate := d
			candidate.ConfigRaw = tampered
			root := filepath.Join(t.TempDir(), "must-not-initialize")
			if _, writeErr := (&Writer{Root: root}).Write(t.Context(), op, p, e, a, candidate); writeErr == nil || !strings.Contains(writeErr.Error(), "server-owned identity or release state") {
				t.Fatalf("tampered binding accepted: %v", writeErr)
			}
			if _, statErr := os.Stat(filepath.Join(root, ".git")); !os.IsNotExist(statErr) {
				t.Fatalf("invalid snapshot reached Git initialization: %v", statErr)
			}
		})
	}
}

func TestRenderedAppConfigPassesRuntimeChartSchemaWhenHelmAvailable(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	_, p, e, a, d := fixture()
	plain := "info"
	terminationGrace := 45
	d.Runtime = domain.NormalizeWorkloadRuntime(domain.WorkloadRuntime{
		Replicas:                      1,
		Command:                       []string{"/app/server"},
		Args:                          []string{"--listen=:8080"},
		TerminationGracePeriodSeconds: &terminationGrace,
		Ports:                         []domain.WorkloadPort{{Name: "web", ContainerPort: 8080}},
		Env: []domain.WorkloadEnv{
			{Name: "LOG_LEVEL", Value: &plain},
			{Name: "DATABASE_PASSWORD", ValueFrom: &domain.WorkloadEnvValueFrom{SecretBindingRef: domain.SecretBindingRef{BindingID: "44444444-4444-4444-8444-444444444444", Name: "database", Key: "password", Version: 3}}},
		},
		Resources:    domain.WorkloadResources{Limits: &domain.ResourceList{CPU: "500m"}},
		NodeSelector: map[string]string{"kubernetes.io/arch": "amd64"},
	})
	body, err := RenderAppConfig(p, e, a, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"command:", `- "/app/server"`, "args:", `- "--listen=:8080"`, "terminationGracePeriodSeconds: 45"} {
		if !strings.Contains(string(body), expected) {
			t.Fatalf("rendered AppConfig omitted %q:\n%s", expected, body)
		}
	}
	values := filepath.Join(t.TempDir(), "app.yaml")
	if err = os.WriteFile(values, body, 0o600); err != nil {
		t.Fatal(err)
	}
	chart := filepath.Join("..", "..", "charts", "kuberploy-runtime")
	cmd := exec.Command(helm, "template", "contract", chart, "-f", values,
		"--set-string", "kuberployExpectedIdentity.projectId="+p.ID,
		"--set-string", "kuberployExpectedIdentity.environmentId="+e.ID,
		"--set-string", "kuberployExpectedIdentity.applicationId="+a.ID)
	if output, commandErr := cmd.CombinedOutput(); commandErr != nil {
		t.Fatalf("rendered AppConfig failed chart validation: %v\n%s\nAppConfig:\n%s", commandErr, output, body)
	}
}
func TestWriterUsesOneCommitPerOperationAndPushesNormally(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("git", "init", "--bare", bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v %s", err, out)
	}
	root := filepath.Join(t.TempDir(), "work")
	op, p, e, a, d := fixture()
	w := &Writer{Root: root, Remote: bare, Branch: "main", AllowLocalTransport: true}
	first, err := w.Write(ctx, op, p, e, a, d)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.Write(ctx, op, p, e, a, d)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("retry made a different commit: %s != %s", first, second)
	}
	count := gitOut(t, root, "rev-list", "--count", "HEAD")
	if strings.TrimSpace(count) != "1" {
		t.Fatalf("commit count=%s", count)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	cmd = exec.Command("git", "clone", "--branch", "main", bare, clone)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	path := filepath.Join(clone, "environments", e.ID, "apps", a.ID, "app.yaml")
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("pushed AppConfig missing: %v", err)
	}
	message := gitOut(t, clone, "log", "-1", "--format=%B")
	if !strings.Contains(message, "Kuberploy-Operation: "+op.ID) {
		t.Fatal("commit lacks operation idempotency trailer")
	}
}

func TestWriterRejectsPreemptedOperationMarkerWithDifferentContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	op, p, e, a, d := fixture()
	w := &Writer{Root: root}
	if _, err := w.Write(t.Context(), op, p, e, a, d); err != nil {
		t.Fatal(err)
	}
	preempted := op
	preempted.ID = "77777777-7777-4777-8777-777777777777"
	if err := os.WriteFile(filepath.Join(root, "unrelated.txt"), []byte("not the operation\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	gitOut(t, root, "add", "unrelated.txt")
	gitOut(t, root, "commit", "-m", "spoofed retry marker\n\nKuberploy-Operation: "+preempted.ID)
	d.Image = "registry.test/hello@sha256:" + strings.Repeat("b", 64)
	if _, err := w.Write(t.Context(), preempted, p, e, a, d); err == nil || !strings.Contains(err.Error(), "operation marker does not identify") {
		t.Fatalf("preempted operation marker was trusted: %v", err)
	}
}

func TestWriterBypassesRepositoryFiltersAndDisablesHooks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	w := &Writer{Root: root}
	if err := w.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("*.yaml filter=evil\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	gitOut(t, root, "add", ".gitattributes")
	gitOut(t, root, "commit", "-m", "add hostile attributes")
	hooks := filepath.Join(root, "hostile-hooks")
	if err := os.MkdirAll(hooks, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nexit 97\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	gitOut(t, root, "config", "core.hooksPath", hooks)
	gitOut(t, root, "config", "filter.evil.clean", "false")
	gitOut(t, root, "config", "filter.evil.required", "true")
	op, p, e, a, d := fixture()
	revision, err := w.Write(t.Context(), op, p, e, a, d)
	if err != nil {
		t.Fatalf("repository-controlled filter or hook affected trusted write: %v", err)
	}
	expected, err := RenderAppConfig(p, e, a, d)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.ToSlash(filepath.Join("environments", e.ID, "apps", a.ID, "app.yaml"))
	if actual := gitOut(t, root, "cat-file", "blob", revision+":"+path); actual != string(expected) {
		t.Fatalf("committed bytes passed through a repository filter:\n%s", actual)
	}
	localConfig, err := os.ReadFile(filepath.Join(root, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(localConfig) != trustedLocalConfig {
		t.Fatalf("repository-controlled local config survived trusted write:\n%s", localConfig)
	}
}

func TestWriterRejectsSymlinkedAppConfigAncestors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	outside := filepath.Join(t.TempDir(), "outside")
	w := &Writer{Root: root}
	if err := w.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "environments"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	op, p, e, a, d := fixture()
	if err := os.Symlink(outside, filepath.Join(root, "environments", e.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(t.Context(), op, p, e, a, d); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlinked AppConfig ancestor was accepted: %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("writer escaped through symlink: entries=%v err=%v", entries, err)
	}
}

func TestWriterDropsUnrelatedStagedContentFromOperationCommit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	w := &Writer{Root: root}
	op, p, e, a, d := fixture()
	if _, err := w.Write(t.Context(), op, p, e, a, d); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "staged-by-other-process"), []byte("must not commit\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	gitOut(t, root, "add", "staged-by-other-process")
	op.ID = "88888888-8888-4888-8888-888888888888"
	d.Image = "registry.test/hello@sha256:" + strings.Repeat("d", 64)
	revision, err := w.Write(t.Context(), op, p, e, a, d)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "cat-file", "-e", revision+":staged-by-other-process")
	cmd.Dir = root
	if err = cmd.Run(); err == nil {
		t.Fatal("unrelated staged path was smuggled into operation commit")
	}
}

func TestWriterRejectsCredentialedOrNonHTTPSProductionRemoteBeforeMutation(t *testing.T) {
	for name, remote := range map[string]string{
		"credential": "https://token@example.test/platform.git",
		"plain HTTP": "http://example.test/platform.git",
		"local path": filepath.Join(t.TempDir(), "remote.git"),
	} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "must-not-exist")
			if err := (&Writer{Root: root, Remote: remote}).Ensure(t.Context()); err == nil {
				t.Fatal("unsafe production remote was accepted")
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("invalid remote mutated the worktree: %v", err)
			}
		})
	}
}

func TestWriterRejectsAuthorHeaderInjectionBeforeMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-not-exist")
	w := &Writer{Root: root, AuthorName: "Kuberploy\nInjected: true", AuthorEmail: "gitops@kuberploy.local"}
	if err := w.Ensure(t.Context()); err == nil {
		t.Fatal("invalid Git author identity was accepted")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("invalid identity mutated the worktree: %v", err)
	}
}

func TestWriterRejectsCredentialFilesWithoutProductionHTTPSRemoteBeforeMutation(t *testing.T) {
	for name, writer := range map[string]*Writer{
		"no remote":    {Root: filepath.Join(t.TempDir(), "must-not-exist"), UseCredentialFiles: true},
		"local remote": {Root: filepath.Join(t.TempDir(), "must-not-exist"), Remote: filepath.Join(t.TempDir(), "remote.git"), AllowLocalTransport: true, UseCredentialFiles: true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := writer.Ensure(t.Context()); err == nil || !strings.Contains(err.Error(), "production HTTPS remote") {
				t.Fatalf("unsafe credential transport accepted: %v", err)
			}
			if _, err := os.Stat(writer.Root); !os.IsNotExist(err) {
				t.Fatalf("invalid credential mode mutated the worktree: %v", err)
			}
		})
	}
}

func TestWriterSafelyRebasesUnrelatedRemoteAdvanceBeforeNormalPush(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("git", "init", "--bare", bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v %s", err, out)
	}
	root := filepath.Join(t.TempDir(), "writer-a")
	op, p, e, a, d := fixture()
	w := &Writer{Root: root, Remote: bare, Branch: "main", AllowLocalTransport: true}
	if _, err := w.Write(ctx, op, p, e, a, d); err != nil {
		t.Fatal(err)
	}
	// Leave the next deterministic operation commit local, as if its first push
	// lost a race with another safe writer.
	op2 := op
	op2.ID = "66666666-6666-4666-8666-666666666666"
	d.Image = "registry.test/hello@sha256:" + strings.Repeat("c", 64)
	content, err := RenderAppConfig(p, e, a, d)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "environments", e.ID, "apps", a.ID, "app.yaml")
	if err = os.WriteFile(path, content, 0o640); err != nil {
		t.Fatal(err)
	}
	gitOut(t, root, "add", "--", filepath.ToSlash(filepath.Join("environments", e.ID, "apps", a.ID, "app.yaml")))
	gitOut(t, root, "commit", "-m", "local operation\n\nKuberploy-Operation: "+op2.ID)
	other := filepath.Join(t.TempDir(), "writer-b")
	cmd = exec.Command("git", "clone", "--branch", "main", bare, other)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	gitOut(t, other, "config", "user.name", "Other Writer")
	gitOut(t, other, "config", "user.email", "other@example.test")
	if err = os.MkdirAll(filepath.Join(other, "platform"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(other, "platform", "unrelated.txt"), []byte("unrelated\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	gitOut(t, other, "add", "platform/unrelated.txt")
	gitOut(t, other, "commit", "-m", "unrelated platform update")
	gitOut(t, other, "push", "origin", "HEAD:refs/heads/main")
	revision, err := w.Write(ctx, op2, p, e, a, d)
	if err != nil {
		t.Fatal(err)
	}
	verify := filepath.Join(t.TempDir(), "verify")
	cmd = exec.Command("git", "clone", "--branch", "main", bare, verify)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("verify clone: %v %s", err, out)
	}
	if _, err = os.Stat(filepath.Join(verify, "platform", "unrelated.txt")); err != nil {
		t.Fatal("unrelated remote commit was lost")
	}
	remoteRevision := strings.TrimSpace(gitOut(t, verify, "log", "-1", "--format=%H", "--fixed-strings", "--grep=Kuberploy-Operation: "+op2.ID))
	if revision != remoteRevision {
		t.Fatalf("returned stale pre-rebase revision %s; remote has %s", revision, remoteRevision)
	}
	if count := strings.TrimSpace(gitOut(t, verify, "rev-list", "--count", "HEAD")); count != "3" {
		t.Fatalf("expected base, unrelated, and rebased operation commits; got %s", count)
	}
}
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	commandArgs := []string{"-c", "user.name=Kuberploy Test", "-c", "user.email=test@kuberploy.local"}
	commandArgs = append(commandArgs, args...)
	cmd := exec.Command("git", commandArgs...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v %s", args[0], err, out)
	}
	return string(out)
}
