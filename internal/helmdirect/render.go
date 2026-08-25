package helmdirect

import (
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	ArgoNamespace = "argocd"
	InClusterURL  = "https://kubernetes.default.svc"
)

type applicationManifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Finalizers  []string          `yaml:"finalizers"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		Project string `yaml:"project"`
		Source  struct {
			RepoURL        string `yaml:"repoURL"`
			Chart          string `yaml:"chart,omitempty"`
			Path           string `yaml:"path,omitempty"`
			TargetRevision string `yaml:"targetRevision"`
			Helm           struct {
				ReleaseName string `yaml:"releaseName"`
				Values      string `yaml:"values"`
			} `yaml:"helm"`
		} `yaml:"source"`
		Destination struct {
			Server    string `yaml:"server"`
			Namespace string `yaml:"namespace"`
		} `yaml:"destination"`
		SyncPolicy struct {
			Automated struct {
				Prune      bool `yaml:"prune"`
				SelfHeal   bool `yaml:"selfHeal"`
				AllowEmpty bool `yaml:"allowEmpty"`
			} `yaml:"automated"`
			SyncOptions []string `yaml:"syncOptions"`
		} `yaml:"syncPolicy"`
	} `yaml:"spec"`
}

func ApplicationName(applicationID string) string {
	return "kp-h-" + strings.ReplaceAll(strings.ToLower(applicationID), "-", "")
}

func RenderApplication(revision Revision, argoNamespace string) ([]byte, error) {
	if revision.Validate() != nil || !revision.DesiredEnabled || !dnsLabelRE.MatchString(argoNamespace) {
		return nil, ErrInvalid
	}
	source, _ := revision.Source.Normalize()
	var manifest applicationManifest
	manifest.APIVersion = "argoproj.io/v1alpha1"
	manifest.Kind = "Application"
	manifest.Metadata.Name = ApplicationName(revision.Target.ApplicationID)
	manifest.Metadata.Namespace = argoNamespace
	manifest.Metadata.Finalizers = []string{"resources-finalizer.argocd.argoproj.io"}
	manifest.Metadata.Labels = map[string]string{
		"app.kubernetes.io/managed-by": "kuberploy",
		"app.kubernetes.io/component":  "helm-application",
		"kuberploy.io/project-id":      revision.Target.ProjectID,
		"kuberploy.io/environment-id":  revision.Target.EnvironmentID,
		"kuberploy.io/application-id":  revision.Target.ApplicationID,
	}
	manifest.Metadata.Annotations = map[string]string{
		"kuberploy.io/helm-revision-id": revision.ID,
		"kuberploy.io/helm-generation":  strconv.FormatInt(revision.Generation, 10),
		"kuberploy.io/values-digest":    revision.ValuesDigest,
	}
	manifest.Spec.Project = revision.ArgoProject
	manifest.Spec.Source.RepoURL = source.RepositoryURL
	manifest.Spec.Source.Chart = source.Chart
	manifest.Spec.Source.Path = source.Path
	manifest.Spec.Source.TargetRevision = source.TargetRevision
	manifest.Spec.Source.Helm.ReleaseName = revision.ReleaseName
	manifest.Spec.Source.Helm.Values = string(revision.ValuesYAML)
	manifest.Spec.Destination.Server = InClusterURL
	manifest.Spec.Destination.Namespace = revision.DestinationNamespace
	manifest.Spec.SyncPolicy.Automated.Prune = true
	manifest.Spec.SyncPolicy.Automated.SelfHeal = true
	manifest.Spec.SyncPolicy.Automated.AllowEmpty = false
	manifest.Spec.SyncPolicy.SyncOptions = []string{"CreateNamespace=false", "ServerSideApply=true"}
	result, err := yaml.Marshal(manifest)
	if err != nil || len(result) == 0 || len(result) > 512<<10 {
		return nil, ErrInvalid
	}
	return result, nil
}
