package imagepull

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeSecretResources struct {
	get       kubernetesSecret
	getErr    error
	create    kubernetesSecret
	createErr error
	getCalls  int
	creates   int
	captured  kubernetesSecret
}

func (r *fakeSecretResources) Get(context.Context, string, string) (kubernetesSecret, error) {
	r.getCalls++
	return cloneKubernetesSecret(r.get), r.getErr
}

func (r *fakeSecretResources) Create(_ context.Context, _ string, value kubernetesSecret) (kubernetesSecret, error) {
	r.creates++
	r.captured = value
	if r.createErr != nil {
		return kubernetesSecret{}, r.createErr
	}
	if r.create.APIVersion != "" {
		return cloneKubernetesSecret(r.create), nil
	}
	value.Metadata.UID = "44444444-4444-4444-8444-444444444444"
	value.Metadata.ResourceVersion = "123"
	return value, nil
}

func kubernetesRequest(t *testing.T) SecretRequest {
	t.Helper()
	desired, err := Desired(testRuntimeConfig(), testEnvironmentID, "tenant-a-dev", testTargetID)
	if err != nil {
		t.Fatal(err)
	}
	return SecretRequest{DesiredArtifact: desired, RegistryServer: "registry.example.test:5000",
		DockerConfig: []byte(`{"auths":{"registry.example.test:5000":{"auth":"dXNlcjpwYXNz"}}}`)}
}

func liveSecret(request SecretRequest) kubernetesSecret {
	value := desiredKubernetesSecret(request)
	value.Metadata.UID = "44444444-4444-4444-8444-444444444444"
	value.Metadata.ResourceVersion = "123"
	return value
}

func TestKubernetesSecretAPICreatesOrAdoptsOnlyExactImmutableSecret(t *testing.T) {
	request := kubernetesRequest(t)
	resources := &fakeSecretResources{getErr: errSecretNotFound}
	api, err := NewKubernetesSecretAPI(resources)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := api.EnsureImagePullSecret(t.Context(), request)
	if err != nil || resources.creates != 1 || resources.getCalls != 1 || observation.Namespace != request.Namespace ||
		observation.Name != request.SecretName || observation.UID == "" {
		t.Fatalf("observation=%#v gets=%d creates=%d err=%v", observation, resources.getCalls, resources.creates, err)
	}
	if value := resources.captured.Data[".dockerconfigjson"]; len(value) > 0 && strings.Trim(string(value), "\x00") != "" {
		t.Fatalf("create request credential copy was retained: %q", value)
	}

	resources = &fakeSecretResources{get: liveSecret(request)}
	api, _ = NewKubernetesSecretAPI(resources)
	observation, err = api.EnsureImagePullSecret(t.Context(), request)
	if err != nil || resources.creates != 0 || observation.ResourceVersion != "123" {
		t.Fatalf("adoption=%#v creates=%d err=%v", observation, resources.creates, err)
	}
}

func TestKubernetesSecretAPIRecoversCreateConflictByExactRead(t *testing.T) {
	request := kubernetesRequest(t)
	resources := &fakeSecretResources{getErr: errSecretNotFound, createErr: errSecretConflict}
	api, _ := NewKubernetesSecretAPI(resources)
	// The second GET returns the exact object after another worker won create.
	first := true
	resourcesGet := &sequencedSecretResources{base: resources, next: liveSecret(request), first: &first}
	api, _ = NewKubernetesSecretAPI(resourcesGet)
	observation, err := api.EnsureImagePullSecret(t.Context(), request)
	if err != nil || observation.UID == "" || resources.getCalls != 2 || resources.creates != 1 {
		t.Fatalf("conflict adoption=%#v gets=%d creates=%d err=%v", observation, resources.getCalls, resources.creates, err)
	}
}

type sequencedSecretResources struct {
	base  *fakeSecretResources
	next  kubernetesSecret
	first *bool
}

func (r *sequencedSecretResources) Get(ctx context.Context, namespace, name string) (kubernetesSecret, error) {
	if *r.first {
		*r.first = false
		return r.base.Get(ctx, namespace, name)
	}
	r.base.getCalls++
	return cloneKubernetesSecret(r.next), nil
}

func (r *sequencedSecretResources) Create(ctx context.Context, namespace string, value kubernetesSecret) (kubernetesSecret, error) {
	return r.base.Create(ctx, namespace, value)
}

func TestKubernetesSecretAPIRejectsEveryIdentityAndContentMutation(t *testing.T) {
	request := kubernetesRequest(t)
	for name, mutate := range map[string]func(*kubernetesSecret){
		"mutable":     func(value *kubernetesSecret) { value.Immutable = false },
		"wrong type":  func(value *kubernetesSecret) { value.Type = "Opaque" },
		"data":        func(value *kubernetesSecret) { value.Data[".dockerconfigjson"][0] ^= 1 },
		"extra data":  func(value *kubernetesSecret) { value.Data["token"] = []byte("private") },
		"label":       func(value *kubernetesSecret) { value.Metadata.Labels[componentLabel] = "other" },
		"extra label": func(value *kubernetesSecret) { value.Metadata.Labels["attacker"] = "true" },
		"annotation":  func(value *kubernetesSecret) { value.Metadata.Annotations[credentialRefAnnotate] = "other" },
		"owner": func(value *kubernetesSecret) {
			value.Metadata.OwnerReferences = []any{map[string]any{"uid": "attacker"}}
		},
		"finalizer":       func(value *kubernetesSecret) { value.Metadata.Finalizers = []string{"attacker"} },
		"deleting":        func(value *kubernetesSecret) { value.Metadata.DeletionTimestamp = "2026-08-09T00:00:00Z" },
		"resourceVersion": func(value *kubernetesSecret) { value.Metadata.ResourceVersion = "unsafe\nvalue" },
	} {
		t.Run(name, func(t *testing.T) {
			live := liveSecret(request)
			mutate(&live)
			resources := &fakeSecretResources{get: live}
			api, _ := NewKubernetesSecretAPI(resources)
			_, err := api.EnsureImagePullSecret(t.Context(), request)
			if !errors.Is(err, ErrConflict) || strings.Contains(err.Error(), "private") {
				t.Fatalf("mutation result=%v", err)
			}
		})
	}
}

func TestKubernetesSecretAPIRejectsInvalidMaterialAtDirectBoundary(t *testing.T) {
	request := kubernetesRequest(t)
	request.DockerConfig = []byte(`{"auths":{"evil.example.test":{"auth":"private"}}}`)
	resources := &fakeSecretResources{}
	api, _ := NewKubernetesSecretAPI(resources)
	if _, err := api.EnsureImagePullSecret(t.Context(), request); !errors.Is(err, ErrInvalid) || resources.getCalls != 0 {
		t.Fatalf("invalid direct request err=%v calls=%d", err, resources.getCalls)
	}
}

func TestKubernetesSecretDecoderRejectsDuplicateUnknownAndTrailingFields(t *testing.T) {
	request := kubernetesRequest(t)
	live := liveSecret(request)
	encoded, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeKubernetesSecret(encoded)
	if err != nil || !exactKubernetesSecret(decoded, live) {
		clearKubernetesSecret(&decoded)
		t.Fatalf("decoded exact object err=%v", err)
	}
	clearKubernetesSecret(&decoded)
	for name, raw := range map[string][]byte{
		"duplicate": bytes.Replace(encoded, []byte(`"type":"kubernetes.io/dockerconfigjson"`), []byte(`"type":"Opaque","type":"kubernetes.io/dockerconfigjson"`), 1),
		"unknown":   bytes.Replace(encoded, []byte(`"type":"kubernetes.io/dockerconfigjson"`), []byte(`"stringData":{"password":"private"},"type":"kubernetes.io/dockerconfigjson"`), 1),
		"trailing":  append(append([]byte(nil), encoded...), []byte(` {}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			object, decodeErr := decodeKubernetesSecret(raw)
			clearKubernetesSecret(&object)
			if !errors.Is(decodeErr, ErrUnavailable) || strings.Contains(decodeErr.Error(), "private") {
				t.Fatalf("unsafe decode err=%v", decodeErr)
			}
		})
	}
}

func TestImagePullSecretPathIsExactAndNonEscaping(t *testing.T) {
	path, err := imagePullSecretPath("tenant-a-dev", "kuberploy-pull-aaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || path != "/api/v1/namespaces/tenant-a-dev/secrets/kuberploy-pull-aaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	for _, value := range []struct{ namespace, name string }{{"../default", "valid"}, {"tenant-a-dev", "../secret"}, {"tenant_a", "secret"}} {
		if _, err = imagePullSecretPath(value.namespace, value.name); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe path accepted: %#v err=%v", value, err)
		}
	}
}
