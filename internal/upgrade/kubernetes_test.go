package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

type fakeJobAPI struct {
	job           *KubernetesJob
	gets          int
	creates       int
	completeAtGet int
	getErrAt      int
}

func executableComponentCharts(version, digest string) []domain.ManifestChart {
	names := []string{"kuberploy-argocd", "kuberploy-installer", "kuberploy-builder", "kuberploy-cert-manager", "kuberploy-edge", "kuberploy-external-dns", "kuberploy-external-secrets", "kuberploy-monitoring", "kuberploy-postgresql", "kuberploy-registry", "kuberploy-runtime", "kuberploy-sealed-secrets", "kuberploy-valkey"}
	charts := make([]domain.ManifestChart, 0, len(names))
	for _, name := range names {
		charts = append(charts, domain.ManifestChart{Name: name, Version: version, OCIReference: "ghcr.io/kuberploy/charts/" + name + ":" + version, OCIDigest: digest, Package: name + "-" + version + ".tgz", PackageSHA256: digest})
	}
	return charts
}

func (f *fakeJobAPI) ServerVersion(context.Context) (string, error) { return "v1.35.3", nil }

func (f *fakeJobAPI) GetJob(_ context.Context, _, _ string) (KubernetesJob, error) {
	f.gets++
	if f.getErrAt > 0 && f.gets == f.getErrAt {
		return KubernetesJob{}, errors.New("temporary Kubernetes API outage")
	}
	if f.job == nil {
		return KubernetesJob{}, errJobNotFound
	}
	if f.completeAtGet > 0 && f.gets >= f.completeAtGet {
		f.job.Status = JobStatus{Succeeded: 1, CompletionTime: "2026-08-06T00:01:00Z", Conditions: []JobCondition{{Type: "Complete", Status: "True"}}}
	}
	return *f.job, nil
}
func (f *fakeJobAPI) CreateJob(_ context.Context, _ string, job KubernetesJob) (KubernetesJob, error) {
	f.creates++
	if f.job != nil {
		return KubernetesJob{}, errJobConflict
	}
	f.job = &job
	return job, nil
}

func validExecutableRequest() ExecutableRequest {
	version := "1.1.0"
	artifactDigest := "sha256:" + strings.Repeat("a", 64)
	commit := strings.Repeat("b", 40)
	manifest := domain.ReleaseManifest{
		Schema:        "https://raw.githubusercontent.com/kuberploy/kuberploy/" + commit + "/release/release-manifest.schema.json",
		SchemaVersion: "1.0.0",
		Release: domain.ManifestRelease{
			Tag:       "v" + version,
			Version:   version,
			CreatedAt: time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
			NotesURL:  "https://github.com/kuberploy/kuberploy/releases/tag/v" + version,
			Summary:   "Safe namespaced control-plane update",
		},
		Source:   domain.ManifestSource{Repository: "kuberploy/kuberploy", Commit: commit},
		Versions: domain.ManifestVersions{Kuberploy: version, API: version, Worker: version, Web: version, Migration: version, Upgrader: version, BuilderAgent: version, Chart: version},
		Compatibility: domain.ReleaseCompatibility{
			SupportedUpgradeFrom: ">=1.0.0 <1.1.0",
			Kubernetes:           domain.KubernetesCompatibility{Constraint: ">=1.34.0-0 <1.37.0-0", TestedMinors: []string{"1.34", "1.35", "1.36"}},
			Database: domain.DatabaseCompatibility{
				Engine: "postgresql", CurrentSchema: "002_platform_upgrades", MinimumUpgradeableSchema: "001_initial",
				MigrationSetSHA256: artifactDigest, Strategy: "prisma-migrate-deploy-with-advisory-lock",
				RollbackPolicy: "Only roll back to a schema-compatible control-plane release.",
			},
		},
		Artifacts: domain.ManifestArtifacts{
			Images: []domain.ManifestImage{
				{Component: "api", Reference: "ghcr.io/kuberploy/kuberploy-api", Digest: artifactDigest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "worker", Reference: "ghcr.io/kuberploy/kuberploy-worker", Digest: artifactDigest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "web", Reference: "ghcr.io/kuberploy/kuberploy-web", Digest: artifactDigest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "migration", Reference: "ghcr.io/kuberploy/kuberploy-migration", Digest: artifactDigest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "upgrader", Reference: "ghcr.io/kuberploy/kuberploy-upgrader", Digest: artifactDigest, Platforms: []string{"linux/amd64", "linux/arm64"}},
				{Component: "builder-agent", Reference: "ghcr.io/kuberploy/kuberploy-builder-agent", Digest: artifactDigest, Platforms: []string{"linux/amd64", "linux/arm64"}},
			},
			Chart:           domain.ManifestChart{Name: "kuberploy", Version: version, OCIReference: "ghcr.io/kuberploy/charts/kuberploy:" + version, OCIDigest: artifactDigest, Package: "kuberploy-" + version + ".tgz", PackageSHA256: artifactDigest},
			ComponentCharts: executableComponentCharts(version, artifactDigest),
		},
		DependencyLock: domain.ManifestDependencyLock{File: "DEPENDENCIES.md", SHA256: artifactDigest},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(manifestBytes)
	manifestDigest := "sha256:" + hex.EncodeToString(sum[:])
	operationID := "11111111-2222-4333-8444-555555555555"
	return ExecutableRequest{OperationID: operationID, Generation: 2, JobName: JobName(operationID, 2), Namespace: "kuberploy-system", ReleaseName: "kuberploy", TargetVersion: version, ManifestDigest: manifestDigest, ManifestBytes: manifestBytes}
}

func TestKubernetesRunnerCreatesSecureDigestPinnedJobAndReconciles(t *testing.T) {
	request := validExecutableRequest()
	jobs := &fakeJobAPI{completeAtGet: 2}
	runner := KubernetesRunner{Jobs: jobs, Namespace: request.Namespace, ServiceAccount: "kuberploy-upgrade", ReleaseName: request.ReleaseName, ActiveDeadlineSeconds: 900, PollInterval: time.Millisecond}
	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RunnerRef != request.JobName || jobs.creates != 1 || jobs.job == nil {
		t.Fatalf("result=%#v creates=%d", result, jobs.creates)
	}
	job := jobs.job
	container := job.Spec.Template.Spec.Containers[0]
	artifactDigest := "sha256:" + strings.Repeat("a", 64)
	if container.Image != "ghcr.io/kuberploy/kuberploy-upgrader@"+artifactDigest {
		t.Fatalf("untrusted upgrader image %q", container.Image)
	}
	args := strings.Join(container.Args, " ")
	for _, want := range []string{"oci://ghcr.io/kuberploy/charts/kuberploy@" + artifactDigest, "--reuse-values", "--rollback-on-failure", "components.api.image.reference=ghcr.io/kuberploy/kuberploy-api@" + artifactDigest, "components.migration.image.reference=ghcr.io/kuberploy/kuberploy-migration@" + artifactDigest, "builder.builderAgentImage=ghcr.io/kuberploy/kuberploy-builder-agent@" + artifactDigest} {
		if !strings.Contains(args, want) {
			t.Fatalf("Helm args missing %q: %s", want, args)
		}
	}
	if *job.Spec.Template.Spec.AutomountServiceAccountToken || container.SecurityContext.Privileged == nil || *container.SecurityContext.Privileged {
		t.Fatal("upgrade Job is not unprivileged and token-explicit")
	}
	projections := job.Spec.Template.Spec.Volumes[1].Projected.Sources
	if token := projections[0].ServiceAccountToken; token == nil || token.Audience != "https://kubernetes.default.svc.cluster.local" || token.ExpirationSeconds == nil || *token.ExpirationSeconds > 900 {
		t.Fatalf("unsafe projected token %#v", token)
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"Name"`) || !strings.Contains(string(encoded), `"name":"HOME"`) {
		t.Fatalf("Job does not use Kubernetes JSON field names: %s", encoded)
	}
	if strings.Contains(string(encoded), "ttlSecondsAfterFinished") {
		t.Fatalf("upgrade Job must remain available for restart reconciliation: %s", encoded)
	}
	// A replacement worker observes the terminal deterministic Job and must not
	// create or execute another Helm upgrade.
	result, err = runner.Run(context.Background(), request)
	if err != nil || result.RunnerRef != request.JobName || jobs.creates != 1 {
		t.Fatalf("idempotent reconcile result=%#v creates=%d err=%v", result, jobs.creates, err)
	}
}

func TestKubernetesRunnerRequeuesObservationOutageAndRecoversSameJob(t *testing.T) {
	request := validExecutableRequest()
	jobs := &fakeJobAPI{getErrAt: 2}
	runner := KubernetesRunner{Jobs: jobs, Namespace: request.Namespace, ServiceAccount: "kuberploy-upgrade", ReleaseName: request.ReleaseName, PollInterval: time.Millisecond}
	result, err := runner.Run(context.Background(), request)
	if err != nil || !result.Pending || result.RunnerRef != request.JobName || jobs.creates != 1 {
		t.Fatalf("pending result=%#v creates=%d err=%v", result, jobs.creates, err)
	}
	jobs.getErrAt = 0
	jobs.completeAtGet = jobs.gets + 1
	result, err = runner.Run(context.Background(), request)
	if err != nil || result.Pending || result.RunnerRef != request.JobName || jobs.creates != 1 {
		t.Fatalf("recovered result=%#v creates=%d err=%v", result, jobs.creates, err)
	}
}

func TestKubernetesRunnerRejectsTamperedExactManifestBytes(t *testing.T) {
	request := validExecutableRequest()
	request.ManifestBytes = append([]byte(nil), request.ManifestBytes...)
	request.ManifestBytes[len(request.ManifestBytes)-1] ^= 1
	jobs := &fakeJobAPI{}
	runner := KubernetesRunner{Jobs: jobs, Namespace: request.Namespace, ServiceAccount: "kuberploy-upgrade", ReleaseName: request.ReleaseName}
	if _, err := runner.Run(context.Background(), request); err == nil || !strings.Contains(err.Error(), "exact release manifest bytes") {
		t.Fatalf("expected exact-byte rejection, got %v", err)
	}
	if jobs.creates != 0 {
		t.Fatalf("tampered manifest created %d Jobs", jobs.creates)
	}
}

func TestKubernetesRunnerRejectsConflictingExistingJob(t *testing.T) {
	request := validExecutableRequest()
	jobs := &fakeJobAPI{}
	runner := KubernetesRunner{Jobs: jobs, Namespace: request.Namespace, ServiceAccount: "kuberploy-upgrade", ReleaseName: request.ReleaseName, PollInterval: time.Millisecond}
	desired, err := runner.desiredJob(request)
	if err != nil {
		t.Fatal(err)
	}
	desired.Spec.Template.Spec.Containers[0].Image = "example.invalid/attacker@" + request.ManifestDigest
	desired.Status = JobStatus{Succeeded: 1}
	jobs.job = &desired
	if _, err = runner.Run(context.Background(), request); err == nil || !strings.Contains(err.Error(), "spec differs") {
		t.Fatalf("expected conflicting Job rejection, got %v", err)
	}
}

func TestKubernetesRunnerRejectsMutatedNetworkPolicyLabel(t *testing.T) {
	request := validExecutableRequest()
	jobs := &fakeJobAPI{}
	runner := KubernetesRunner{Jobs: jobs, Namespace: request.Namespace, ServiceAccount: "kuberploy-upgrade", ReleaseName: request.ReleaseName}
	job, err := runner.desiredJob(request)
	if err != nil {
		t.Fatal(err)
	}
	job.Spec.Template.Metadata.Labels["app.kubernetes.io/component"] = "worker"
	job.Status.Succeeded = 1
	jobs.job = &job
	if _, err = runner.Run(context.Background(), request); err == nil || !strings.Contains(err.Error(), "template label") {
		t.Fatalf("expected mutated template label rejection, got %v", err)
	}
}

func TestKubernetesRunnerPersistsFailedTerminalState(t *testing.T) {
	request := validExecutableRequest()
	jobs := &fakeJobAPI{}
	runner := KubernetesRunner{Jobs: jobs, Namespace: request.Namespace, ServiceAccount: "kuberploy-upgrade", ReleaseName: request.ReleaseName}
	job, err := runner.desiredJob(request)
	if err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []JobCondition{{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded", Message: "Helm exited 1"}}
	jobs.job = &job
	_, err = runner.Run(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "Helm exited 1") || errors.Is(err, context.Canceled) {
		t.Fatalf("expected terminal Helm failure, got %v", err)
	}
}

func TestSupportedKubernetesVersionIsBoundedByManifestContract(t *testing.T) {
	for _, version := range []string{"v1.34.0", "v1.35.7+k3s1", "1.36.1"} {
		if !supportedKubernetesVersion(version) {
			t.Errorf("expected %s to be supported", version)
		}
	}
	for _, version := range []string{"v1.33.9", "v1.37.0", "v2.34.0", "garbage"} {
		if supportedKubernetesVersion(version) {
			t.Errorf("expected %s to be rejected", version)
		}
	}
}
