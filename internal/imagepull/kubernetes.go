package imagepull

import (
	"context"
	"crypto/subtle"
	"errors"
	"strconv"
)

var (
	errSecretNotFound = errors.New("runtime image-pull Secret not found")
	errSecretConflict = errors.New("runtime image-pull Secret already exists")
)

const (
	managedByLabel        = "app.kubernetes.io/managed-by"
	componentLabel        = "app.kubernetes.io/component"
	environmentLabel      = "kuberploy.io/environment-id"
	registryTargetLabel   = "kuberploy.io/registry-target-id"
	profileRevisionLabel  = "kuberploy.io/profile-revision"
	credentialRefAnnotate = "kuberploy.io/pull-credential-ref"
)

type kubernetesSecret struct {
	APIVersion string                   `json:"apiVersion"`
	Kind       string                   `json:"kind"`
	Metadata   kubernetesSecretMetadata `json:"metadata"`
	Immutable  bool                     `json:"immutable"`
	Type       string                   `json:"type"`
	Data       map[string][]byte        `json:"data"`
}

type kubernetesSecretMetadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	GenerateName      string            `json:"generateName,omitempty"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	OwnerReferences   []any             `json:"ownerReferences,omitempty"`
	Finalizers        []string          `json:"finalizers,omitempty"`
	DeletionTimestamp any               `json:"deletionTimestamp,omitempty"`
}

type kubernetesSecretResources interface {
	Get(context.Context, string, string) (kubernetesSecret, error)
	Create(context.Context, string, kubernetesSecret) (kubernetesSecret, error)
}

type KubernetesSecretAPI struct {
	resources kubernetesSecretResources
}

func NewKubernetesSecretAPI(resources kubernetesSecretResources) (*KubernetesSecretAPI, error) {
	if resources == nil {
		return nil, ErrInvalid
	}
	return &KubernetesSecretAPI{resources: resources}, nil
}

func (a *KubernetesSecretAPI) EnsureImagePullSecret(ctx context.Context, request SecretRequest) (SecretObservation, error) {
	// Source Secret coordinates do not participate in the destination object;
	// request validation has already bound the material to the full real
	// profile. The exact destination metadata is revalidated below.
	profile := Profile{Name: request.ProfileName, TargetID: request.RegistryTargetID, RegistryServer: request.RegistryServer,
		CredentialRef: request.PullCredentialRef, Revision: request.ProfileRevision,
		SourceSecretRef: "runtime-pull-source", SourceSecretKey: ".dockerconfigjson"}
	if request.DesiredArtifact.Validate() != nil || request.RegistryServer == "" || len(request.DockerConfig) == 0 ||
		len(request.RegistryServer) > 253 || !registryServerPattern.MatchString(request.RegistryServer) ||
		profile.Validate() != nil || ValidateDockerConfig(request.DockerConfig, profile) != nil {
		return SecretObservation{}, ErrInvalid
	}
	desired := desiredKubernetesSecret(request)
	defer clearKubernetesSecret(&desired)
	live, err := a.resources.Get(ctx, request.Namespace, request.SecretName)
	if errors.Is(err, errSecretNotFound) {
		create := cloneKubernetesSecret(desired)
		created, createErr := a.resources.Create(ctx, request.Namespace, create)
		if createErr == nil {
			live = cloneKubernetesSecret(created)
		}
		clearKubernetesSecret(&created)
		clearKubernetesSecret(&create)
		err = createErr
		if errors.Is(err, errSecretConflict) {
			live, err = a.resources.Get(ctx, request.Namespace, request.SecretName)
		}
	}
	if err != nil {
		clearKubernetesSecret(&live)
		return SecretObservation{}, ErrUnavailable
	}
	defer clearKubernetesSecret(&live)
	if !exactKubernetesSecret(live, desired) {
		return SecretObservation{}, ErrConflict
	}
	return SecretObservation{Namespace: live.Metadata.Namespace, Name: live.Metadata.Name,
		UID: live.Metadata.UID, ResourceVersion: live.Metadata.ResourceVersion}, nil
}

func desiredKubernetesSecret(request SecretRequest) kubernetesSecret {
	return kubernetesSecret{
		APIVersion: "v1", Kind: "Secret", Immutable: true, Type: "kubernetes.io/dockerconfigjson",
		Metadata: kubernetesSecretMetadata{Name: request.SecretName, Namespace: request.Namespace,
			Labels: map[string]string{managedByLabel: "kuberploy", componentLabel: "runtime-registry-pull",
				environmentLabel: request.EnvironmentID, registryTargetLabel: request.RegistryTargetID,
				profileRevisionLabel: strconv.FormatInt(request.ProfileRevision, 10)},
			Annotations: map[string]string{credentialRefAnnotate: request.PullCredentialRef}},
		Data: map[string][]byte{".dockerconfigjson": append([]byte(nil), request.DockerConfig...)},
	}
}

func exactKubernetesSecret(live, desired kubernetesSecret) bool {
	if live.APIVersion != desired.APIVersion || live.Kind != desired.Kind || live.Type != desired.Type || !live.Immutable ||
		live.Metadata.Name != desired.Metadata.Name || live.Metadata.Namespace != desired.Metadata.Namespace ||
		!kubernetesUIDPattern.MatchString(live.Metadata.UID) || !resourceVersionPattern.MatchString(live.Metadata.ResourceVersion) ||
		live.Metadata.GenerateName != "" || len(live.Metadata.OwnerReferences) != 0 || len(live.Metadata.Finalizers) != 0 || live.Metadata.DeletionTimestamp != nil ||
		!exactStringMap(live.Metadata.Labels, desired.Metadata.Labels) || !exactStringMap(live.Metadata.Annotations, desired.Metadata.Annotations) ||
		len(live.Data) != 1 || len(desired.Data) != 1 {
		return false
	}
	liveValue, liveOK := live.Data[".dockerconfigjson"]
	desiredValue, desiredOK := desired.Data[".dockerconfigjson"]
	return liveOK && desiredOK && len(liveValue) == len(desiredValue) && subtle.ConstantTimeCompare(liveValue, desiredValue) == 1
}

func exactStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func cloneKubernetesSecret(value kubernetesSecret) kubernetesSecret {
	value.Metadata.Labels = cloneStringMap(value.Metadata.Labels)
	value.Metadata.Annotations = cloneStringMap(value.Metadata.Annotations)
	value.Metadata.OwnerReferences = append([]any(nil), value.Metadata.OwnerReferences...)
	value.Metadata.Finalizers = append([]string(nil), value.Metadata.Finalizers...)
	sourceData := value.Data
	value.Data = make(map[string][]byte, len(sourceData))
	for key, item := range sourceData {
		value.Data[key] = append([]byte(nil), item...)
	}
	return value
}

func cloneStringMap(value map[string]string) map[string]string {
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func clearKubernetesSecret(value *kubernetesSecret) {
	if value == nil {
		return
	}
	for key, item := range value.Data {
		clearBytes(item)
		delete(value.Data, key)
	}
}

var _ SecretAPI = (*KubernetesSecretAPI)(nil)
