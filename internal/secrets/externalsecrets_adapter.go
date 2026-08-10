package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strings"
)

const remoteRevisionAnnotation = "kuberploy.io/remote-material-revision"

// RemoteWriteRequest contains only durable identity and exact destination
// metadata. It deliberately has no URL, credential, arbitrary provider JSON or
// caller-selected namespace/name.
type RemoteWriteRequest struct {
	Scope            Scope
	BindingID        string
	VersionID        string
	TargetSecretName string
	ExplicitKeys     []string
}

type RemoteKeyReference struct {
	SecretKey string
	RemoteKey string
	Property  string
	Version   string
}

type RemoteWriteResult struct {
	Revision   string
	References []RemoteKeyReference
}

type RemoteDeleteRequest struct {
	Namespace        string
	TargetSecretName string
	Revision         string
}

// RemoteMaterialWriter is the deliberately narrow External Secrets backend
// plugin seam. Implementations consume Material synchronously, must be
// idempotent by VersionID, must not retain or log callback bytes, and return
// only an opaque revision plus explicit remote references.
type RemoteMaterialWriter interface {
	StageRemoteMaterial(context.Context, RemoteWriteRequest, *Material) (RemoteWriteResult, error)
	DeleteRemoteMaterial(context.Context, RemoteDeleteRequest) (bool, error)
}

// The provider part of SecretStore is closed inside this package. A concrete,
// typed Vault/cloud writer must implement this private profile boundary; an
// arbitrary JSON fragment cannot be supplied by an API caller or config map.
type externalSecretStoreProvider interface {
	validateExternalSecretStoreProvider() error
	externalSecretStoreProviderSpec() map[string]any
}

type externalSecretStoreProfile struct {
	provider externalSecretStoreProvider
	spec     map[string]any
}

type profiledRemoteMaterialWriter interface {
	RemoteMaterialWriter
	externalSecretStoreProfile() externalSecretStoreProfile
}

// ExternalSecretsAdapter owns exact namespaced SecretStore and ExternalSecret
// objects. It is intentionally impossible to enable without a concrete,
// package-approved remote writer/profile.
type ExternalSecretsAdapter struct {
	resources secretKubernetesResources
	writer    profiledRemoteMaterialWriter
	profile   externalSecretStoreProfile
	now       providerClock
}

func NewInClusterExternalSecretsProvider(writer RemoteMaterialWriter) (*ExternalSecretsAdapter, error) {
	profiled, ok := writer.(profiledRemoteMaterialWriter)
	if !ok || nilProviderDependency(profiled) {
		return nil, ErrInvalid
	}
	resources, err := newInClusterSecretResources()
	if err != nil {
		return nil, err
	}
	return newExternalSecretsAdapter(resources, profiled, nil)
}

func newExternalSecretsAdapter(resources secretKubernetesResources, writer profiledRemoteMaterialWriter, now providerClock) (*ExternalSecretsAdapter, error) {
	if nilProviderDependency(resources) || nilProviderDependency(writer) {
		return nil, ErrInvalid
	}
	profile := writer.externalSecretStoreProfile()
	if profile.provider == nil || profile.provider.validateExternalSecretStoreProvider() != nil {
		return nil, ErrInvalid
	}
	providerSpec := profile.provider.externalSecretStoreProviderSpec()
	if providerSpec == nil {
		return nil, ErrInvalid
	}
	if encoded, err := digestJSON(providerSpec); err != nil || encoded == "" {
		return nil, ErrInvalid
	}
	profile.spec = cloneSecretObject(providerSpec)
	return &ExternalSecretsAdapter{resources: resources, writer: writer, profile: profile, now: now}, nil
}

var _ ExternalSecretsProvider = (*ExternalSecretsAdapter)(nil)

func (a *ExternalSecretsAdapter) StageExternalSecret(ctx context.Context, request StageRequest, material *Material) (Artifact, error) {
	if a == nil || request.Binding.Provider != ProviderExternalSecrets || request.validate() != nil || requireExactKeys(request, material) != nil {
		return Artifact{}, ErrInvalid
	}
	if request.Version.TargetSecretType != TargetSecretOpaque {
		return Artifact{}, ErrInvalid
	}
	contentDigest, err := requestContentDigest(request)
	if err != nil {
		return Artifact{}, err
	}
	storeName := externalStoreName(request.TargetSecretName)
	liveExternal, externalFound, err := liveObject(ctx, a.resources, resourceExternalSecret, request.Binding.Scope.Namespace, request.TargetSecretName)
	if err != nil {
		return Artifact{}, err
	}
	liveStore, storeFound, err := liveObject(ctx, a.resources, resourceSecretStore, request.Binding.Scope.Namespace, storeName)
	if err != nil {
		return Artifact{}, err
	}
	if externalFound && storeFound {
		artifact, adoptErr := externalArtifactFromLive(liveExternal, liveStore, request.TargetSecretName, contentDigest, a.profile)
		if adoptErr != nil || !slices.Equal(externalSecretKeys(liveExternal), request.ExplicitKeys) {
			return Artifact{}, ErrProviderMismatch
		}
		return artifact, nil
	}
	newRemoteWrite := !externalFound && !storeFound

	writeRequest := RemoteWriteRequest{
		Scope: request.Binding.Scope, BindingID: request.Binding.ID, VersionID: request.Version.ID,
		TargetSecretName: request.TargetSecretName, ExplicitKeys: slices.Clone(request.ExplicitKeys),
	}
	result, err := a.writer.StageRemoteMaterial(ctx, writeRequest, material)
	if err != nil || validateRemoteWriteResult(result, request.ExplicitKeys) != nil {
		return Artifact{}, ErrProviderOperation
	}
	desiredStore, desiredExternal, _, err := desiredExternalSecretObjects(request, result, a.profile)
	if err != nil {
		return Artifact{}, err
	}
	createdStore, createdExternal := false, false
	cleanup := func() {
		cleanupContext := context.WithoutCancel(ctx)
		if createdExternal {
			_ = deleteIfExact(cleanupContext, a.resources, resourceExternalSecret, desiredExternal)
		}
		if createdStore {
			_ = deleteIfExact(cleanupContext, a.resources, resourceSecretStore, desiredStore)
		}
		if newRemoteWrite {
			_, _ = a.writer.DeleteRemoteMaterial(cleanupContext, RemoteDeleteRequest{
				Namespace: request.Binding.Scope.Namespace, TargetSecretName: request.TargetSecretName, Revision: result.Revision,
			})
		}
	}

	liveStore, createdStore, err = ensureSecretObject(ctx, a.resources, resourceSecretStore, desiredStore)
	if err != nil {
		cleanup()
		return Artifact{}, err
	}
	liveExternal, createdExternal, err = ensureSecretObject(ctx, a.resources, resourceExternalSecret, desiredExternal)
	if err != nil {
		cleanup()
		return Artifact{}, err
	}
	artifact, err := externalArtifactFromLive(liveExternal, liveStore, request.TargetSecretName, contentDigest, a.profile)
	if err != nil || !slices.Equal(externalSecretKeys(liveExternal), request.ExplicitKeys) {
		cleanup()
		return Artifact{}, ErrProviderMismatch
	}
	return artifact, nil
}

func validateRemoteWriteResult(result RemoteWriteResult, expectedKeys []string) error {
	if !validateRemoteOpaque(result.Revision, 60) || len(result.References) != len(expectedKeys) || len(result.References) == 0 || len(result.References) > MaxMaterialKeys {
		return ErrProviderMismatch
	}
	references := slices.Clone(result.References)
	slices.SortFunc(references, func(left, right RemoteKeyReference) int { return strings.Compare(left.SecretKey, right.SecretKey) })
	keys := make([]string, len(references))
	for index, reference := range references {
		keys[index] = reference.SecretKey
		if !secretKeyRE.MatchString(reference.SecretKey) || !validateRemoteOpaque(reference.RemoteKey, 256) ||
			reference.Property != "" && !validateRemoteOpaque(reference.Property, 128) || !validateRemoteOpaque(reference.Version, 128) {
			return ErrProviderMismatch
		}
		if index > 0 && references[index-1].SecretKey == reference.SecretKey {
			return ErrProviderMismatch
		}
	}
	if !slices.Equal(keys, expectedKeys) {
		return ErrProviderMismatch
	}
	return nil
}

func desiredExternalSecretObjects(request StageRequest, result RemoteWriteResult, profile externalSecretStoreProfile) (map[string]any, map[string]any, string, error) {
	if requestContent, err := requestContentDigest(request); err != nil || validateRemoteWriteResult(result, request.ExplicitKeys) != nil ||
		profile.provider == nil || profile.spec == nil {
		return nil, nil, "", ErrInvalid
	} else {
		labels := exactManagedLabels(request, "external-secret")
		annotations := map[string]any{contentDigestAnnotation: requestContent, remoteRevisionAnnotation: result.Revision}
		storeName := externalStoreName(request.TargetSecretName)
		store := map[string]any{
			"apiVersion": "external-secrets.io/v1", "kind": "SecretStore",
			"metadata": map[string]any{
				"name": storeName, "namespace": request.Binding.Scope.Namespace,
				"labels": cloneSecretValue(labels), "annotations": cloneSecretValue(annotations),
			},
			"spec": map[string]any{
				"controller": externalControllerClass,
				"provider":   cloneSecretObject(profile.spec),
			},
		}
		references := slices.Clone(result.References)
		slices.SortFunc(references, func(left, right RemoteKeyReference) int { return strings.Compare(left.SecretKey, right.SecretKey) })
		data := make([]any, 0, len(references))
		for _, reference := range references {
			remoteRef := map[string]any{
				"key": reference.RemoteKey, "version": reference.Version,
				"conversionStrategy": "Default", "decodingStrategy": "None", "metadataPolicy": "None",
			}
			if reference.Property != "" {
				remoteRef["property"] = reference.Property
			}
			data = append(data, map[string]any{"secretKey": reference.SecretKey, "remoteRef": remoteRef})
		}
		external := map[string]any{
			"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret",
			"metadata": map[string]any{
				"name": request.TargetSecretName, "namespace": request.Binding.Scope.Namespace,
				"labels": cloneSecretValue(labels), "annotations": cloneSecretValue(annotations),
			},
			"spec": map[string]any{
				"refreshPolicy":   ExternalRefreshCreatedOnce,
				"refreshInterval": "0s",
				"secretStoreRef":  map[string]any{"name": storeName, "kind": "SecretStore"},
				"target": map[string]any{
					"name": request.TargetSecretName, "creationPolicy": "Owner", "deletionPolicy": "Delete", "immutable": true,
					"template": map[string]any{
						"type": "Opaque", "engineVersion": "v2", "mergePolicy": "Replace",
						"metadata": map[string]any{"labels": cloneSecretValue(labels)},
					},
				},
				"data": data,
			},
		}
		manifest, digestErr := setManifestDigest(store, external)
		if digestErr != nil {
			return nil, nil, "", digestErr
		}
		return store, external, manifest, nil
	}
}

func externalArtifactFromLive(external, store map[string]any, target, expectedContent string, profile externalSecretStoreProfile) (Artifact, error) {
	if external == nil || store == nil || external["apiVersion"] != "external-secrets.io/v1" || external["kind"] != "ExternalSecret" ||
		store["apiVersion"] != "external-secrets.io/v1" || store["kind"] != "SecretStore" || objectName(external) != target ||
		objectName(store) != externalStoreName(target) || objectNamespace(external) != objectNamespace(store) ||
		validateManagedIdentity(external, ProviderExternalSecrets, "external-secret", target) != nil ||
		validateManagedIdentity(store, ProviderExternalSecrets, "external-secret", target) != nil || !sameManagedVersion(external, store) {
		return Artifact{}, ErrProviderMismatch
	}
	if !validControllerFinalizers(objectMetadata(external)["finalizers"]) || !emptySecretMetadataList(objectMetadata(store)["finalizers"]) {
		return Artifact{}, ErrProviderMismatch
	}
	externalAnnotations, _ := objectMetadata(external)["annotations"].(map[string]any)
	storeAnnotations, _ := objectMetadata(store)["annotations"].(map[string]any)
	if len(externalAnnotations) != 3 || len(storeAnnotations) != 3 || expectedContent != "" && externalAnnotations[contentDigestAnnotation] != expectedContent ||
		externalAnnotations[remoteRevisionAnnotation] != storeAnnotations[remoteRevisionAnnotation] {
		return Artifact{}, ErrProviderMismatch
	}
	remoteRevision, _ := externalAnnotations[remoteRevisionAnnotation].(string)
	manifestDigest, _ := externalAnnotations[manifestDigestAnnotation].(string)
	if !validateRemoteOpaque(remoteRevision, 60) || !digestRE.MatchString(manifestDigest) || verifyManifestDigest(manifestDigest, store, external) != nil {
		return Artifact{}, ErrProviderMismatch
	}
	if profile.provider == nil || profile.spec == nil {
		return Artifact{}, ErrProviderMismatch
	}
	storeSpec, _ := store["spec"].(map[string]any)
	if len(storeSpec) != 2 || storeSpec["controller"] != externalControllerClass || !secretCanonicalEqual(storeSpec["provider"], profile.spec) {
		return Artifact{}, ErrProviderMismatch
	}
	if validateExternalSecretSpec(external, store, target) != nil {
		return Artifact{}, ErrProviderMismatch
	}
	externalUID, err := objectUID(external)
	if err != nil {
		return Artifact{}, err
	}
	storeUID, err := objectUID(store)
	if err != nil {
		return Artifact{}, err
	}
	revision, err := externalProviderRevision(externalUID, storeUID, remoteRevision, objectNamespace(external), objectName(external))
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{
		Provider: ProviderExternalSecrets, Namespace: objectNamespace(external), ObjectName: objectName(external), TargetSecretName: target,
		TargetSecretType: TargetSecretOpaque, ProviderRevision: revision, ManifestDigest: manifestDigest,
	}, nil
}

func validateExternalSecretSpec(external, store map[string]any, target string) error {
	spec, _ := external["spec"].(map[string]any)
	storeRef, _ := spec["secretStoreRef"].(map[string]any)
	targetSpec, _ := spec["target"].(map[string]any)
	template, _ := targetSpec["template"].(map[string]any)
	templateMetadata, _ := template["metadata"].(map[string]any)
	data, _ := spec["data"].([]any)
	if len(spec) != 5 || spec["refreshPolicy"] != ExternalRefreshCreatedOnce || spec["refreshInterval"] != "0s" || len(storeRef) != 2 ||
		storeRef["name"] != objectName(store) || storeRef["kind"] != "SecretStore" || len(targetSpec) != 5 ||
		targetSpec["name"] != target || targetSpec["creationPolicy"] != "Owner" || targetSpec["deletionPolicy"] != "Delete" || targetSpec["immutable"] != true ||
		len(template) != 4 || template["type"] != "Opaque" || template["engineVersion"] != "v2" || template["mergePolicy"] != "Replace" ||
		len(templateMetadata) != 1 || !secretCanonicalEqual(templateMetadata["labels"], objectMetadata(external)["labels"]) || len(data) == 0 || len(data) > MaxMaterialKeys {
		return ErrProviderMismatch
	}
	previous := ""
	for _, raw := range data {
		entry, ok := raw.(map[string]any)
		if !ok || len(entry) != 2 {
			return ErrProviderMismatch
		}
		secretKey, _ := entry["secretKey"].(string)
		remote, _ := entry["remoteRef"].(map[string]any)
		if !secretKeyRE.MatchString(secretKey) || previous != "" && strings.Compare(previous, secretKey) >= 0 || len(remote) < 5 || len(remote) > 6 ||
			!validateRemoteOpaque(stringValue(remote["key"]), 256) || !validateRemoteOpaque(stringValue(remote["version"]), 128) ||
			remote["conversionStrategy"] != "Default" || remote["decodingStrategy"] != "None" || remote["metadataPolicy"] != "None" {
			return ErrProviderMismatch
		}
		if property, exists := remote["property"]; exists && !validateRemoteOpaque(stringValue(property), 128) {
			return ErrProviderMismatch
		}
		previous = secretKey
	}
	if _, exists := spec["dataFrom"]; exists {
		return ErrProviderMismatch
	}
	return nil
}

func externalSecretKeys(external map[string]any) []string {
	spec, _ := external["spec"].(map[string]any)
	data, _ := spec["data"].([]any)
	keys := make([]string, 0, len(data))
	for _, raw := range data {
		entry, _ := raw.(map[string]any)
		key, _ := entry["secretKey"].(string)
		if key == "" {
			return nil
		}
		keys = append(keys, key)
	}
	return keys
}

func (a *ExternalSecretsAdapter) ObserveExternalSecret(ctx context.Context, artifact Artifact) (ReadinessObservation, error) {
	if a == nil || validateProviderArtifactCoordinates(artifact, ProviderExternalSecrets) != nil {
		return ReadinessObservation{}, ErrProviderMismatch
	}
	uidExternal, uidStore, coordinate, _, err := parseExternalProviderRevision(artifact.ProviderRevision)
	if err != nil {
		return ReadinessObservation{}, err
	}
	if coordinate != providerCoordinateFingerprint(artifact.Namespace, artifact.ObjectName) {
		return ReadinessObservation{}, ErrProviderMismatch
	}
	external, externalFound, err := liveObject(ctx, a.resources, resourceExternalSecret, artifact.Namespace, artifact.ObjectName)
	if err != nil {
		return ReadinessObservation{}, err
	}
	store, storeFound, err := liveObject(ctx, a.resources, resourceSecretStore, artifact.Namespace, externalStoreName(artifact.TargetSecretName))
	if err != nil || !externalFound || !storeFound {
		return ReadinessObservation{}, ErrProviderMismatch
	}
	observed, err := externalArtifactFromLive(external, store, artifact.TargetSecretName, "", a.profile)
	if err != nil || observed != artifact || uidFingerprint(stringValue(objectMetadata(external)["uid"])) != uidExternal || uidFingerprint(stringValue(objectMetadata(store)["uid"])) != uidStore {
		return ReadinessObservation{}, ErrProviderMismatch
	}
	storeStatus, _ := conditionState(store, "Ready", false)
	externalStatus, _ := conditionState(external, "Ready", false)
	if storeStatus == ReadinessFailed {
		return ReadinessObservation{Artifact: artifact, Status: ReadinessFailed, FailureCode: "secret-store-not-ready", ObservedAt: providerNow(a.now)}, nil
	}
	if externalStatus == ReadinessFailed {
		return ReadinessObservation{Artifact: artifact, Status: ReadinessFailed, FailureCode: "external-secret-sync-failed", ObservedAt: providerNow(a.now)}, nil
	}
	if storeStatus != ReadinessReady || externalStatus != ReadinessReady {
		return ReadinessObservation{Artifact: artifact, Status: ReadinessPending, ObservedAt: providerNow(a.now)}, nil
	}
	externalStatusMap, _ := external["status"].(map[string]any)
	if !validateRemoteOpaque(stringValue(externalStatusMap["syncedResourceVersion"]), 256) {
		return ReadinessObservation{Artifact: artifact, Status: ReadinessPending, ObservedAt: providerNow(a.now)}, nil
	}
	return ReadinessObservation{Artifact: artifact, Status: ReadinessReady, ObservedAt: providerNow(a.now)}, nil
}

func (a *ExternalSecretsAdapter) DeleteExternalSecret(ctx context.Context, artifact Artifact) (DeleteObservation, error) {
	if a == nil || validateProviderArtifactCoordinates(artifact, ProviderExternalSecrets) != nil {
		return DeleteObservation{}, ErrProviderMismatch
	}
	uidExternal, uidStore, coordinate, remoteRevision, err := parseExternalProviderRevision(artifact.ProviderRevision)
	if err != nil {
		return DeleteObservation{}, err
	}
	if coordinate != providerCoordinateFingerprint(artifact.Namespace, artifact.ObjectName) {
		return DeleteObservation{}, ErrProviderMismatch
	}
	external, externalFound, err := liveObject(ctx, a.resources, resourceExternalSecret, artifact.Namespace, artifact.ObjectName)
	if err != nil {
		return DeleteObservation{}, err
	}
	store, storeFound, err := liveObject(ctx, a.resources, resourceSecretStore, artifact.Namespace, externalStoreName(artifact.TargetSecretName))
	if err != nil {
		return DeleteObservation{}, err
	}
	if externalFound && storeFound {
		if objectDeletionInProgress(external) || objectDeletionInProgress(store) {
			return DeleteObservation{}, ErrProviderOperation
		}
		observed, validationErr := externalArtifactFromLive(external, store, artifact.TargetSecretName, "", a.profile)
		if validationErr != nil || observed != artifact || uidFingerprint(stringValue(objectMetadata(external)["uid"])) != uidExternal || uidFingerprint(stringValue(objectMetadata(store)["uid"])) != uidStore {
			return DeleteObservation{}, ErrProviderMismatch
		}
	} else if externalFound {
		return DeleteObservation{}, ErrProviderMismatch
	} else if storeFound {
		if objectDeletionInProgress(store) {
			return DeleteObservation{}, ErrProviderOperation
		}
		if validateExternalStoreForDelete(store, artifact, uidStore, remoteRevision, a.profile) != nil {
			return DeleteObservation{}, ErrProviderMismatch
		}
	}
	if externalFound {
		if err = deleteExactSecretObject(ctx, a.resources, resourceExternalSecret, external, external); err != nil {
			return DeleteObservation{}, err
		}
		if _, present, getErr := liveObject(ctx, a.resources, resourceExternalSecret, artifact.Namespace, artifact.ObjectName); getErr != nil || present {
			return DeleteObservation{}, ErrProviderOperation
		}
	}
	if storeFound {
		if err = deleteExactSecretObject(ctx, a.resources, resourceSecretStore, store, store); err != nil {
			return DeleteObservation{}, err
		}
		if _, present, getErr := liveObject(ctx, a.resources, resourceSecretStore, artifact.Namespace, objectName(store)); getErr != nil || present {
			return DeleteObservation{}, ErrProviderOperation
		}
	}
	absent, err := a.writer.DeleteRemoteMaterial(ctx, RemoteDeleteRequest{Namespace: artifact.Namespace, TargetSecretName: artifact.TargetSecretName, Revision: remoteRevision})
	if err != nil || !absent {
		return DeleteObservation{}, ErrProviderOperation
	}
	return DeleteObservation{Artifact: artifact, Absent: true, ObservedAt: providerNow(a.now)}, nil
}

func validateExternalStoreForDelete(store map[string]any, artifact Artifact, expectedUIDFingerprint, remoteRevision string, profile externalSecretStoreProfile) error {
	if store == nil || store["apiVersion"] != "external-secrets.io/v1" || store["kind"] != "SecretStore" ||
		objectNamespace(store) != artifact.Namespace || objectName(store) != externalStoreName(artifact.TargetSecretName) ||
		validateManagedIdentity(store, ProviderExternalSecrets, "external-secret", artifact.TargetSecretName) != nil ||
		!emptySecretMetadataList(objectMetadata(store)["finalizers"]) || uidFingerprint(stringValue(objectMetadata(store)["uid"])) != expectedUIDFingerprint {
		return ErrProviderMismatch
	}
	annotations, _ := objectMetadata(store)["annotations"].(map[string]any)
	if len(annotations) != 3 || annotations[manifestDigestAnnotation] != artifact.ManifestDigest || annotations[remoteRevisionAnnotation] != remoteRevision {
		return ErrProviderMismatch
	}
	spec, _ := store["spec"].(map[string]any)
	if len(spec) != 2 || spec["controller"] != externalControllerClass || profile.provider == nil || profile.spec == nil ||
		!secretCanonicalEqual(spec["provider"], profile.spec) {
		return ErrProviderMismatch
	}
	_, err := objectUID(store)
	return err
}

func deleteIfExact(ctx context.Context, resources secretKubernetesResources, resource secretKubernetesResource, desired map[string]any) error {
	live, found, err := liveObject(ctx, resources, resource, objectNamespace(desired), objectName(desired))
	if err != nil || !found {
		return err
	}
	return deleteExactSecretObject(ctx, resources, resource, live, desired)
}

func externalStoreName(target string) string {
	digest := sha256.Sum256([]byte("kuberploy-external-secret-store\x00" + target))
	return "kp-es-" + hex.EncodeToString(digest[:8])
}

func uidFingerprint(uid string) string {
	digest := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(digest[:16])
}

func externalProviderRevision(externalUID, storeUID, remoteRevision, namespace, name string) (string, error) {
	if !kubernetesObjectIDRE.MatchString(externalUID) || !kubernetesObjectIDRE.MatchString(storeUID) || !validateRemoteOpaque(remoteRevision, 60) ||
		!dnsLabelRE.MatchString(namespace) || !kubeNameRE.MatchString(name) {
		return "", ErrProviderMismatch
	}
	encodedRemote := base64.RawURLEncoding.EncodeToString([]byte(remoteRevision))
	revision := "es1." + uidFingerprint(externalUID) + "." + uidFingerprint(storeUID) + "." + providerCoordinateFingerprint(namespace, name) + "." + encodedRemote
	if !safeOpaque(revision, 256) {
		return "", ErrProviderMismatch
	}
	return revision, nil
}

func parseExternalProviderRevision(revision string) (string, string, string, string, error) {
	parts := strings.Split(revision, ".")
	if len(parts) != 5 || parts[0] != "es1" || len(parts[1]) != 32 || len(parts[2]) != 32 || len(parts[3]) != 32 || !safeOpaque(revision, 256) {
		return "", "", "", "", ErrProviderMismatch
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", "", "", "", ErrProviderMismatch
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		return "", "", "", "", ErrProviderMismatch
	}
	if _, err := hex.DecodeString(parts[3]); err != nil {
		return "", "", "", "", ErrProviderMismatch
	}
	remote, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil || !validateRemoteOpaque(string(remote), 60) {
		clear(remote)
		return "", "", "", "", ErrProviderMismatch
	}
	remoteRevision := string(remote)
	clear(remote)
	return parts[1], parts[2], parts[3], remoteRevision, nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
