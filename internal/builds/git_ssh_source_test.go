package builds

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
	"github.com/kuberploy/kuberploy/internal/gitssh"
)

func validGitSSHDefinition(t *testing.T) BuildDefinition {
	t.Helper()
	definition := validDefinition(t, testNow, RegistryExternal)
	definition.SourceKind = SourceGitSSH
	definition.InstallationID = ""
	definition.RepositoryID = ""
	definition.GitSSH = &GitSSHSource{
		RepositoryURL: "ssh://git@git.example.test/team/repository.git",
		ApprovedHost:  "git.example.test",
		KeyScope:      "app",
		KeyOwnerID:    definition.ServiceID,
		KeyRevision:   1,
		KnownHosts:    "git.example.test ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixture",
	}
	prepared, err := PrepareDefinition(definition, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestKubernetesWorkloadUsesOnlyGitSSHCredentials(t *testing.T) {
	definition := validGitSSHDefinition(t)
	attempt, err := newAttempt(definition, Repository{}, EnqueuePush{
		ClaimKey: "sha256:" + strings.Repeat("b", 64), CommitSHA: strings.Repeat("a", 40), GitRef: definition.TriggerRef, ResolvedAt: testNow,
	}, 1, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &KubernetesAdapter{namespace: attempt.JobNamespace}
	plan, err := builder.PlanJob(attempt.PlanRequest)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfixture\n-----END OPENSSH PRIVATE KEY-----\n")
	desired, err := adapter.desiredWorkload(BuildWorkload{
		Attempt: attempt, Plan: plan, CheckoutRequest: attempt.CheckoutRequest, InputDigest: attempt.InputDigest,
		SSHPrivateKey: privateKey, SSHKnownHosts: []byte(definition.GitSSH.KnownHosts),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	data := desired.sourceSecret["data"].(map[string]any)
	if len(data) != 2 || data["username"] != nil || data["token"] != nil {
		t.Fatalf("mixed source credential Secret: %#v", data)
	}
	decoded, err := base64.StdEncoding.DecodeString(data["ssh-private-key"].(string))
	if err != nil || string(decoded) != string(privateKey) {
		t.Fatal("SSH private key was not bound exactly")
	}
}

type fakeGitSSHCredentials struct {
	metadata   gitssh.KeyMetadata
	privateKey []byte
}

func (f *fakeGitSSHCredentials) Active(context.Context, gitssh.Scope, string) (gitssh.KeyMetadata, error) {
	return f.metadata, nil
}

func (f *fakeGitSSHCredentials) PrivateKey(context.Context, gitssh.Scope, string) ([]byte, error) {
	return append([]byte(nil), f.privateKey...), nil
}

func TestControllerRunsGitSSHWithoutGitHubProvider(t *testing.T) {
	definition := validGitSSHDefinition(t)
	store := NewMemoryStore()
	if err := store.PutDefinition(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	attempt, err := newAttempt(definition, Repository{}, EnqueuePush{
		ClaimKey: "sha256:" + strings.Repeat("b", 64), CommitSHA: strings.Repeat("a", 40), GitRef: definition.TriggerRef, ResolvedAt: testNow,
	}, 1, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.attempts[attempt.ID] = attempt
	store.mu.Unlock()
	credentials := &fakeGitSSHCredentials{metadata: gitssh.KeyMetadata{
		Scope: gitssh.ScopeApp, OwnerID: definition.ServiceID, Revision: definition.GitSSH.KeyRevision, Status: gitssh.StatusActive,
	}, privateKey: []byte("private-key-fixture")}
	kube := &fakeKubernetes{state: WorkloadSucceeded, promoted: true}
	clock := testNow
	controller := &BuildController{Store: store, GitSSH: credentials, Kubernetes: kube, Owner: "git-ssh-controller", LeaseDuration: time.Minute, Now: func() time.Time { return clock }}
	result, err := controller.ReconcileNext(t.Context())
	if err != nil || result.State != AttemptSucceeded || len(kube.workloads) != 1 {
		t.Fatalf("result=%#v workloads=%d err=%v", result, len(kube.workloads), err)
	}
	if kube.workloads[0].SourceUsername != "" || kube.workloads[0].SourceCredential.Reveal() != "" || len(kube.workloads[0].SSHKnownHosts) == 0 {
		t.Fatalf("mixed or missing Git SSH workload credentials: %#v", kube.workloads[0])
	}
}

func TestGitSSHDefinitionProducesPinnedCheckout(t *testing.T) {
	definition := validGitSSHDefinition(t)
	commit := strings.Repeat("a", 40)
	attempt, err := newAttempt(definition, Repository{}, EnqueuePush{
		ClaimKey: "sha256:" + strings.Repeat("b", 64), CommitSHA: commit, GitRef: definition.TriggerRef, ResolvedAt: testNow,
	}, 1, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkout := attempt.CheckoutRequest
	if checkout.RepositoryURL != definition.GitSSH.RepositoryURL || checkout.ApprovedHost != definition.GitSSH.ApprovedHost ||
		checkout.SSHPrivateKeyFile != builder.SourceSSHPrivateKeyFile || checkout.SSHKnownHostsFile != builder.SourceSSHKnownHostsFile ||
		checkout.UsernameFile != "" || checkout.AccessTokenFile != "" {
		t.Fatalf("unexpected Git SSH checkout: %#v", checkout)
	}
	originalDigest := definition.DefinitionDigest
	definition.GitSSH.RepositoryURL = "ssh://git@git.example.test/team/other.git"
	if err := definition.validate(); err == nil {
		t.Fatal("Git SSH source changed without changing definition digest")
	}
	if originalDigest == "" {
		t.Fatal("definition digest is empty")
	}
}

func TestGitSSHDefinitionRejectsInvalidBindings(t *testing.T) {
	valid := validGitSSHDefinition(t)
	cases := []BuildDefinition{valid, valid, valid, valid, valid, valid}
	cases[0].GitSSH = nil
	cases[1].GitSSH = cloneGitSSHSource(valid.GitSSH)
	cases[1].GitSSH.KeyOwnerID = valid.ProjectID
	cases[2].GitSSH = cloneGitSSHSource(valid.GitSSH)
	cases[2].GitSSH.RepositoryURL = "ssh://git:password@git.example.test/team/repository.git"
	cases[3].GitSSH = cloneGitSSHSource(valid.GitSSH)
	cases[3].GitSSH.ApprovedHost = "attacker.example.test"
	cases[4].GitSSH = cloneGitSSHSource(valid.GitSSH)
	cases[4].GitSSH.KnownHosts = "attacker.example.test ssh-ed25519 AAAAFixture"
	cases[5].InstallationID = testInstallationID
	for index, candidate := range cases {
		if _, err := PrepareDefinition(candidate, testNow); err == nil {
			t.Fatalf("unsafe Git SSH definition %d accepted", index)
		}
	}
}

func cloneGitSSHSource(source *GitSSHSource) *GitSSHSource {
	copy := *source
	return &copy
}
