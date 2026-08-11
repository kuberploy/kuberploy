package argo_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/argo"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"go.yaml.in/yaml/v3"
)

const (
	projectID     = "22222222-2222-4222-8222-222222222222"
	environmentID = "33333333-3333-4333-8333-333333333333"
	applicationID = "44444444-4444-4444-8444-444444444444"
	deploymentID  = "99999999-9999-4999-8999-999999999999"
	bindingID     = "11111111-1111-4111-8111-111111111111"
	operationID   = "55555555-5555-4555-8555-555555555555"
)

func targetFixture(t *testing.T) (argo.EnvironmentTarget, domain.Application) {
	t.Helper()
	now := time.Now().UTC()
	project := domain.Project{ID: projectID, Name: "Demo", Slug: "demo", CreatedAt: now}
	namespace, argoProject := domain.DeriveEnvironmentDestination(project, "production")
	environment := domain.Environment{ID: environmentID, ProjectID: projectID, Name: "Production", Slug: "production", Namespace: namespace, ArgoProject: argoProject, CreatedAt: now}
	binding, err := gitprojection.NewEnvironmentBinding(bindingID, projectID, environmentID, gitprojection.RepositoryIdentity{Provider: "github", InstallationID: 7, RepositoryID: 8, Owner: "kuberploy", Name: "environments"}, "refs/heads/main", "kuberploy-git-writer", now)
	if err != nil {
		t.Fatal(err)
	}
	binding.TargetHeadRevision = strings.Repeat("a", 40)
	binding.TargetHeadObservedAt = now
	binding.IndexedRevision = binding.TargetHeadRevision
	binding.IndexedAt = now
	binding.ProjectionGeneration = 1
	binding.State = gitprojection.BindingReady
	binding.UpdatedAt = now
	target := argo.EnvironmentTarget{Project: project, Environment: environment, Binding: binding, ArgoNamespace: "argocd", Runtime: argo.RuntimeLock{ChartRepository: "oci://ghcr.io/kuberploy/charts", ChartName: "kuberploy-runtime", ChartVersion: "1.2.3", ChartDigest: "sha256:" + strings.Repeat("b", 64), RendererImage: "ghcr.io/kuberploy/runtime-renderer@sha256:" + strings.Repeat("c", 64)}}
	application := domain.Application{ID: applicationID, ProjectID: projectID, Name: "API", Slug: "api", CreatedAt: now}
	return target, application
}

func decodeYAML(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := yaml.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func deploymentFixture() domain.Deployment {
	return domain.Deployment{ID: deploymentID, EnvironmentID: environmentID, ApplicationID: applicationID}
}

func TestArgoManifestsAreDeterministicAndDestinationsAreServerOwned(t *testing.T) {
	target, application := targetFixture(t)
	deployment := deploymentFixture()
	first, err := argo.RenderApplication(target, application, deployment)
	if err != nil {
		t.Fatal(err)
	}
	second, err := argo.RenderApplication(target, application, deployment)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("Application rendering is nondeterministic")
	}
	text := string(first)
	for _, required := range []string{`project: ` + target.Environment.ArgoProject, `server: https://kubernetes.default.svc`, `namespace: ` + target.Environment.Namespace, `repoURL: oci://ghcr.io/kuberploy/charts/kuberploy-runtime`, `targetRevision: ` + target.Runtime.ChartDigest, `path: .`, `targetRevision: ` + target.Binding.IndexedRevision, `kuberploy.io/runtime-chart-digest: sha256:`, `$values/tenants/` + projectID + `/environments/` + environmentID + `/apps/` + applicationID + `/app.yaml`} {
		if !strings.Contains(text, required) {
			t.Errorf("missing %q in:\n%s", required, text)
		}
	}
	if !strings.Contains(text, `kuberploy.io/service: `+applicationID) {
		t.Fatalf("Application is not labeled with stable application/service identity:\n%s", text)
	}
	if !strings.Contains(text, `kuberploy.io/deployment-id: `+deploymentID) || !strings.Contains(text, `name: `+argo.ApplicationName(deploymentID)) {
		t.Fatalf("Application is not uniquely deployment-scoped:\n%s", text)
	}
	for _, forbidden := range []string{"chart:", "valuesObject:", "passCredentials:", "skipCrds:", "CreateNamespace=true", "destination: '{{", "argocd.argoproj.io/manifest-generate-paths"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("unsafe Argo field %q in:\n%s", forbidden, text)
		}
	}
	decoded := decodeYAML(t, first)
	spec, ok := decoded["spec"].(map[string]any)
	if !ok {
		t.Fatalf("Application spec has unexpected shape: %#v", decoded["spec"])
	}
	sources, ok := spec["sources"].([]any)
	if !ok || len(sources) != 2 || spec["source"] != nil {
		t.Fatalf("Application must contain exactly two sources and no singular source: %#v", spec)
	}
	runtimeSource, ok := sources[0].(map[string]any)
	if !ok || len(runtimeSource) != 4 || runtimeSource["repoURL"] != "oci://ghcr.io/kuberploy/charts/kuberploy-runtime" || runtimeSource["targetRevision"] != target.Runtime.ChartDigest || runtimeSource["path"] != "." || runtimeSource["chart"] != nil {
		t.Fatalf("runtime OCI source is not closed and digest-pinned: %#v", sources[0])
	}
	helm, ok := runtimeSource["helm"].(map[string]any)
	if !ok || len(helm) != 3 || helm["ignoreMissingValueFiles"] != true {
		t.Fatalf("runtime Helm configuration is not closed: %#v", runtimeSource["helm"])
	}
	valueFiles, ok := helm["valueFiles"].([]any)
	if !ok || len(valueFiles) != 3 || valueFiles[0] != "$values/tenants/"+projectID+"/variables.yaml" ||
		valueFiles[1] != "$values/tenants/"+projectID+"/environments/"+environmentID+"/variables.yaml" ||
		valueFiles[2] != "$values/tenants/"+projectID+"/environments/"+environmentID+"/apps/"+applicationID+"/app.yaml" {
		t.Fatalf("runtime values source is not exact: %#v", helm)
	}
	parameters, ok := helm["parameters"].([]any)
	if !ok || len(parameters) != 3 {
		t.Fatalf("operator-owned expected identity is missing: %#v", helm)
	}
	wantedIdentity := map[string]string{"kuberployExpectedIdentity.projectId": projectID, "kuberployExpectedIdentity.environmentId": environmentID, "kuberployExpectedIdentity.applicationId": applicationID}
	for _, raw := range parameters {
		parameter, valid := raw.(map[string]any)
		name, _ := parameter["name"].(string)
		if !valid || len(parameter) != 3 || parameter["forceString"] != true || parameter["value"] != wantedIdentity[name] {
			t.Fatalf("unexpected expected-identity parameter: %#v", raw)
		}
		delete(wantedIdentity, name)
	}
	if len(wantedIdentity) != 0 {
		t.Fatalf("missing identity parameters: %#v", wantedIdentity)
	}
	valuesSource, ok := sources[1].(map[string]any)
	if !ok || len(valuesSource) != 3 || valuesSource["repoURL"] != "https://github.com/kuberploy/environments.git" || valuesSource["targetRevision"] != target.Binding.IndexedRevision || valuesSource["ref"] != "values" || valuesSource["path"] != nil {
		t.Fatalf("Git values source is not closed: %#v", sources[1])
	}
	project, err := argo.RenderAppProject(target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(project), "kind: Secret\n") {
		t.Fatal("AppProject permits tenant Secret materialization")
	}
	if !strings.Contains(string(project), "clusterResourceWhitelist: []") {
		t.Fatalf("cluster resources were not denied:\n%s", project)
	}
	if !strings.Contains(string(project), "argocd.argoproj.io/sync-wave: \"-10\"") {
		t.Fatal("AppProject does not follow foundation sync waves")
	}
	otherApplication := domain.Application{ID: "66666666-6666-4666-8666-666666666666", ProjectID: projectID}
	otherDeployment := domain.Deployment{ID: "77777777-7777-4777-8777-777777777777", EnvironmentID: environmentID, ApplicationID: otherApplication.ID}
	setA, err := argo.RenderApplicationSet(target, []domain.Application{otherApplication, application}, []domain.Deployment{deployment, otherDeployment})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(setA), "argocd.argoproj.io/sync-wave: \"0\"") {
		t.Fatal("ApplicationSet is not ordered after AppProject")
	}
	setB, err := argo.RenderApplicationSet(target, []domain.Application{application, otherApplication}, []domain.Deployment{otherDeployment, deployment})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(setA, setB) {
		t.Fatal("ApplicationSet changes with input order")
	}
	if strings.Index(string(setA), otherDeployment.ID) > strings.Index(string(setA), deployment.ID) {
		t.Fatal("trusted generator elements are not sorted")
	}
	_ = decodeYAML(t, setA)
}

func TestArgo35CRDSchemaSupportsClosedDigestPinnedOCIMultiSource(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate Argo package")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	chart := filepath.Join(repositoryRoot, "charts", "kuberploy-argocd")
	values := filepath.Join(chart, "testdata", "managed-values.yaml")
	command := exec.Command(helm, "template", "argo-schema-contract", chart, "--namespace", "kuberploy-system", "--include-crds", "-f", values)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("render Argo 3.5 chart: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("quay.io/argoproj/argocd:v3.5.0")) {
		t.Fatal("schema contract was not rendered from the pinned Argo CD 3.5.0 image identity")
	}

	decoder := yaml.NewDecoder(bytes.NewReader(output))
	var applicationCRD map[string]any
	for {
		var document map[string]any
		decodeErr := decoder.Decode(&document)
		if errors.Is(decodeErr, io.EOF) {
			break
		}
		if decodeErr != nil {
			t.Fatalf("decode rendered Argo chart: %v", decodeErr)
		}
		metadata, _ := document["metadata"].(map[string]any)
		if document["kind"] == "CustomResourceDefinition" && metadata["name"] == "applications.argoproj.io" {
			applicationCRD = document
			break
		}
	}
	if applicationCRD == nil {
		t.Fatal("Argo 3.5 Application CRD was not rendered")
	}
	object := func(parent map[string]any, key string) map[string]any {
		t.Helper()
		value, valid := parent[key].(map[string]any)
		if !valid {
			t.Fatalf("Argo CRD field %q has unexpected shape: %#v", key, parent[key])
		}
		return value
	}
	spec := object(applicationCRD, "spec")
	versions, valid := spec["versions"].([]any)
	if !valid {
		t.Fatalf("Argo CRD versions have unexpected shape: %#v", spec["versions"])
	}
	var schema map[string]any
	for _, versionValue := range versions {
		version, versionOK := versionValue.(map[string]any)
		if versionOK && version["name"] == "v1alpha1" {
			schema = object(version, "schema")
			break
		}
	}
	if schema == nil {
		t.Fatal("Argo 3.5 Application v1alpha1 schema is absent")
	}
	rootProperties := object(object(schema, "openAPIV3Schema"), "properties")
	applicationSpecProperties := object(object(object(rootProperties, "spec"), "properties"), "sources")
	if applicationSpecProperties["type"] != "array" {
		t.Fatalf("Application.spec.sources is not an array: %#v", applicationSpecProperties)
	}
	source := object(applicationSpecProperties, "items")
	sourceProperties := object(source, "properties")
	for _, field := range []string{"repoURL", "path", "targetRevision", "ref"} {
		if object(sourceProperties, field)["type"] != "string" {
			t.Fatalf("Argo 3.5 source field %s is not a string", field)
		}
	}
	helmSchema := object(sourceProperties, "helm")
	if helmSchema["type"] != "object" {
		t.Fatalf("Argo 3.5 source.helm is not an object: %#v", helmSchema)
	}
	valueFiles := object(object(helmSchema, "properties"), "valueFiles")
	if valueFiles["type"] != "array" || object(valueFiles, "items")["type"] != "string" {
		t.Fatalf("Argo 3.5 source.helm.valueFiles is incompatible: %#v", valueFiles)
	}
}

func TestArgoRenderingRejectsUnpinnedOrTenantSelectedAuthority(t *testing.T) {
	target, application := targetFixture(t)
	deployment := deploymentFixture()
	target.Runtime.RendererImage = "ghcr.io/kuberploy/runtime-renderer:latest"
	if _, err := argo.RenderApplication(target, application, deployment); !errors.Is(err, argo.ErrInvalid) {
		t.Fatalf("mutable renderer accepted: %v", err)
	}
	target, _ = targetFixture(t)
	target.Environment.Namespace = "tenant-selected"
	if _, err := argo.RenderApplication(target, application, deployment); !errors.Is(err, argo.ErrInvalid) {
		t.Fatalf("tenant namespace accepted: %v", err)
	}
	target, _ = targetFixture(t)
	target.Binding.Prefix = "../../escape"
	if _, err := argo.RenderApplication(target, application, deployment); !errors.Is(err, argo.ErrInvalid) {
		t.Fatalf("tenant Git path accepted: %v", err)
	}
}

func observationFixture(t *testing.T) argo.Observation {
	target, _ := targetFixture(t)
	now := time.Now().UTC()
	revision := strings.Repeat("a", 40)
	return argo.Observation{DeploymentID: deploymentID, ApplicationID: applicationID, ProjectID: projectID, EnvironmentID: environmentID, ArgoUID: "77777777-7777-4777-8777-777777777777", ArgoNamespace: "argocd", ArgoName: argo.ApplicationName(deploymentID), DestinationNamespace: target.Environment.Namespace, DesiredRevision: revision, ObservedRevision: revision, Sync: argo.SyncSynced, Health: argo.HealthHealthy, OperationPhase: "succeeded", Resources: []argo.ResourceIdentity{{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: target.Environment.Namespace, Name: "kp-a-runtime", UID: "deployment-uid", Health: argo.HealthHealthy}}, ObservedAt: now, UpdatedAt: now}
}

func TestObservedStateRejectsCrossNamespaceAndOutOfOrderEvents(t *testing.T) {
	store := argo.NewMemoryObservationStore()
	value := observationFixture(t)
	if err := store.PutObservation(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObservation(t.Context(), value); err != nil {
		t.Fatalf("exact watch replay was not idempotent: %v", err)
	}
	tied := value
	tied.Health = argo.HealthDegraded
	if err := store.PutObservation(t.Context(), tied); !errors.Is(err, argo.ErrConflict) {
		t.Fatalf("different observation at the same cursor replaced state: %v", err)
	}
	invalidTime := value
	invalidTime.UpdatedAt = invalidTime.ObservedAt.Add(-time.Second)
	if err := store.PutObservation(t.Context(), invalidTime); !errors.Is(err, argo.ErrInvalid) {
		t.Fatalf("observation with regressed update time accepted: %v", err)
	}
	got, err := store.Observation(t.Context(), deploymentID)
	if err != nil || !got.Reconciled() {
		t.Fatalf("observation not reconciled: %#v %v", got, err)
	}
	old := value
	old.ObservedAt = old.ObservedAt.Add(-time.Second)
	old.UpdatedAt = old.UpdatedAt.Add(time.Second)
	if err = store.PutObservation(t.Context(), old); !errors.Is(err, argo.ErrConflict) {
		t.Fatalf("out-of-order watch event accepted: %v", err)
	}
	cross := value
	cross.Resources = append([]argo.ResourceIdentity(nil), value.Resources...)
	cross.ObservedAt = cross.ObservedAt.Add(time.Second)
	cross.UpdatedAt = cross.UpdatedAt.Add(time.Second)
	cross.Resources[0].Namespace = "other"
	if err = store.PutObservation(t.Context(), cross); !errors.Is(err, argo.ErrInvalid) {
		t.Fatalf("cross-namespace resource accepted: %v", err)
	}

	var wait sync.WaitGroup
	var conflicts atomic.Int64
	for index := range 16 {
		wait.Add(1)
		go func(offset int) {
			defer wait.Done()
			candidate := value
			candidate.ObservedAt = candidate.ObservedAt.Add(time.Duration(offset+1) * time.Second)
			candidate.UpdatedAt = candidate.ObservedAt
			if putErr := store.PutObservation(context.Background(), candidate); errors.Is(putErr, argo.ErrConflict) {
				conflicts.Add(1)
			}
		}(index)
	}
	wait.Wait()
	if conflicts.Load() == 0 {
		t.Fatal("concurrent out-of-order observations never exercised CAS")
	}
}

type artifactVerifier struct {
	calls atomic.Int64
	err   error
}

func (v *artifactVerifier) VerifyRetainedRelease(_ context.Context, application, repository, digest string) error {
	v.calls.Add(1)
	if application != applicationID || repository != "registry.example/api" || digest != "sha256:"+strings.Repeat("d", 64) {
		return errors.New("wrong artifact identity")
	}
	return v.err
}

type gitCommitter struct {
	calls    atomic.Int64
	revision string
	mutation gitprojection.Mutation
}

func (g *gitCommitter) CommitMutation(_ context.Context, mutation gitprojection.Mutation) (string, error) {
	g.calls.Add(1)
	g.mutation = mutation
	return g.revision, nil
}

func TestRollbackCreatesNewGitDesiredStateAndNeverCallsArgo(t *testing.T) {
	target, application := targetFixture(t)
	project := target.Project
	environment := target.Environment
	current, err := gitops.RenderAppConfig(project, environment, application, domain.Deployment{Image: "registry.example/api@sha256:" + strings.Repeat("a", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)})
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &artifactVerifier{}
	git := &gitCommitter{revision: strings.Repeat("e", 40)}
	store := argo.NewMemoryRollbackStore()
	service := argo.RollbackService{Store: store, Artifacts: artifacts, Git: git}
	now := time.Now().UTC()
	command, mutation, err := service.Plan(t.Context(), argo.RollbackRequest{ID: "88888888-8888-4888-8888-888888888888", OperationID: operationID, ApplicationID: applicationID, EnvironmentID: environmentID, Binding: target.Binding, BaseRevision: target.Binding.TargetHeadRevision, ExpectedETag: `"sha256:` + strings.Repeat("f", 64) + `"`, Current: current, ReleaseRepository: "registry.example/api", ReleaseDigest: "sha256:" + strings.Repeat("d", 64), SourceRevision: strings.Repeat("9", 40), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if command.State != argo.RollbackPendingGit || !strings.Contains(string(mutation.Content), strings.Repeat("d", 64)) || strings.Contains(string(mutation.Content), strings.Repeat("a", 64)) {
		t.Fatalf("rollback candidate wrong: %#v\n%s", command, mutation.Content)
	}
	if created, replayErr := store.CreateRollback(t.Context(), command, mutation); replayErr != nil || created {
		t.Fatalf("exact rollback replay created=%v err=%v", created, replayErr)
	}
	tampered := mutation
	tampered.Message = "different rollback"
	if _, replayErr := store.CreateRollback(t.Context(), command, tampered); !errors.Is(replayErr, argo.ErrConflict) {
		t.Fatalf("operation replay with changed mutation accepted: %v", replayErr)
	}
	tampered = mutation
	tampered.Path = "tenants/" + projectID + "/environments/" + environmentID + "/apps/other/app.yaml"
	if _, replayErr := store.CreateRollback(t.Context(), command, tampered); !errors.Is(replayErr, argo.ErrInvalid) {
		t.Fatalf("rollback accepted an arbitrary Git path: %v", replayErr)
	}
	revision, err := service.Execute(t.Context(), command.ID, now.Add(time.Second))
	if err != nil || revision != git.revision {
		t.Fatalf("execute: %s %v", revision, err)
	}
	if _, err = service.Execute(t.Context(), command.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if git.calls.Load() != 1 || artifacts.calls.Load() != 1 {
		t.Fatalf("non-idempotent rollback: git=%d artifact=%d", git.calls.Load(), artifacts.calls.Load())
	}
	stored, _, err := store.Rollback(t.Context(), command.ID)
	if err != nil || stored.State != argo.RollbackGitCommitted || stored.GitRevision != git.revision {
		t.Fatalf("durable rollback result: %#v %v", stored, err)
	}
}
