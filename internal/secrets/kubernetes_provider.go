package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	managedNameLabel         = "app.kubernetes.io/name"
	managedByLabel           = "app.kubernetes.io/managed-by"
	managedComponentLabel    = "app.kubernetes.io/component"
	bindingIdentityLabel     = "kuberploy.io/secret-binding"
	versionIdentityLabel     = "kuberploy.io/secret-version"
	providerIdentityLabel    = "kuberploy.io/secret-provider"
	targetIdentityLabel      = "kuberploy.io/target-secret"
	manifestDigestAnnotation = "kuberploy.io/secret-manifest-digest"
	contentDigestAnnotation  = "kuberploy.io/content-fingerprint"
	sealingKeyAnnotation     = "kuberploy.io/sealing-key-fingerprint"
	externalControllerClass  = "kuberploy"
)

var remoteOpaqueRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+=@-]{0,255}$`)

type providerClock func() time.Time

func exactManagedLabels(request StageRequest, component string) map[string]any {
	return map[string]any{
		managedNameLabel:      "kuberploy-runtime-secret",
		managedByLabel:        "kuberploy",
		managedComponentLabel: component,
		bindingIdentityLabel:  request.Binding.ID,
		versionIdentityLabel:  request.Version.ID,
		providerIdentityLabel: string(request.Binding.Provider),
		targetIdentityLabel:   request.TargetSecretName,
	}
}

func requestContentDigest(request StageRequest) (string, error) {
	if request.validate() != nil || request.Version.ContentFingerprint == [32]byte{} {
		return "", ErrInvalid
	}
	return "sha256:" + hex.EncodeToString(request.Version.ContentFingerprint[:]), nil
}

func digestJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumSecretObjectBytes {
		clear(encoded)
		return "", ErrProviderOperation
	}
	digest := sha256.Sum256(encoded)
	clear(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func ciphertextMapDigest(values map[string]any) (string, error) {
	if len(values) == 0 || len(values) > MaxMaterialKeys {
		return "", ErrProviderMismatch
	}
	for key, raw := range values {
		if !secretKeyRE.MatchString(key) {
			return "", ErrProviderMismatch
		}
		var decoded []byte
		switch value := raw.(type) {
		case string:
			if value == "" || len(value) > 2*MaxValueBytes {
				return "", ErrProviderMismatch
			}
			var err error
			decoded, err = base64.StdEncoding.DecodeString(value)
			if err != nil {
				clear(decoded)
				return "", ErrProviderMismatch
			}
		case []byte:
			decoded = slices.Clone(value)
		default:
			return "", ErrProviderMismatch
		}
		if len(decoded) < 64 || len(decoded) > MaxValueBytes+2048 {
			clear(decoded)
			return "", ErrProviderMismatch
		}
		clear(decoded)
	}
	return digestJSON(values)
}

func ensureSecretObject(ctx context.Context, resources secretKubernetesResources, resource secretKubernetesResource, desired map[string]any) (map[string]any, bool, error) {
	if resources == nil {
		return nil, false, ErrProviderOperation
	}
	namespace, name := objectNamespace(desired), objectName(desired)
	live, err := resources.Get(ctx, resource, namespace, name)
	if err == nil {
		if validateExactSecretObject(live, desired) != nil {
			return nil, false, ErrProviderMismatch
		}
		return live, false, nil
	}
	if !errors.Is(err, errSecretObjectNotFound) {
		return nil, false, ErrProviderOperation
	}
	live, err = resources.Create(ctx, resource, namespace, cloneSecretObject(desired))
	created := err == nil
	if errors.Is(err, errSecretObjectConflict) {
		live, err = resources.Get(ctx, resource, namespace, name)
	}
	if err != nil || validateExactSecretObject(live, desired) != nil {
		return nil, false, ErrProviderMismatch
	}
	return live, created, nil
}

func validateExactSecretObject(live, desired map[string]any) error {
	if live == nil || desired == nil || live["apiVersion"] != desired["apiVersion"] || live["kind"] != desired["kind"] ||
		objectNamespace(live) != objectNamespace(desired) || objectName(live) != objectName(desired) {
		return ErrProviderMismatch
	}
	liveMetadata, desiredMetadata := objectMetadata(live), objectMetadata(desired)
	if liveMetadata == nil || desiredMetadata == nil || validateServerMetadata(liveMetadata) != nil ||
		!secretCanonicalEqual(liveMetadata["labels"], desiredMetadata["labels"]) ||
		!secretCanonicalEqual(liveMetadata["annotations"], desiredMetadata["annotations"]) ||
		!secretCanonicalEqual(normalizeSecretKubernetesJSON(cloneSecretValue(live["spec"])), normalizeSecretKubernetesJSON(cloneSecretValue(desired["spec"]))) {
		return ErrProviderMismatch
	}
	for key := range live {
		if key != "apiVersion" && key != "kind" && key != "metadata" && key != "spec" && key != "status" {
			return ErrProviderMismatch
		}
	}
	return nil
}

func validateServerMetadata(metadata map[string]any) error {
	uid, _ := metadata["uid"].(string)
	resourceVersion, _ := metadata["resourceVersion"].(string)
	if !kubernetesObjectIDRE.MatchString(uid) || !kubernetesObjectIDRE.MatchString(resourceVersion) {
		return ErrProviderMismatch
	}
	if generateName, _ := metadata["generateName"].(string); generateName != "" {
		return ErrProviderMismatch
	}
	if value, exists := metadata["ownerReferences"]; exists && !emptySecretMetadataList(value) {
		return ErrProviderMismatch
	}
	if value, exists := metadata["deletionTimestamp"]; exists && value != nil && value != "" {
		return ErrProviderMismatch
	}
	if value, exists := metadata["deletionGracePeriodSeconds"]; exists && value != nil {
		return ErrProviderMismatch
	}
	if generation, exists := metadata["generation"]; exists {
		integer, ok := secretInteger(generation)
		if !ok || integer < 1 {
			return ErrProviderMismatch
		}
	}
	if finalizers, exists := metadata["finalizers"]; exists && !validControllerFinalizers(finalizers) {
		return ErrProviderMismatch
	}
	return nil
}

func validControllerFinalizers(value any) bool {
	if emptySecretMetadataList(value) {
		return true
	}
	items, ok := value.([]any)
	if !ok || len(items) > 2 {
		return false
	}
	seen := map[string]struct{}{}
	for _, raw := range items {
		item, ok := raw.(string)
		if !ok || item != "externalsecrets.external-secrets.io/externalsecret-cleanup" {
			return false
		}
		if _, duplicate := seen[item]; duplicate {
			return false
		}
		seen[item] = struct{}{}
	}
	return true
}

func emptySecretMetadataList(value any) bool {
	if value == nil {
		return true
	}
	switch typed := value.(type) {
	case []any:
		return len(typed) == 0
	case []string:
		return len(typed) == 0
	default:
		return false
	}
}

func secretObjectIdentity(object map[string]any) (secretDeletePreconditions, error) {
	if validateServerMetadata(objectMetadata(object)) != nil {
		return secretDeletePreconditions{}, ErrProviderMismatch
	}
	metadata := objectMetadata(object)
	uid, _ := metadata["uid"].(string)
	version, _ := metadata["resourceVersion"].(string)
	return secretDeletePreconditions{UID: uid, ResourceVersion: version}, nil
}

func deleteExactSecretObject(ctx context.Context, resources secretKubernetesResources, resource secretKubernetesResource, live, desired map[string]any) error {
	if validateExactSecretObject(live, desired) != nil {
		return ErrProviderMismatch
	}
	preconditions, err := secretObjectIdentity(live)
	if err != nil {
		return err
	}
	err = resources.Delete(ctx, resource, objectNamespace(live), objectName(live), preconditions)
	if err != nil && !errors.Is(err, errSecretObjectNotFound) {
		return ErrProviderOperation
	}
	return nil
}

func liveObject(ctx context.Context, resources secretKubernetesResources, resource secretKubernetesResource, namespace, name string) (map[string]any, bool, error) {
	object, err := resources.Get(ctx, resource, namespace, name)
	if errors.Is(err, errSecretObjectNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, ErrProviderOperation
	}
	return object, true, nil
}

func objectUID(object map[string]any) (string, error) {
	identity, err := secretObjectIdentity(object)
	if err != nil {
		return "", err
	}
	return identity.UID, nil
}

func encodeRevisionPart(value string) (string, error) {
	if !remoteOpaqueRE.MatchString(value) {
		return "", ErrProviderMismatch
	}
	return base64.RawURLEncoding.EncodeToString([]byte(value)), nil
}

func decodeRevisionPart(value string) (string, error) {
	if value == "" || len(value) > 344 {
		return "", ErrProviderMismatch
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || !remoteOpaqueRE.Match(decoded) {
		clear(decoded)
		return "", ErrProviderMismatch
	}
	result := string(decoded)
	clear(decoded)
	return result, nil
}

func cloneSecretObject(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	return cloneSecretValue(input).(map[string]any)
}

func secretCanonicalEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	equal := leftErr == nil && rightErr == nil && len(leftJSON) <= maximumSecretObjectBytes && len(rightJSON) <= maximumSecretObjectBytes &&
		string(leftJSON) == string(rightJSON)
	clear(leftJSON)
	clear(rightJSON)
	return equal
}

func cloneSecretValue(input any) any {
	switch typed := input.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, value := range typed {
			result[key] = cloneSecretValue(value)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, value := range typed {
			result[index] = cloneSecretValue(value)
		}
		return result
	case []byte:
		return slices.Clone(typed)
	default:
		return input
	}
}

func clearStringMap(object map[string]any, path ...string) {
	current := object
	for _, part := range path[:len(path)-1] {
		next, _ := current[part].(map[string]any)
		if next == nil {
			return
		}
		current = next
	}
	values, _ := current[path[len(path)-1]].(map[string]any)
	for key := range values {
		values[key] = ""
		delete(values, key)
	}
}

func secretInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case float64:
		if typed == float64(int64(typed)) {
			return int64(typed), true
		}
	}
	return 0, false
}

func objectGeneration(object map[string]any) (int64, bool) {
	generation, exists := objectMetadata(object)["generation"]
	if !exists {
		return 0, false
	}
	return secretInteger(generation)
}

func objectDeletionInProgress(object map[string]any) bool {
	value, exists := objectMetadata(object)["deletionTimestamp"]
	if !exists || value == nil || value == "" {
		return false
	}
	_, ok := value.(string)
	return ok
}

func conditionState(object map[string]any, conditionType string, requireObservedGeneration bool) (ReadinessStatus, string) {
	status, _ := object["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	if len(conditions) == 0 || len(conditions) > 16 {
		return ReadinessPending, ""
	}
	if requireObservedGeneration {
		generation, generationOK := objectGeneration(object)
		observed, observedOK := secretInteger(status["observedGeneration"])
		if !generationOK || !observedOK || observed != generation {
			return ReadinessPending, ""
		}
	}
	var matched map[string]any
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			return ReadinessPending, ""
		}
		typeName, _ := condition["type"].(string)
		if typeName == conditionType {
			if matched != nil {
				return ReadinessPending, ""
			}
			matched = condition
		}
	}
	if matched == nil {
		return ReadinessPending, ""
	}
	conditionStatus, _ := matched["status"].(string)
	switch conditionStatus {
	case "True":
		return ReadinessReady, ""
	case "False":
		return ReadinessFailed, "provider-reconciliation-failed"
	default:
		return ReadinessPending, ""
	}
}

func manifestEnvelopeDigest(objects ...map[string]any) (string, error) {
	values := make([]any, 0, len(objects))
	for _, object := range objects {
		metadata := objectMetadata(object)
		annotations, _ := metadata["annotations"].(map[string]any)
		annotationCopy := cloneSecretValue(annotations).(map[string]any)
		delete(annotationCopy, manifestDigestAnnotation)
		values = append(values, map[string]any{
			"apiVersion": object["apiVersion"], "kind": object["kind"],
			"metadata": map[string]any{
				"name": objectName(object), "namespace": objectNamespace(object),
				"labels": cloneSecretValue(metadata["labels"]), "annotations": annotationCopy,
			},
			"spec": cloneSecretValue(object["spec"]),
		})
	}
	return digestJSON(values)
}

func setManifestDigest(objects ...map[string]any) (string, error) {
	digest, err := manifestEnvelopeDigest(objects...)
	if err != nil {
		return "", err
	}
	for _, object := range objects {
		metadata := objectMetadata(object)
		annotations, _ := metadata["annotations"].(map[string]any)
		if annotations == nil {
			annotations = map[string]any{}
			metadata["annotations"] = annotations
		}
		annotations[manifestDigestAnnotation] = digest
	}
	return digest, nil
}

func verifyManifestDigest(expected string, objects ...map[string]any) error {
	if !digestRE.MatchString(expected) {
		return ErrProviderMismatch
	}
	for _, object := range objects {
		annotations, _ := objectMetadata(object)["annotations"].(map[string]any)
		if annotations == nil || annotations[manifestDigestAnnotation] != expected {
			return ErrProviderMismatch
		}
	}
	digest, err := manifestEnvelopeDigest(objects...)
	if err != nil || digest != expected {
		return ErrProviderMismatch
	}
	return nil
}

func validateManagedIdentity(object map[string]any, provider ProviderKind, component, target string) error {
	if !provider.valid() || !kubeNameRE.MatchString(target) || objectNamespace(object) == "" || objectName(object) == "" {
		return ErrProviderMismatch
	}
	labels, _ := objectMetadata(object)["labels"].(map[string]any)
	if len(labels) != 7 || labels[managedNameLabel] != "kuberploy-runtime-secret" || labels[managedByLabel] != "kuberploy" ||
		labels[managedComponentLabel] != component || labels[providerIdentityLabel] != string(provider) || labels[targetIdentityLabel] != target {
		return ErrProviderMismatch
	}
	bindingID, bindingOK := labels[bindingIdentityLabel].(string)
	versionID, versionOK := labels[versionIdentityLabel].(string)
	if !bindingOK || !versionOK || !uuidRE.MatchString(bindingID) || !uuidRE.MatchString(versionID) {
		return ErrProviderMismatch
	}
	annotations, _ := objectMetadata(object)["annotations"].(map[string]any)
	content, _ := annotations[contentDigestAnnotation].(string)
	manifest, _ := annotations[manifestDigestAnnotation].(string)
	if !digestRE.MatchString(content) || !digestRE.MatchString(manifest) {
		return ErrProviderMismatch
	}
	return nil
}

func sameManagedVersion(objects ...map[string]any) bool {
	if len(objects) < 2 {
		return true
	}
	firstLabels, _ := objectMetadata(objects[0])["labels"].(map[string]any)
	firstAnnotations, _ := objectMetadata(objects[0])["annotations"].(map[string]any)
	for _, object := range objects[1:] {
		labels, _ := objectMetadata(object)["labels"].(map[string]any)
		annotations, _ := objectMetadata(object)["annotations"].(map[string]any)
		for _, key := range []string{bindingIdentityLabel, versionIdentityLabel, providerIdentityLabel, targetIdentityLabel} {
			if labels[key] != firstLabels[key] {
				return false
			}
		}
		if annotations[contentDigestAnnotation] != firstAnnotations[contentDigestAnnotation] || annotations[manifestDigestAnnotation] != firstAnnotations[manifestDigestAnnotation] {
			return false
		}
	}
	return true
}

func providerNow(clock providerClock) time.Time {
	if clock == nil {
		return time.Now().UTC()
	}
	return clock().UTC()
}

func fixedProviderError(err error) error {
	if errors.Is(err, ErrInvalid) || errors.Is(err, ErrProviderMismatch) {
		return err
	}
	return ErrProviderOperation
}

func requireExactKeys(request StageRequest, material *Material) error {
	keys, err := material.keys()
	if err != nil || !slices.Equal(keys, request.ExplicitKeys) {
		return ErrInvalid
	}
	return nil
}

func nilProviderDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func validateProviderArtifactCoordinates(artifact Artifact, provider ProviderKind) error {
	if artifact.Provider != provider || !dnsLabelRE.MatchString(artifact.Namespace) ||
		!kubeNameRE.MatchString(artifact.ObjectName) || !kubeNameRE.MatchString(artifact.TargetSecretName) ||
		artifact.ObjectName != artifact.TargetSecretName || !digestRE.MatchString(artifact.ManifestDigest) || !safeOpaque(artifact.ProviderRevision, 256) {
		return ErrProviderMismatch
	}
	if provider == ProviderSealedSecrets {
		if !digestRE.MatchString(artifact.SealedKeyFingerprint) || !digestRE.MatchString(artifact.CiphertextDigest) {
			return ErrProviderMismatch
		}
	} else if artifact.SealedKeyFingerprint != "" || artifact.CiphertextDigest != "" {
		return ErrProviderMismatch
	}
	return nil
}

func providerCoordinateFingerprint(namespace, name string) string {
	digest := sha256.Sum256([]byte("kuberploy-runtime-secret-coordinate-v1\x00" + namespace + "\x00" + name))
	return hex.EncodeToString(digest[:16])
}

func validateRemoteOpaque(value string, max int) bool {
	return len(value) <= max && remoteOpaqueRE.MatchString(value) && strings.TrimSpace(value) == value
}

func providerRevision(parts ...string) (string, error) {
	encoded := make([]string, len(parts))
	for index, part := range parts {
		value, err := encodeRevisionPart(part)
		if err != nil {
			return "", err
		}
		encoded[index] = value
	}
	revision := strings.Join(encoded, ".")
	if !safeOpaque(revision, 256) {
		return "", ErrProviderMismatch
	}
	return revision, nil
}

func parseProviderRevision(revision string, count int) ([]string, error) {
	parts := strings.Split(revision, ".")
	if len(parts) != count || !safeOpaque(revision, 256) {
		return nil, ErrProviderMismatch
	}
	decoded := make([]string, count)
	for index, part := range parts {
		value, err := decodeRevisionPart(part)
		if err != nil {
			return nil, err
		}
		decoded[index] = value
	}
	return decoded, nil
}

func describeProviderObject(object map[string]any) string {
	return fmt.Sprintf("%s/%s", objectNamespace(object), objectName(object))
}
