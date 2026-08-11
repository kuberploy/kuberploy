package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func testManagedRuntimeConfig(t *testing.T) RuntimeConfig {
	t.Helper()
	values := validRuntimeEnvironment()
	config, err := RuntimeConfigFromLookup(func(name string) (string, bool) { value, ok := values[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestRuntimeTargetBindingRejectsExternalAndMutation(t *testing.T) {
	config := testManagedRuntimeConfig(t)
	target, err := config.ManagedTarget()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateTarget(target); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*domain.RegistryTarget){
		func(value *domain.RegistryTarget) { value.Mode = domain.RegistryTargetExternal },
		func(value *domain.RegistryTarget) { value.Name = "renamed" },
		func(value *domain.RegistryTarget) { value.Endpoint = "https://attacker.invalid" },
		func(value *domain.RegistryTarget) { value.RepositoryPrefix = "other" },
		func(value *domain.RegistryTarget) { value.PushCredentialRef = config.CredentialRef },
		func(value *domain.RegistryTarget) { value.PullCredentialRef = config.CredentialRef },
		func(value *domain.RegistryTarget) { value.CacheCredentialRef = config.CredentialRef },
	}
	for index, mutate := range mutations {
		changed := target
		mutate(&changed)
		if err := config.ValidateTarget(changed); !errors.Is(err, ErrDistributionScopeMismatch) {
			t.Fatalf("mutation %d accepted: %v", index, err)
		}
	}
	rotatedBuildCredential := target
	rotatedBuildCredential.PushCredentialRef = "rotated-builder-push-secret"
	if err := config.ValidateTarget(rotatedBuildCredential); !errors.Is(err, ErrDistributionScopeMismatch) {
		t.Fatalf("operator-owned build credential mutation was accepted: %v", err)
	}
}

func TestManagedRegistryBackingObjectsAreExactAndImmutable(t *testing.T) {
	config := testManagedRuntimeConfig(t)
	labels := map[string]any{"app.kubernetes.io/name": "kuberploy-registry", "app.kubernetes.io/instance": config.Deployment,
		"app.kubernetes.io/component": "registry", "app.kubernetes.io/managed-by": "Helm"}
	pvc := map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": config.PersistentVolumeClaim, "namespace": config.Namespace, "uid": "pvc-uid", "resourceVersion": "7", "labels": labels},
		"spec":     map[string]any{"accessModes": []any{"ReadWriteOnce"}, "volumeName": "pv-managed", "volumeMode": "Filesystem"},
		"status":   map[string]any{"phase": "Bound"}}
	if err := validateManagedRegistryPVC(pvc, config); err != nil {
		t.Fatal(err)
	}
	mutatedPVC := cloneRegistryTestJSON(t, pvc)
	mutatedPVC["spec"].(map[string]any)["volumeName"] = ""
	if err := validateManagedRegistryPVC(mutatedPVC, config); !errors.Is(err, ErrRegistryMaintenanceInvalid) {
		t.Fatalf("unbound PVC accepted: %v", err)
	}
	configuration := `version: 0.1
log:
  level: info
  formatter: json
storage:
  filesystem:
    rootdirectory: /var/lib/registry
  delete:
    enabled: true
  cache:
    blobdescriptor: inmemory
  maintenance:
    uploadpurging:
      enabled: true
      age: 168h
      interval: 24h
      dryrun: false
http:
  addr: :5000
  draintimeout: 60s
  headers:
    X-Content-Type-Options: [nosniff]
health:
  storagedriver:
    enabled: true
    interval: 10s
    threshold: 3
`
	configMap := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "immutable": true,
		"metadata": map[string]any{"name": config.RegistryConfigMap, "namespace": config.Namespace, "uid": "config-uid", "resourceVersion": "8", "labels": labels},
		"data":     map[string]any{"config.yml": configuration}}
	if err := validateManagedRegistryConfigMap(configMap, config); err != nil {
		t.Fatal(err)
	}
	tlsConfig := config
	tlsConfig.AllowPlainHTTP = false
	tlsConfiguration := string(bytes.ReplaceAll([]byte(configuration), []byte("  draintimeout: 60s\n"), []byte("  draintimeout: 60s\n  tls:\n    certificate: /tls/tls.crt\n    key: /tls/tls.key\n")))
	tlsConfigMap := cloneRegistryTestJSON(t, configMap)
	tlsConfigMap["data"].(map[string]any)["config.yml"] = tlsConfiguration
	if err := validateManagedRegistryConfigMap(tlsConfigMap, tlsConfig); err != nil {
		t.Fatalf("exact backend TLS config rejected: %v", err)
	}
	if err := validateManagedRegistryConfigMap(configMap, tlsConfig); !errors.Is(err, ErrRegistryMaintenanceInvalid) {
		t.Fatalf("plaintext backend accepted for HTTPS runtime: %v", err)
	}
	changedTLS := cloneRegistryTestJSON(t, tlsConfigMap)
	changedTLS["data"].(map[string]any)["config.yml"] = strings.ReplaceAll(tlsConfiguration, "/tls/tls.key", "/tmp/tls.key")
	if err := validateManagedRegistryConfigMap(changedTLS, tlsConfig); !errors.Is(err, ErrRegistryMaintenanceInvalid) {
		t.Fatalf("substituted backend TLS key accepted: %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"mutable":   func(value map[string]any) { value["immutable"] = false },
		"extra-key": func(value map[string]any) { value["data"].(map[string]any)["other"] = "value" },
		"wrong-root": func(value map[string]any) {
			value["data"].(map[string]any)["config.yml"] = string(bytes.ReplaceAll([]byte(configuration), []byte("/var/lib/registry"), []byte("/tmp/other")))
		},
	} {
		changed := cloneRegistryTestJSON(t, configMap)
		mutate(changed)
		if err := validateManagedRegistryConfigMap(changed, config); !errors.Is(err, ErrRegistryMaintenanceInvalid) {
			t.Fatalf("%s ConfigMap mutation accepted: %v", name, err)
		}
	}
}

func TestMaintenanceJobAdoptionRejectsMutation(t *testing.T) {
	config := testManagedRuntimeConfig(t)
	digests := []string{"sha256:" + repeatHex("1", 64), "sha256:" + repeatHex("2", 64)}
	candidateDigest, ordered, err := cleanupCandidateSetDigest(digests)
	if err != nil {
		t.Fatal(err)
	}
	request := maintenanceHelperRequest{Version: 1, Mode: "checkpoint", TargetID: config.TargetID,
		PlanID: "11111111-2222-4333-8444-555555555555", PlanDigest: "sha256:" + repeatHex("3", 64),
		ExecutionKey: "sha256:" + repeatHex("4", 64), CandidateSetDigest: candidateDigest,
		CandidateDigests: ordered, NotBefore: time.Now().UTC().Add(-time.Minute)}
	expected, inputDigest, err := registryMaintenanceJob(config, request)
	if err != nil {
		t.Fatal(err)
	}
	actual := cloneRegistryTestJSON(t, expected)
	metadata := actual["metadata"].(map[string]any)
	metadata["uid"], metadata["resourceVersion"] = "job-uid", "12"
	if err = validateRegistryMaintenanceJob(actual, expected, config, inputDigest); err != nil {
		t.Fatal(err)
	}
	mutations := []func(map[string]any){
		func(value map[string]any) {
			spec, _ := nestedMap(value, "spec", "template", "spec")
			spec["serviceAccountName"] = "attacker"
		},
		func(value map[string]any) {
			spec, _ := nestedMap(value, "spec", "template", "spec")
			container := spec["containers"].([]any)[0].(map[string]any)
			container["image"] = "attacker.invalid/image@sha256:" + repeatHex("f", 64)
		},
		func(value map[string]any) {
			spec, _ := nestedMap(value, "spec", "template", "spec")
			container := spec["containers"].([]any)[0].(map[string]any)
			container["securityContext"].(map[string]any)["allowPrivilegeEscalation"] = true
		},
	}
	for index, mutate := range mutations {
		changed := cloneRegistryTestJSON(t, actual)
		mutate(changed)
		if err = validateRegistryMaintenanceJob(changed, expected, config, inputDigest); !errors.Is(err, ErrRegistryMaintenanceInvalid) {
			t.Fatalf("job mutation %d accepted: %v", index, err)
		}
	}
}

func TestMaximumMaintenanceResultFitsTerminationMessage(t *testing.T) {
	digests := make([]string, maximumMaintenanceCandidates)
	rows := make([]RegistryBlobReachability, maximumMaintenanceCandidates)
	for index := range digests {
		digests[index] = "sha256:" + repeatHex(string("0123456789abcdef"[index]), 64)
		rows[index] = RegistryBlobReachability{Digest: digests[index], Present: true, Reachable: true}
	}
	candidateDigest, ordered, err := cleanupCandidateSetDigest(digests)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	result := maintenanceHelperResult{Version: 1, Mode: "checkpoint", Checkpoint: &physicalReachabilityCheckpoint{
		TargetID: "11111111-1111-4111-8111-111111111111", PlanID: "22222222-2222-4222-8222-222222222222",
		PlanDigest: "sha256:" + repeatHex("a", 64), ExecutionKey: "sha256:" + repeatHex("b", 64), CandidateSetDigest: candidateDigest,
		Revision: "physical-" + repeatHex("c", 24), InventoryRevision: "inventory-" + repeatHex("d", 24),
		RegistryWide: true, InventoryComplete: true, ReachabilityComplete: true, StartedAt: now, ObservedAt: now, Blobs: rows}}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != maximumMaintenanceCandidates || len(encoded) > maximumMaintenanceResultBytes {
		t.Fatalf("maximum result bytes=%d limit=%d", len(encoded), maximumMaintenanceResultBytes)
	}
	if _, err = decodeMaintenanceHelperResult(string(encoded), "checkpoint"); err != nil {
		t.Fatalf("maximum valid termination result rejected: %v", err)
	}
	for name, message := range map[string]string{
		"trailing":  string(encoded) + `{}`,
		"truncated": string(encoded[:len(encoded)-1]),
		"oversized": string(encoded) + repeatHex("x", maximumMaintenanceResultBytes),
	} {
		if _, err = decodeMaintenanceHelperResult(message, "checkpoint"); !errors.Is(err, ErrRegistryMaintenanceInvalid) {
			t.Fatalf("%s termination message accepted: %v", name, err)
		}
	}
}

func cloneRegistryTestJSON(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var cloned map[string]any
	if err = decoder.Decode(&cloned); err != nil {
		t.Fatal(err)
	}
	return normalizeRegistryKubernetesJSON(cloned).(map[string]any)
}
