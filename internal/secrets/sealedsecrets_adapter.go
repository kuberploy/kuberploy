package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// DefaultSealedSecretsCertificatePath is an operator-controlled public
	// certificate projection. It is never selected by an API request.
	DefaultSealedSecretsCertificateKey  = "tls.crt"
	DefaultSealedSecretsCertificatePath = "/var/run/secrets/kuberploy-system/sealed-secrets/" + DefaultSealedSecretsCertificateKey
	maximumSealingCertificateBytes      = 64 << 10
)

type sealingPublicKey struct {
	key         *rsa.PublicKey
	fingerprint string
}

type sealingPublicKeySource interface {
	ActivePublicKey(context.Context, time.Time) (sealingPublicKey, error)
}

type projectedSealingCertificate struct{ path string }

func (p projectedSealingCertificate) ActivePublicKey(ctx context.Context, now time.Time) (sealingPublicKey, error) {
	if p.path != DefaultSealedSecretsCertificatePath || now.IsZero() || ctx.Err() != nil {
		return sealingPublicKey{}, ErrProviderOperation
	}
	return readProjectedSealingCertificate(ctx, p.path, now)
}

func readProjectedSealingCertificate(ctx context.Context, path string, now time.Time) (sealingPublicKey, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) || now.IsZero() || ctx.Err() != nil {
		return sealingPublicKey{}, ErrProviderOperation
	}
	file, err := openProjectedFile(path)
	if err != nil {
		return sealingPublicKey{}, ErrProviderOperation
	}
	info, statErr := file.Stat()
	if statErr != nil || !secureProjectedRegularFile(info) || info.Size() < 1 || info.Size() > maximumSealingCertificateBytes {
		file.Close() //nolint:errcheck
		return sealingPublicKey{}, ErrProviderOperation
	}
	encoded, readErr := io.ReadAll(io.LimitReader(file, maximumSealingCertificateBytes+1))
	closeErr := file.Close()
	defer clear(encoded)
	if readErr != nil || closeErr != nil || len(encoded) == 0 || len(encoded) > maximumSealingCertificateBytes || ctx.Err() != nil {
		return sealingPublicKey{}, ErrProviderOperation
	}
	return parseSealingCertificate(encoded, now)
}

func parseSealingCertificate(encoded []byte, now time.Time) (sealingPublicKey, error) {
	block, rest := pem.Decode(encoded)
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return sealingPublicKey{}, ErrProviderOperation
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return sealingPublicKey{}, ErrProviderOperation
	}
	publicKey, ok := certificate.PublicKey.(*rsa.PublicKey)
	if !ok || publicKey.N == nil || publicKey.N.BitLen() < 2048 || publicKey.N.BitLen() > 8192 || publicKey.E < 3 {
		return sealingPublicKey{}, ErrProviderOperation
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return sealingPublicKey{}, ErrProviderOperation
	}
	digest := sha256.Sum256(der)
	clear(der)
	return sealingPublicKey{key: publicKey, fingerprint: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

// StrictSealedSecretsAdapter creates only strict namespace/name-bound
// SealedSecret resources. It never reads the resulting Secret.
type StrictSealedSecretsAdapter struct {
	resources secretKubernetesResources
	keys      sealingPublicKeySource
	random    io.Reader
	now       providerClock
}

// NewInClusterStrictSealedSecretsProvider constructs the self-contained
// production adapter. The controller certificate must be projected at the
// fixed DefaultSealedSecretsCertificatePath.
func NewInClusterStrictSealedSecretsProvider() (*StrictSealedSecretsAdapter, error) {
	resources, err := newInClusterSecretResources()
	if err != nil {
		return nil, err
	}
	return newStrictSealedSecretsAdapter(resources, projectedSealingCertificate{path: DefaultSealedSecretsCertificatePath}, rand.Reader, nil)
}

func newStrictSealedSecretsAdapter(resources secretKubernetesResources, keys sealingPublicKeySource, random io.Reader, now providerClock) (*StrictSealedSecretsAdapter, error) {
	if nilProviderDependency(resources) || nilProviderDependency(keys) || nilProviderDependency(random) {
		return nil, ErrInvalid
	}
	return &StrictSealedSecretsAdapter{resources: resources, keys: keys, random: random, now: now}, nil
}

var _ SealedSecretsProvider = (*StrictSealedSecretsAdapter)(nil)

func (a *StrictSealedSecretsAdapter) StageStrictSealedSecret(ctx context.Context, request StageRequest, material *Material) (Artifact, error) {
	if a == nil || request.Binding.Provider != ProviderSealedSecrets || request.validate() != nil || requireExactKeys(request, material) != nil {
		return Artifact{}, ErrInvalid
	}
	contentDigest, err := requestContentDigest(request)
	if err != nil {
		return Artifact{}, err
	}
	if live, found, getErr := liveObject(ctx, a.resources, resourceSealedSecret, request.Binding.Scope.Namespace, request.TargetSecretName); getErr != nil {
		return Artifact{}, getErr
	} else if found {
		defer clearSealedObjectCiphertext(live)
		artifact, adoptErr := sealedArtifactFromLive(live, request.TargetSecretName, contentDigest, request.Version.TargetSecretType)
		if adoptErr != nil || !slices.Equal(sealedSecretKeys(live), request.ExplicitKeys) {
			return Artifact{}, ErrProviderMismatch
		}
		return artifact, nil
	}

	activeKey, err := a.keys.ActivePublicKey(ctx, providerNow(a.now))
	if err != nil || activeKey.key == nil || !digestRE.MatchString(activeKey.fingerprint) {
		return Artifact{}, ErrProviderOperation
	}
	encrypted := make(map[string]any, len(request.ExplicitKeys))
	defer clearCiphertextValues(encrypted)
	label := []byte(request.Binding.Scope.Namespace + "/" + request.TargetSecretName)
	err = material.WithEntries(func(key string, value []byte) error {
		ciphertext, sealErr := sealStrictValue(a.random, activeKey.key, label, value)
		if sealErr != nil {
			clear(ciphertext)
			return ErrProviderOperation
		}
		encrypted[key] = ciphertext
		return nil
	})
	clear(label)
	if err != nil {
		return Artifact{}, fixedProviderError(err)
	}
	desired, _, err := desiredStrictSealedSecret(request, encrypted, activeKey.fingerprint)
	if err != nil {
		return Artifact{}, err
	}
	defer clearCiphertextValues(desired["spec"].(map[string]any)["encryptedData"].(map[string]any))
	payload := cloneSecretObject(desired)
	defer clearCiphertextValues(payload["spec"].(map[string]any)["encryptedData"].(map[string]any))
	live, createErr := a.resources.Create(ctx, resourceSealedSecret, request.Binding.Scope.Namespace, payload)
	if errors.Is(createErr, errSecretObjectConflict) {
		live, _, createErr = liveObject(ctx, a.resources, resourceSealedSecret, request.Binding.Scope.Namespace, request.TargetSecretName)
		if createErr == nil && live == nil {
			createErr = ErrProviderOperation
		}
	}
	if createErr != nil {
		return Artifact{}, ErrProviderOperation
	}
	defer clearSealedObjectCiphertext(live)
	if validateExactSecretObject(live, desired) != nil {
		// A concurrent retry can create different randomized ciphertext for the
		// same immutable version. Adopt only after validating its exact content
		// fingerprint, strict template and manifest.
		artifact, adoptErr := sealedArtifactFromLive(live, request.TargetSecretName, contentDigest, request.Version.TargetSecretType)
		if adoptErr != nil || !slices.Equal(sealedSecretKeys(live), request.ExplicitKeys) {
			return Artifact{}, ErrProviderMismatch
		}
		return artifact, nil
	}
	artifact, err := sealedArtifactFromLive(live, request.TargetSecretName, contentDigest, request.Version.TargetSecretType)
	if err != nil || !slices.Equal(sealedSecretKeys(live), request.ExplicitKeys) {
		return Artifact{}, ErrProviderMismatch
	}
	return artifact, nil
}

func desiredStrictSealedSecret(request StageRequest, encrypted map[string]any, keyFingerprint string) (map[string]any, string, error) {
	contentDigest, err := requestContentDigest(request)
	if err != nil || !digestRE.MatchString(keyFingerprint) || len(encrypted) != len(request.ExplicitKeys) {
		return nil, "", ErrInvalid
	}
	keys := make([]string, 0, len(encrypted))
	for key := range encrypted {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, request.ExplicitKeys) {
		return nil, "", ErrInvalid
	}
	labels := exactManagedLabels(request, "sealed-secret")
	object := map[string]any{
		"apiVersion": "bitnami.com/v1alpha1",
		"kind":       "SealedSecret",
		"metadata": map[string]any{
			"name": request.TargetSecretName, "namespace": request.Binding.Scope.Namespace,
			"labels": labels,
			"annotations": map[string]any{
				contentDigestAnnotation: contentDigest,
				sealingKeyAnnotation:    keyFingerprint,
			},
		},
		"spec": map[string]any{
			"encryptedData": cloneSecretValue(encrypted),
			"template": map[string]any{
				"metadata": map[string]any{
					"name": request.TargetSecretName, "namespace": request.Binding.Scope.Namespace,
					"labels": cloneSecretValue(labels),
				},
				"type": string(request.Version.TargetSecretType), "immutable": true,
			},
		},
	}
	manifestDigest, err := setManifestDigest(object)
	if err != nil {
		clearCiphertextValues(object["spec"].(map[string]any)["encryptedData"].(map[string]any))
		return nil, "", err
	}
	return object, manifestDigest, nil
}

func sealedArtifactFromLive(live map[string]any, target, expectedContentDigest string, expectedType TargetSecretType) (Artifact, error) {
	if live == nil || live["apiVersion"] != "bitnami.com/v1alpha1" || live["kind"] != "SealedSecret" ||
		objectName(live) != target || validateManagedIdentity(live, ProviderSealedSecrets, "sealed-secret", target) != nil ||
		!emptySecretMetadataList(objectMetadata(live)["finalizers"]) || !expectedType.valid() {
		return Artifact{}, ErrProviderMismatch
	}
	annotations, _ := objectMetadata(live)["annotations"].(map[string]any)
	if len(annotations) != 3 || expectedContentDigest != "" && annotations[contentDigestAnnotation] != expectedContentDigest {
		return Artifact{}, ErrProviderMismatch
	}
	keyFingerprint, _ := annotations[sealingKeyAnnotation].(string)
	manifestDigest, _ := annotations[manifestDigestAnnotation].(string)
	if !digestRE.MatchString(keyFingerprint) || !digestRE.MatchString(manifestDigest) {
		return Artifact{}, ErrProviderMismatch
	}
	spec, _ := live["spec"].(map[string]any)
	encrypted, _ := spec["encryptedData"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	templateMetadata, _ := template["metadata"].(map[string]any)
	if len(spec) != 2 || len(template) != 3 || len(templateMetadata) != 3 || template["type"] != string(expectedType) || template["immutable"] != true ||
		templateMetadata["name"] != target || templateMetadata["namespace"] != objectNamespace(live) ||
		!secretCanonicalEqual(templateMetadata["labels"], objectMetadata(live)["labels"]) {
		return Artifact{}, ErrProviderMismatch
	}
	labels, _ := objectMetadata(live)["labels"].(map[string]any)
	if labels == nil || len(encrypted) == 0 || len(encrypted) > MaxMaterialKeys {
		return Artifact{}, ErrProviderMismatch
	}
	for key := range encrypted {
		if !secretKeyRE.MatchString(key) {
			return Artifact{}, ErrProviderMismatch
		}
	}
	if verifyManifestDigest(manifestDigest, live) != nil {
		return Artifact{}, ErrProviderMismatch
	}
	ciphertextDigest, err := ciphertextMapDigest(encrypted)
	if err != nil {
		return Artifact{}, err
	}
	uid, err := objectUID(live)
	if err != nil {
		return Artifact{}, err
	}
	revision, err := providerRevision(uid, providerCoordinateFingerprint(objectNamespace(live), objectName(live)))
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{
		Provider: ProviderSealedSecrets, Namespace: objectNamespace(live), ObjectName: objectName(live), TargetSecretName: target,
		TargetSecretType: expectedType, ProviderRevision: revision, ManifestDigest: manifestDigest,
		SealedKeyFingerprint: keyFingerprint, CiphertextDigest: ciphertextDigest,
	}, nil
}

func sealedSecretKeys(live map[string]any) []string {
	spec, _ := live["spec"].(map[string]any)
	encrypted, _ := spec["encryptedData"].(map[string]any)
	keys := make([]string, 0, len(encrypted))
	for key := range encrypted {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func (a *StrictSealedSecretsAdapter) ObserveStrictSealedSecret(ctx context.Context, artifact Artifact) (ReadinessObservation, error) {
	if a == nil || validateProviderArtifactCoordinates(artifact, ProviderSealedSecrets) != nil {
		return ReadinessObservation{}, ErrProviderMismatch
	}
	parts, err := parseProviderRevision(artifact.ProviderRevision, 2)
	if err != nil {
		return ReadinessObservation{}, err
	}
	if parts[1] != providerCoordinateFingerprint(artifact.Namespace, artifact.ObjectName) {
		return ReadinessObservation{}, ErrProviderMismatch
	}
	live, found, err := liveObject(ctx, a.resources, resourceSealedSecret, artifact.Namespace, artifact.ObjectName)
	if err != nil {
		return ReadinessObservation{}, err
	}
	if !found {
		return ReadinessObservation{}, ErrProviderMismatch
	}
	defer clearSealedObjectCiphertext(live)
	observed, err := sealedArtifactFromLive(live, artifact.TargetSecretName, "", artifact.TargetSecretType)
	if err != nil || observed != artifact {
		return ReadinessObservation{}, ErrProviderMismatch
	}
	uid, _ := objectUID(live)
	if uid != parts[0] {
		return ReadinessObservation{}, ErrProviderMismatch
	}
	status, failure := conditionState(live, "Synced", true)
	if status == ReadinessFailed {
		failure = "sealed-secret-sync-failed"
	}
	return ReadinessObservation{Artifact: artifact, Status: status, FailureCode: failure, ObservedAt: providerNow(a.now)}, nil
}

func (a *StrictSealedSecretsAdapter) DeleteStrictSealedSecret(ctx context.Context, artifact Artifact) (DeleteObservation, error) {
	if a == nil || validateProviderArtifactCoordinates(artifact, ProviderSealedSecrets) != nil {
		return DeleteObservation{}, ErrProviderMismatch
	}
	parts, err := parseProviderRevision(artifact.ProviderRevision, 2)
	if err != nil {
		return DeleteObservation{}, err
	}
	if parts[1] != providerCoordinateFingerprint(artifact.Namespace, artifact.ObjectName) {
		return DeleteObservation{}, ErrProviderMismatch
	}
	live, found, err := liveObject(ctx, a.resources, resourceSealedSecret, artifact.Namespace, artifact.ObjectName)
	if err != nil {
		return DeleteObservation{}, err
	}
	if found {
		defer clearSealedObjectCiphertext(live)
		if objectDeletionInProgress(live) {
			return DeleteObservation{}, ErrProviderOperation
		}
		observed, validateErr := sealedArtifactFromLive(live, artifact.TargetSecretName, "", artifact.TargetSecretType)
		uid, _ := objectUID(live)
		if validateErr != nil || observed != artifact || uid != parts[0] {
			return DeleteObservation{}, ErrProviderMismatch
		}
		if err = deleteExactSecretObject(ctx, a.resources, resourceSealedSecret, live, live); err != nil {
			return DeleteObservation{}, err
		}
		if _, stillPresent, getErr := liveObject(ctx, a.resources, resourceSealedSecret, artifact.Namespace, artifact.ObjectName); getErr != nil || stillPresent {
			return DeleteObservation{}, ErrProviderOperation
		}
	}
	return DeleteObservation{Artifact: artifact, Absent: true, ObservedAt: providerNow(a.now)}, nil
}

func sealStrictValue(random io.Reader, publicKey *rsa.PublicKey, label, plaintext []byte) ([]byte, error) {
	if random == nil || publicKey == nil || len(label) == 0 || len(plaintext) == 0 || len(plaintext) > MaxValueBytes {
		return nil, ErrInvalid
	}
	sessionKey := make([]byte, 32)
	defer clear(sessionKey)
	if _, err := io.ReadFull(random, sessionKey); err != nil {
		return nil, ErrProviderOperation
	}
	encryptedKey, err := rsa.EncryptOAEP(sha256.New(), random, publicKey, sessionKey, label)
	if err != nil || len(encryptedKey) > 0xffff {
		clear(encryptedKey)
		return nil, ErrProviderOperation
	}
	defer clear(encryptedKey)
	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		return nil, ErrProviderOperation
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrProviderOperation
	}
	nonce := make([]byte, aead.NonceSize())
	result := make([]byte, 2+len(encryptedKey))
	binary.BigEndian.PutUint16(result[:2], uint16(len(encryptedKey)))
	copy(result[2:], encryptedKey)
	result = aead.Seal(result, nonce, plaintext, nil)
	clear(nonce)
	return result, nil
}

func clearCiphertextValues(values map[string]any) {
	for key, raw := range values {
		if bytes, ok := raw.([]byte); ok {
			clear(bytes)
		}
		values[key] = nil
		delete(values, key)
	}
}

func clearSealedObjectCiphertext(object map[string]any) {
	spec, _ := object["spec"].(map[string]any)
	values, _ := spec["encryptedData"].(map[string]any)
	clearCiphertextValues(values)
}
