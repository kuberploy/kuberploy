package argo

import (
	"errors"
	"slices"
	"strings"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"go.yaml.in/yaml/v3"
)

type typeMeta struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

type objectMeta struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace,omitempty"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

type appProjectManifest struct {
	typeMeta `yaml:",inline"`
	Metadata objectMeta `yaml:"metadata"`
	Spec     struct {
		SourceRepos                []string            `yaml:"sourceRepos"`
		Destinations               []map[string]string `yaml:"destinations"`
		ClusterResourceWhitelist   []map[string]string `yaml:"clusterResourceWhitelist"`
		NamespaceResourceWhitelist []map[string]string `yaml:"namespaceResourceWhitelist"`
		OrphanedResources          map[string]bool     `yaml:"orphanedResources"`
	} `yaml:"spec"`
}

type argoSource struct {
	RepoURL        string      `yaml:"repoURL"`
	Path           string      `yaml:"path,omitempty"`
	TargetRevision string      `yaml:"targetRevision"`
	Ref            string      `yaml:"ref,omitempty"`
	Helm           *helmSource `yaml:"helm,omitempty"`
}

type helmSource struct {
	ValueFiles              []string        `yaml:"valueFiles"`
	IgnoreMissingValueFiles bool            `yaml:"ignoreMissingValueFiles"`
	Parameters              []helmParameter `yaml:"parameters"`
}

type helmParameter struct {
	Name        string `yaml:"name"`
	Value       string `yaml:"value"`
	ForceString bool   `yaml:"forceString"`
}

type applicationManifest struct {
	typeMeta `yaml:",inline"`
	Metadata objectMeta      `yaml:"metadata"`
	Spec     applicationSpec `yaml:"spec"`
}

type applicationSpec struct {
	Project     string            `yaml:"project"`
	Sources     []argoSource      `yaml:"sources"`
	Destination map[string]string `yaml:"destination"`
	SyncPolicy  struct {
		Automated   map[string]bool `yaml:"automated"`
		SyncOptions []string        `yaml:"syncOptions"`
	} `yaml:"syncPolicy"`
}

type applicationSetTemplate struct {
	Metadata objectMeta      `yaml:"metadata"`
	Spec     applicationSpec `yaml:"spec"`
}

type applicationElement struct {
	DeploymentID    string `yaml:"deploymentId"`
	ApplicationID   string `yaml:"applicationId"`
	ApplicationName string `yaml:"applicationName"`
	ApplicationPath string `yaml:"applicationPath"`
	ValuesRevision  string `yaml:"valuesRevision"`
}

type applicationSetManifest struct {
	typeMeta `yaml:",inline"`
	Metadata objectMeta `yaml:"metadata"`
	Spec     struct {
		GoTemplate        bool     `yaml:"goTemplate"`
		GoTemplateOptions []string `yaml:"goTemplateOptions"`
		Generators        []struct {
			List struct {
				Elements []applicationElement `yaml:"elements"`
			} `yaml:"list"`
		} `yaml:"generators"`
		Template applicationSetTemplate `yaml:"template"`
	} `yaml:"spec"`
}

func baseLabels(target EnvironmentTarget) map[string]string {
	return map[string]string{"app.kubernetes.io/managed-by": "kuberploy", "kuberploy.io/project-id": target.Project.ID, "kuberploy.io/environment-id": target.Environment.ID}
}

func chartRepoForArgo(runtime RuntimeLock) string {
	return strings.TrimSuffix(runtime.ChartRepository, "/") + "/" + runtime.ChartName
}

func RenderAppProject(target EnvironmentTarget) ([]byte, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	gitRemote, err := target.Binding.Repository.CanonicalRemote()
	if err != nil {
		return nil, err
	}
	manifest := appProjectManifest{typeMeta: typeMeta{"argoproj.io/v1alpha1", "AppProject"}, Metadata: objectMeta{Name: target.Environment.ArgoProject, Namespace: target.ArgoNamespace, Labels: baseLabels(target),
		Annotations: map[string]string{"argocd.argoproj.io/sync-options": "PruneLast=true", "argocd.argoproj.io/sync-wave": "-10", "kuberploy.io/git-binding-id": target.Binding.ID, "kuberploy.io/runtime-chart-digest": target.Runtime.ChartDigest, "kuberploy.io/renderer-image": target.Runtime.RendererImage}}}
	manifest.Spec.SourceRepos = []string{gitRemote, chartRepoForArgo(target.Runtime)}
	manifest.Spec.Destinations = []map[string]string{{"server": InClusterServer, "namespace": target.Environment.Namespace}}
	manifest.Spec.ClusterResourceWhitelist = []map[string]string{}
	manifest.Spec.NamespaceResourceWhitelist = []map[string]string{
		{"group": "", "kind": "ConfigMap"}, {"group": "", "kind": "Service"}, {"group": "", "kind": "ServiceAccount"},
		{"group": "apps", "kind": "Deployment"}, {"group": "autoscaling", "kind": "HorizontalPodAutoscaler"},
		{"group": "networking.k8s.io", "kind": "Ingress"}, {"group": "networking.k8s.io", "kind": "NetworkPolicy"},
		{"group": "policy", "kind": "PodDisruptionBudget"}, {"group": "traefik.io", "kind": "Middleware"}, {"group": "cert-manager.io", "kind": "Certificate"},
	}
	manifest.Spec.OrphanedResources = map[string]bool{"warn": true}
	return yaml.Marshal(manifest)
}

func RenderApplication(target EnvironmentTarget, application domain.Application, deployment domain.Deployment) ([]byte, error) {
	if err := target.Validate(); err != nil || application.ProjectID != target.Project.ID || !uuidRE.MatchString(application.ID) ||
		!uuidRE.MatchString(deployment.ID) || deployment.ApplicationID != application.ID || deployment.EnvironmentID != target.Environment.ID {
		return nil, ErrInvalid
	}
	applicationPath, err := gitprojection.ApplicationPath(target.Binding, application.ID)
	if err != nil {
		return nil, err
	}
	valuesRevision := target.Binding.IndexedRevision
	if deployment.DesiredRevision != "" {
		if !commitRE.MatchString(deployment.DesiredRevision) {
			return nil, ErrInvalid
		}
		valuesRevision = deployment.DesiredRevision
	}
	manifest, err := applicationTemplate(target, ApplicationName(deployment.ID), applicationPath, application.ID, deployment.ID, valuesRevision)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(manifest)
}

func applicationTemplate(target EnvironmentTarget, name, applicationPath, applicationID, deploymentID, valuesRevision string) (applicationManifest, error) {
	gitRemote, err := target.Binding.Repository.CanonicalRemote()
	if err != nil {
		return applicationManifest{}, err
	}
	if !commitRE.MatchString(valuesRevision) && valuesRevision != "{{.valuesRevision}}" {
		return applicationManifest{}, ErrInvalid
	}
	labels := baseLabels(target)
	labels["kuberploy.io/application-id"] = applicationID
	labels["kuberploy.io/deployment-id"] = deploymentID
	labels["kuberploy.io/service"] = applicationID
	dependencyPaths, err := gitprojection.DependencyPaths(target.Binding)
	if err != nil {
		return applicationManifest{}, err
	}
	manifest := applicationManifest{typeMeta: typeMeta{"argoproj.io/v1alpha1", "Application"}, Metadata: objectMeta{Name: name, Namespace: target.ArgoNamespace, Labels: labels,
		Annotations: map[string]string{"kuberploy.io/git-binding-id": target.Binding.ID, "kuberploy.io/runtime-chart-digest": target.Runtime.ChartDigest,
			"kuberploy.io/runtime-chart-version": target.Runtime.ChartVersion, "kuberploy.io/renderer-image": target.Runtime.RendererImage}}}
	manifest.Spec.Project = target.Environment.ArgoProject
	manifest.Spec.Sources = []argoSource{
		{RepoURL: chartRepoForArgo(target.Runtime), Path: ".", TargetRevision: target.Runtime.ChartDigest, Helm: &helmSource{ValueFiles: []string{"$values/" + dependencyPaths[0], "$values/" + dependencyPaths[1], "$values/" + applicationPath}, IgnoreMissingValueFiles: true, Parameters: []helmParameter{
			{Name: "kuberployExpectedIdentity.projectId", Value: target.Project.ID, ForceString: true},
			{Name: "kuberployExpectedIdentity.environmentId", Value: target.Environment.ID, ForceString: true},
			{Name: "kuberployExpectedIdentity.applicationId", Value: applicationID, ForceString: true},
		}}},
		{RepoURL: gitRemote, TargetRevision: valuesRevision, Ref: "values"},
	}
	manifest.Spec.Destination = map[string]string{"server": InClusterServer, "namespace": target.Environment.Namespace}
	manifest.Spec.SyncPolicy.Automated = map[string]bool{"allowEmpty": false, "prune": true, "selfHeal": true}
	manifest.Spec.SyncPolicy.SyncOptions = []string{"CreateNamespace=false", "ServerSideApply=true"}
	return manifest, nil
}

func RenderApplicationSet(target EnvironmentTarget, applications []domain.Application, deployments []domain.Deployment) ([]byte, error) {
	if err := target.Validate(); err != nil || len(applications) > 10_000 || len(deployments) > 10_000 {
		return nil, ErrInvalid
	}
	apps := slices.Clone(applications)
	slices.SortFunc(apps, func(a, b domain.Application) int { return strings.Compare(a.ID, b.ID) })
	applicationByID := make(map[string]domain.Application, len(apps))
	for _, application := range apps {
		if application.ProjectID != target.Project.ID || !uuidRE.MatchString(application.ID) {
			return nil, ErrInvalid
		}
		if _, duplicate := applicationByID[application.ID]; duplicate {
			return nil, ErrInvalid
		}
		applicationByID[application.ID] = application
	}
	deploymentValues := slices.Clone(deployments)
	slices.SortFunc(deploymentValues, func(a, b domain.Deployment) int { return strings.Compare(a.ID, b.ID) })
	elements := make([]applicationElement, 0, len(deploymentValues))
	seenDeployments := map[string]struct{}{}
	for _, deployment := range deploymentValues {
		application, exists := applicationByID[deployment.ApplicationID]
		if !exists || !uuidRE.MatchString(deployment.ID) || deployment.EnvironmentID != target.Environment.ID {
			return nil, ErrInvalid
		}
		if _, duplicate := seenDeployments[deployment.ID]; duplicate {
			return nil, ErrInvalid
		}
		seenDeployments[deployment.ID] = struct{}{}
		applicationPath, err := gitprojection.ApplicationPath(target.Binding, application.ID)
		if err != nil {
			return nil, err
		}
		valuesRevision := target.Binding.IndexedRevision
		if deployment.DesiredRevision != "" {
			if !commitRE.MatchString(deployment.DesiredRevision) {
				return nil, ErrInvalid
			}
			valuesRevision = deployment.DesiredRevision
		}
		elements = append(elements, applicationElement{DeploymentID: deployment.ID, ApplicationID: application.ID, ApplicationName: ApplicationName(deployment.ID), ApplicationPath: applicationPath, ValuesRevision: valuesRevision})
	}
	template, err := applicationTemplate(target, "{{.applicationName}}", "{{.applicationPath}}", "{{.applicationId}}", "{{.deploymentId}}", "{{.valuesRevision}}")
	if err != nil {
		return nil, err
	}
	manifest := applicationSetManifest{typeMeta: typeMeta{"argoproj.io/v1alpha1", "ApplicationSet"}, Metadata: objectMeta{Name: "kp-e-" + strings.ReplaceAll(target.Environment.ID, "-", ""), Namespace: target.ArgoNamespace, Labels: baseLabels(target),
		Annotations: map[string]string{"argocd.argoproj.io/sync-wave": "0", "kuberploy.io/git-binding-id": target.Binding.ID, "kuberploy.io/runtime-chart-digest": target.Runtime.ChartDigest}}}
	manifest.Spec.GoTemplate = true
	manifest.Spec.GoTemplateOptions = []string{"missingkey=error"}
	manifest.Spec.Generators = make([]struct {
		List struct {
			Elements []applicationElement `yaml:"elements"`
		} `yaml:"list"`
	}, 1)
	manifest.Spec.Generators[0].List.Elements = elements
	manifest.Spec.Template = applicationSetTemplate{Metadata: template.Metadata, Spec: template.Spec}
	return yaml.Marshal(manifest)
}

func RenderEnvironment(target EnvironmentTarget, applications []domain.Application, deployments []domain.Deployment) ([]byte, error) {
	project, err := RenderAppProject(target)
	if err != nil {
		return nil, err
	}
	set, err := RenderApplicationSet(target, applications, deployments)
	if err != nil {
		return nil, err
	}
	if len(project)+len(set) > 1<<20 {
		return nil, errors.New("Argo environment manifest exceeds its limit")
	}
	return append(append(project, []byte("---\n")...), set...), nil
}
