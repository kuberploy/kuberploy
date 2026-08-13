package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/store"
	"go.yaml.in/yaml/v3"
)

const (
	registryServiceAccountDirectory    = "/var/run/secrets/kubernetes.io/serviceaccount"
	maximumRegistryKubernetesBodyBytes = 4 << 20
)

var (
	errRegistryKubernetesNotFound = errors.New("managed registry Kubernetes object not found")
	errRegistryKubernetesConflict = errors.New("managed registry Kubernetes object conflict")
	registryMaintenanceJobNameRE  = regexp.MustCompile(`^kuberploy-registry-(?:checkpoint|gc)-[0-9a-f]{20}$`)
)

type registryKubernetesClient struct {
	baseURL   string
	http      *http.Client
	tokenPath string
	runtime   RuntimeConfig
	timeout   time.Duration
	now       func() time.Time
}

func NewInClusterRegistryMaintenanceWorkloads(runtime RuntimeConfig, timeout time.Duration) (RegistryMaintenanceWorkloads, error) {
	if runtime.Validate() != nil || !runtime.Enabled || timeout < time.Minute || timeout > 2*time.Hour {
		return nil, ErrRegistryMaintenanceInvalid
	}
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes in-cluster service environment is missing")
	}
	caPEM, err := os.ReadFile(registryServiceAccountDirectory + "/ca.crt")
	if err != nil {
		return nil, errors.New("read Kubernetes service CA")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("Kubernetes service CA is invalid")
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Kubernetes redirect denied") }}
	return &registryKubernetesClient{baseURL: "https://" + net.JoinHostPort(host, port), http: client,
		tokenPath: registryServiceAccountDirectory + "/token", runtime: runtime, timeout: timeout,
		now: func() time.Time { return time.Now().UTC() }}, nil
}

func (c *registryKubernetesClient) Inspect(ctx context.Context, runtime RuntimeConfig) (ManagedRegistryStopProof, error) {
	if c == nil || runtime != c.runtime {
		return ManagedRegistryStopProof{}, ErrRegistryMaintenanceInvalid
	}
	deployment, err := c.get(ctx, registryDeploymentPath(runtime), nil)
	if err != nil {
		return ManagedRegistryStopProof{}, err
	}
	identity, err := validateManagedRegistryDeployment(deployment, runtime)
	if err != nil || identity.replicas < 1 {
		return ManagedRegistryStopProof{}, ErrRegistryMaintenanceInvalid
	}
	if err = c.validateBackingObjects(ctx, runtime); err != nil {
		return ManagedRegistryStopProof{}, err
	}
	return ManagedRegistryStopProof{Namespace: runtime.Namespace, Deployment: runtime.Deployment,
		DeploymentUID: identity.uid, OriginalReplicas: identity.replicas,
		PersistentVolumeClaim: runtime.PersistentVolumeClaim, RegistryConfigMap: runtime.RegistryConfigMap,
		Stopped: false, NoSelectedPods: false, ObservedAt: c.now()}, nil
}

func (c *registryKubernetesClient) Stop(ctx context.Context, runtime RuntimeConfig, lease store.RegistryMaintenanceLease) (ManagedRegistryStopProof, error) {
	if c == nil || runtime != c.runtime || lease.TargetID != runtime.TargetID || lease.DeploymentUID == "" || lease.OriginalReplicas < 1 {
		return ManagedRegistryStopProof{}, ErrRegistryMaintenanceInvalid
	}
	if err := c.ensureScale(ctx, runtime, lease.DeploymentUID, 0); err != nil {
		return ManagedRegistryStopProof{}, err
	}
	return c.waitStopped(ctx, runtime, lease)
}

func (c *registryKubernetesClient) VerifyStopped(ctx context.Context, runtime RuntimeConfig, lease store.RegistryMaintenanceLease) (ManagedRegistryStopProof, error) {
	if c == nil || runtime != c.runtime || lease.DeploymentUID == "" || lease.OriginalReplicas < 1 {
		return ManagedRegistryStopProof{}, ErrRegistryMaintenanceInvalid
	}
	return c.waitStopped(ctx, runtime, lease)
}

func (c *registryKubernetesClient) waitStopped(ctx context.Context, runtime RuntimeConfig, lease store.RegistryMaintenanceLease) (ManagedRegistryStopProof, error) {
	deadline := c.now().Add(c.timeout)
	for {
		deployment, err := c.get(ctx, registryDeploymentPath(runtime), nil)
		if err != nil {
			return ManagedRegistryStopProof{}, err
		}
		identity, err := validateManagedRegistryDeployment(deployment, runtime)
		if err != nil || identity.uid != lease.DeploymentUID {
			return ManagedRegistryStopProof{}, ErrRegistryMaintenanceInvalid
		}
		if err = c.validateBackingObjects(ctx, runtime); err != nil {
			return ManagedRegistryStopProof{}, err
		}
		pods, err := c.listRegistryPods(ctx, runtime)
		if err != nil {
			return ManagedRegistryStopProof{}, err
		}
		if identity.replicas == 0 && identity.statusReplicas == 0 && identity.availableReplicas == 0 && len(pods) == 0 {
			return ManagedRegistryStopProof{Namespace: runtime.Namespace, Deployment: runtime.Deployment,
				DeploymentUID: identity.uid, OriginalReplicas: lease.OriginalReplicas,
				PersistentVolumeClaim: runtime.PersistentVolumeClaim, RegistryConfigMap: runtime.RegistryConfigMap,
				Stopped: true, NoSelectedPods: true, ObservedAt: c.now()}, nil
		}
		if !c.now().Before(deadline) {
			return ManagedRegistryStopProof{}, ErrRegistryMaintenanceUnavailable
		}
		if err = waitRegistryPoll(ctx); err != nil {
			return ManagedRegistryStopProof{}, err
		}
	}
}

func (c *registryKubernetesClient) Restore(ctx context.Context, runtime RuntimeConfig, lease store.RegistryMaintenanceLease) (ManagedRegistryRestoreProof, error) {
	if c == nil || runtime != c.runtime || lease.DeploymentUID == "" || lease.OriginalReplicas < 1 {
		return ManagedRegistryRestoreProof{}, ErrRegistryMaintenanceInvalid
	}
	if err := c.ensureScale(ctx, runtime, lease.DeploymentUID, lease.OriginalReplicas); err != nil {
		return ManagedRegistryRestoreProof{}, err
	}
	deadline := c.now().Add(c.timeout)
	for {
		deployment, err := c.get(ctx, registryDeploymentPath(runtime), nil)
		if err != nil {
			return ManagedRegistryRestoreProof{}, err
		}
		identity, err := validateManagedRegistryDeployment(deployment, runtime)
		if err != nil || identity.uid != lease.DeploymentUID {
			return ManagedRegistryRestoreProof{}, ErrRegistryMaintenanceInvalid
		}
		if err = c.validateBackingObjects(ctx, runtime); err != nil {
			return ManagedRegistryRestoreProof{}, err
		}
		if identity.replicas == lease.OriginalReplicas && identity.availableReplicas == lease.OriginalReplicas &&
			identity.readyReplicas == lease.OriginalReplicas && identity.observedGeneration >= identity.generation {
			return ManagedRegistryRestoreProof{Namespace: runtime.Namespace, Deployment: runtime.Deployment,
				DeploymentUID: identity.uid, DesiredReplicas: lease.OriginalReplicas, AvailableReplicas: identity.availableReplicas,
				Ready: true, ObservedAt: c.now()}, nil
		}
		if !c.now().Before(deadline) {
			return ManagedRegistryRestoreProof{}, ErrRegistryMaintenanceUnavailable
		}
		if err = waitRegistryPoll(ctx); err != nil {
			return ManagedRegistryRestoreProof{}, err
		}
	}
}

type managedDeploymentIdentity struct {
	uid, resourceVersion                                       string
	replicas, statusReplicas, availableReplicas, readyReplicas int32
	generation, observedGeneration                             int64
}

func validateManagedRegistryDeployment(object map[string]any, runtime RuntimeConfig) (managedDeploymentIdentity, error) {
	if object["apiVersion"] != "apps/v1" || object["kind"] != "Deployment" {
		return managedDeploymentIdentity{}, ErrRegistryMaintenanceInvalid
	}
	metadata, ok := object["metadata"].(map[string]any)
	if !ok || metadata["name"] != runtime.Deployment || metadata["namespace"] != runtime.Namespace {
		return managedDeploymentIdentity{}, ErrRegistryMaintenanceInvalid
	}
	uid, _ := metadata["uid"].(string)
	rv, _ := metadata["resourceVersion"].(string)
	labels, _ := metadata["labels"].(map[string]any)
	if uid == "" || rv == "" || labels["app.kubernetes.io/name"] != "kuberploy-registry" ||
		labels["app.kubernetes.io/instance"] != runtime.Deployment || labels["app.kubernetes.io/component"] != "registry" ||
		labels["app.kubernetes.io/managed-by"] != "Helm" {
		return managedDeploymentIdentity{}, ErrRegistryMaintenanceInvalid
	}
	spec, ok := object["spec"].(map[string]any)
	if !ok || nestedString(spec, "strategy", "type") != "Recreate" {
		return managedDeploymentIdentity{}, ErrRegistryMaintenanceInvalid
	}
	replicas, ok := jsonInt32(spec["replicas"])
	if !ok || replicas < 0 {
		return managedDeploymentIdentity{}, ErrRegistryMaintenanceInvalid
	}
	templateSpec, ok := nestedMap(spec, "template", "spec")
	if !ok || templateSpec["automountServiceAccountToken"] != false {
		return managedDeploymentIdentity{}, ErrRegistryMaintenanceInvalid
	}
	containers, ok := templateSpec["containers"].([]any)
	if !ok || len(containers) != 1 {
		return managedDeploymentIdentity{}, ErrRegistryMaintenanceInvalid
	}
	container, ok := containers[0].(map[string]any)
	if !ok || container["name"] != "registry" || !containerHasMount(container, "data", managedRegistryStorageRoot) || !containerHasMount(container, "config", managedRegistryConfigPath) {
		return managedDeploymentIdentity{}, ErrRegistryMaintenanceInvalid
	}
	volumes, ok := templateSpec["volumes"].([]any)
	if !ok || !volumeBinds(volumes, "data", "persistentVolumeClaim", "claimName", runtime.PersistentVolumeClaim) ||
		!volumeBinds(volumes, "config", "configMap", "name", runtime.RegistryConfigMap) {
		return managedDeploymentIdentity{}, ErrRegistryMaintenanceInvalid
	}
	if !runtime.AllowPlainHTTP && (!containerHasReadOnlyMount(container, "tls", "/tls") || !volumeHasExactTLSSecret(volumes, "tls")) {
		return managedDeploymentIdentity{}, ErrRegistryMaintenanceInvalid
	}
	identity := managedDeploymentIdentity{uid: uid, resourceVersion: rv, replicas: replicas}
	identity.generation, _ = jsonInt64(metadata["generation"])
	if status, statusOK := object["status"].(map[string]any); statusOK {
		identity.statusReplicas, _ = jsonInt32(status["replicas"])
		identity.availableReplicas, _ = jsonInt32(status["availableReplicas"])
		identity.readyReplicas, _ = jsonInt32(status["readyReplicas"])
		identity.observedGeneration, _ = jsonInt64(status["observedGeneration"])
	}
	return identity, nil
}

func nestedMap(root map[string]any, keys ...string) (map[string]any, bool) {
	current := root
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

func nestedString(root map[string]any, keys ...string) string {
	if len(keys) == 0 {
		return ""
	}
	parent, ok := nestedMap(root, keys[:len(keys)-1]...)
	if !ok {
		return ""
	}
	value, _ := parent[keys[len(keys)-1]].(string)
	return value
}

func containerHasMount(container map[string]any, name, path string) bool {
	mounts, _ := container["volumeMounts"].([]any)
	for _, raw := range mounts {
		mount, _ := raw.(map[string]any)
		if mount["name"] == name && mount["mountPath"] == path {
			return true
		}
	}
	return false
}

func containerHasReadOnlyMount(container map[string]any, name, path string) bool {
	mounts, _ := container["volumeMounts"].([]any)
	for _, raw := range mounts {
		mount, _ := raw.(map[string]any)
		if mount["name"] == name && mount["mountPath"] == path && mount["readOnly"] == true {
			return true
		}
	}
	return false
}

func volumeHasExactTLSSecret(volumes []any, name string) bool {
	for _, raw := range volumes {
		volume, _ := raw.(map[string]any)
		secret, _ := volume["secret"].(map[string]any)
		secretName, _ := secret["secretName"].(string)
		mode, modeOK := jsonInt64(secret["defaultMode"])
		items, _ := secret["items"].([]any)
		if volume["name"] != name || secretName == "" || !modeOK || mode != 288 || len(items) != 2 {
			continue
		}
		seen := map[string]bool{}
		for _, itemRaw := range items {
			item, _ := itemRaw.(map[string]any)
			key, _ := item["key"].(string)
			path, _ := item["path"].(string)
			if (key != "tls.crt" && key != "tls.key") || key != path || seen[key] {
				return false
			}
			seen[key] = true
		}
		return seen["tls.crt"] && seen["tls.key"]
	}
	return false
}

func volumeBinds(volumes []any, name, kind, key, value string) bool {
	for _, raw := range volumes {
		volume, _ := raw.(map[string]any)
		binding, _ := volume[kind].(map[string]any)
		if volume["name"] == name && binding[key] == value {
			return true
		}
	}
	return false
}

func (c *registryKubernetesClient) validateBackingObjects(ctx context.Context, runtime RuntimeConfig) error {
	pvc, err := c.get(ctx, registryPVCPath(runtime), nil)
	if err != nil || validateManagedRegistryPVC(pvc, runtime) != nil {
		return ErrRegistryMaintenanceInvalid
	}
	configMap, err := c.get(ctx, registryConfigMapPath(runtime), nil)
	if err != nil || validateManagedRegistryConfigMap(configMap, runtime) != nil {
		return ErrRegistryMaintenanceInvalid
	}
	return nil
}

func validateManagedRegistryPVC(object map[string]any, runtime RuntimeConfig) error {
	if object["apiVersion"] != "v1" || object["kind"] != "PersistentVolumeClaim" {
		return ErrRegistryMaintenanceInvalid
	}
	metadata, _ := object["metadata"].(map[string]any)
	labels, _ := metadata["labels"].(map[string]any)
	uid, _ := metadata["uid"].(string)
	rv, _ := metadata["resourceVersion"].(string)
	if metadata["name"] != runtime.PersistentVolumeClaim || metadata["namespace"] != runtime.Namespace || uid == "" || rv == "" ||
		labels["app.kubernetes.io/name"] != "kuberploy-registry" || labels["app.kubernetes.io/instance"] != runtime.Deployment ||
		labels["app.kubernetes.io/component"] != "registry" || labels["app.kubernetes.io/managed-by"] != "Helm" {
		return ErrRegistryMaintenanceInvalid
	}
	spec, _ := object["spec"].(map[string]any)
	modes, _ := spec["accessModes"].([]any)
	volumeName, _ := spec["volumeName"].(string)
	volumeMode, _ := spec["volumeMode"].(string)
	if len(modes) != 1 || (modes[0] != "ReadWriteOnce" && modes[0] != "ReadWriteMany") || volumeName == "" ||
		(volumeMode != "" && volumeMode != "Filesystem") || nestedString(object, "status", "phase") != "Bound" {
		return ErrRegistryMaintenanceInvalid
	}
	return nil
}

type managedRegistryConfiguration struct {
	Version any `yaml:"version"`
	Storage struct {
		Filesystem struct {
			RootDirectory string `yaml:"rootdirectory"`
		} `yaml:"filesystem"`
		Delete struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"delete"`
		Cache struct {
			BlobDescriptor string `yaml:"blobdescriptor"`
		} `yaml:"cache"`
		Maintenance struct {
			UploadPurging struct {
				Enabled  bool   `yaml:"enabled"`
				Age      string `yaml:"age"`
				Interval string `yaml:"interval"`
				DryRun   bool   `yaml:"dryrun"`
			} `yaml:"uploadpurging"`
		} `yaml:"maintenance"`
	} `yaml:"storage"`
	Log struct {
		Level     string `yaml:"level"`
		Formatter string `yaml:"formatter"`
	} `yaml:"log"`
	HTTP struct {
		Address      string `yaml:"addr"`
		DrainTimeout string `yaml:"draintimeout"`
		TLS          *struct {
			Certificate string `yaml:"certificate"`
			Key         string `yaml:"key"`
		} `yaml:"tls,omitempty"`
		Headers map[string][]string `yaml:"headers"`
	} `yaml:"http"`
	Health struct {
		StorageDriver struct {
			Enabled   bool   `yaml:"enabled"`
			Interval  string `yaml:"interval"`
			Threshold int    `yaml:"threshold"`
		} `yaml:"storagedriver"`
	} `yaml:"health"`
	Auth *struct {
		HTPasswd struct {
			Realm string `yaml:"realm"`
			Path  string `yaml:"path"`
		} `yaml:"htpasswd"`
	} `yaml:"auth,omitempty"`
}

func validateManagedRegistryConfigMap(object map[string]any, runtime RuntimeConfig) error {
	if object["apiVersion"] != "v1" || object["kind"] != "ConfigMap" || object["immutable"] != true {
		return ErrRegistryMaintenanceInvalid
	}
	metadata, _ := object["metadata"].(map[string]any)
	labels, _ := metadata["labels"].(map[string]any)
	uid, _ := metadata["uid"].(string)
	rv, _ := metadata["resourceVersion"].(string)
	if metadata["name"] != runtime.RegistryConfigMap || metadata["namespace"] != runtime.Namespace || uid == "" || rv == "" ||
		labels["app.kubernetes.io/name"] != "kuberploy-registry" || labels["app.kubernetes.io/instance"] != runtime.Deployment ||
		labels["app.kubernetes.io/component"] != "registry" || labels["app.kubernetes.io/managed-by"] != "Helm" {
		return ErrRegistryMaintenanceInvalid
	}
	data, _ := object["data"].(map[string]any)
	encoded, _ := data["config.yml"].(string)
	if len(data) != 1 || encoded == "" || len(encoded) > 64<<10 {
		return ErrRegistryMaintenanceInvalid
	}
	var config managedRegistryConfiguration
	decoder := yaml.NewDecoder(strings.NewReader(encoded))
	decoder.KnownFields(true)
	if decoder.Decode(&config) != nil {
		return ErrRegistryMaintenanceInvalid
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || fmt.Sprint(config.Version) != "0.1" ||
		config.Storage.Filesystem.RootDirectory != managedRegistryStorageRoot || !config.Storage.Delete.Enabled ||
		config.Storage.Cache.BlobDescriptor != "inmemory" || !config.Storage.Maintenance.UploadPurging.Enabled ||
		config.Storage.Maintenance.UploadPurging.DryRun || config.HTTP.Address == "" || !config.Health.StorageDriver.Enabled {
		return ErrRegistryMaintenanceInvalid
	}
	if !runtime.AllowPlainHTTP && (config.HTTP.TLS == nil || config.HTTP.TLS.Certificate != "/tls/tls.crt" || config.HTTP.TLS.Key != "/tls/tls.key") {
		return ErrRegistryMaintenanceInvalid
	}
	return nil
}

func (c *registryKubernetesClient) ensureScale(ctx context.Context, runtime RuntimeConfig, uid string, replicas int32) error {
	deployment, err := c.get(ctx, registryDeploymentPath(runtime), nil)
	if err != nil {
		return err
	}
	identity, err := validateManagedRegistryDeployment(deployment, runtime)
	if err != nil || identity.uid != uid {
		return ErrRegistryMaintenanceInvalid
	}
	if identity.replicas == replicas {
		return nil
	}
	body := map[string]any{"apiVersion": "autoscaling/v1", "kind": "Scale",
		"metadata": map[string]any{"name": runtime.Deployment, "namespace": runtime.Namespace, "resourceVersion": identity.resourceVersion},
		"spec":     map[string]any{"replicas": replicas}}
	response, err := c.put(ctx, registryDeploymentPath(runtime)+"/scale", body)
	if err != nil {
		return err
	}
	if validateScaleMutationResponse(response, runtime, uid) != nil {
		return ErrRegistryMaintenanceInvalid
	}
	verification, err := c.get(ctx, registryDeploymentPath(runtime), nil)
	if err != nil {
		return err
	}
	verified, err := validateManagedRegistryDeployment(verification, runtime)
	if err != nil || verified.uid != uid || verified.replicas != replicas {
		return ErrRegistryMaintenanceInvalid
	}
	return nil
}

func validateScaleMutationResponse(response map[string]any, runtime RuntimeConfig, uid string) error {
	metadata, _ := response["metadata"].(map[string]any)
	responseUID, _ := metadata["uid"].(string)
	resourceVersion, _ := metadata["resourceVersion"].(string)
	if response["apiVersion"] != "autoscaling/v1" || response["kind"] != "Scale" ||
		metadata["name"] != runtime.Deployment || metadata["namespace"] != runtime.Namespace ||
		uid == "" || responseUID != uid || resourceVersion == "" {
		return ErrRegistryMaintenanceInvalid
	}
	// Kubernetes servers may omit Scale.spec from an update response even
	// after applying the requested replica count. The authoritative Deployment
	// GET below proves the exact UID and desired replicas before we proceed.
	return nil
}

func (c *registryKubernetesClient) listRegistryPods(ctx context.Context, runtime RuntimeConfig) ([]map[string]any, error) {
	query := url.Values{"labelSelector": {"app.kubernetes.io/name=kuberploy-registry,app.kubernetes.io/instance=" + runtime.Deployment + ",app.kubernetes.io/component=registry"}, "limit": {"2"}}
	object, err := c.get(ctx, "/api/v1/namespaces/"+url.PathEscape(runtime.Namespace)+"/pods", query)
	if err != nil {
		return nil, err
	}
	if object["apiVersion"] != "v1" || object["kind"] != "PodList" || nestedString(object, "metadata", "continue") != "" {
		return nil, ErrRegistryMaintenanceInvalid
	}
	items, ok := object["items"].([]any)
	if !ok || len(items) > 1 {
		return nil, ErrRegistryMaintenanceInvalid
	}
	result := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		pod, ok := raw.(map[string]any)
		if !ok {
			return nil, ErrRegistryMaintenanceInvalid
		}
		result = append(result, pod)
	}
	return result, nil
}

func waitRegistryPoll(ctx context.Context) error {
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *registryKubernetesClient) Checkpoint(ctx context.Context, runtime RuntimeConfig, request maintenanceHelperRequest) (physicalReachabilityCheckpoint, RegistryMaintenanceJobEvidence, error) {
	if runtime != c.runtime || request.validate("checkpoint") != nil {
		return physicalReachabilityCheckpoint{}, RegistryMaintenanceJobEvidence{}, ErrRegistryCheckpointIncomplete
	}
	result, evidence, err := c.runJob(ctx, runtime, request, true)
	if err != nil || result.Checkpoint == nil || result.Sweep != nil || result.Checkpoint.StartedAt.Before(evidence.StartedAt.Add(-time.Second)) || result.Checkpoint.ObservedAt.After(evidence.CompletedAt.Add(time.Second)) {
		return physicalReachabilityCheckpoint{}, RegistryMaintenanceJobEvidence{}, ErrRegistryCheckpointIncomplete
	}
	return *result.Checkpoint, evidence, nil
}

func (c *registryKubernetesClient) Sweep(ctx context.Context, runtime RuntimeConfig, request maintenanceHelperRequest) (GCSweepResult, RegistryMaintenanceJobEvidence, error) {
	if runtime != c.runtime || request.validate("gc") != nil {
		return GCSweepResult{}, RegistryMaintenanceJobEvidence{}, ErrRegistryGCSweepUnconfirmed
	}
	result, evidence, err := c.runJob(ctx, runtime, request, false)
	if err != nil || result.Sweep == nil || result.Checkpoint != nil || result.Sweep.StartedAt.Before(evidence.StartedAt.Add(-time.Second)) || result.Sweep.CompletedAt.After(evidence.CompletedAt.Add(time.Second)) {
		return GCSweepResult{}, RegistryMaintenanceJobEvidence{}, ErrRegistryGCSweepUnconfirmed
	}
	return *result.Sweep, evidence, nil
}

func (c *registryKubernetesClient) RecoverSweep(ctx context.Context, runtime RuntimeConfig, targetID, planID, executionKey, candidateSetDigest string) (maintenanceHelperRequest, GCSweepResult, RegistryMaintenanceJobEvidence, bool, error) {
	if runtime != c.runtime || targetID != runtime.TargetID || !validSafeIdentity(planID) || !validDigest(executionKey) || !validDigest(candidateSetDigest) {
		return maintenanceHelperRequest{}, GCSweepResult{}, RegistryMaintenanceJobEvidence{}, false, ErrRegistryGCSweepUnconfirmed
	}
	name := maintenanceJobName("gc", executionKey)
	job, err := c.get(ctx, registryJobPath(runtime, name), nil)
	if errors.Is(err, errRegistryKubernetesNotFound) {
		return maintenanceHelperRequest{}, GCSweepResult{}, RegistryMaintenanceJobEvidence{}, false, nil
	}
	if err != nil {
		return maintenanceHelperRequest{}, GCSweepResult{}, RegistryMaintenanceJobEvidence{}, false, err
	}
	request, err := maintenanceRequestFromJob(job, runtime, "gc")
	if err != nil || request.TargetID != targetID || request.PlanID != planID || request.ExecutionKey != executionKey || request.CandidateSetDigest != candidateSetDigest {
		return maintenanceHelperRequest{}, GCSweepResult{}, RegistryMaintenanceJobEvidence{}, false, ErrRegistryGCSweepUnconfirmed
	}
	result, evidence, err := c.waitJob(ctx, runtime, request, job)
	if err != nil || result.Sweep == nil || result.Sweep.StartedAt.Before(evidence.StartedAt.Add(-time.Second)) || result.Sweep.CompletedAt.After(evidence.CompletedAt.Add(time.Second)) {
		return maintenanceHelperRequest{}, GCSweepResult{}, RegistryMaintenanceJobEvidence{}, false, ErrRegistryGCSweepUnconfirmed
	}
	return request, *result.Sweep, evidence, true, nil
}

func (c *registryKubernetesClient) runJob(ctx context.Context, runtime RuntimeConfig, request maintenanceHelperRequest, replaceStale bool) (maintenanceHelperResult, RegistryMaintenanceJobEvidence, error) {
	if err := c.validateBackingObjects(ctx, runtime); err != nil {
		return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, err
	}
	object, inputDigest, err := registryMaintenanceJob(runtime, request)
	if err != nil {
		return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, err
	}
	name := maintenanceJobName(request.Mode, request.ExecutionKey)
	job, err := c.get(ctx, registryJobPath(runtime, name), nil)
	if errors.Is(err, errRegistryKubernetesNotFound) {
		job, err = c.create(ctx, registryJobsPath(runtime), object)
	} else if err == nil {
		if validateRegistryMaintenanceJob(job, object, runtime, inputDigest) != nil {
			if !replaceStale {
				return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, ErrRegistryGCSweepUnconfirmed
			}
			if deleteErr := c.deleteJobObject(ctx, runtime, job); deleteErr != nil {
				return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, deleteErr
			}
			job, err = c.create(ctx, registryJobsPath(runtime), object)
		}
	}
	if err != nil {
		return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, err
	}
	if validateRegistryMaintenanceJob(job, object, runtime, inputDigest) != nil {
		return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, ErrRegistryMaintenanceInvalid
	}
	return c.waitJob(ctx, runtime, request, job)
}

func registryMaintenanceJob(runtime RuntimeConfig, request maintenanceHelperRequest) (map[string]any, string, error) {
	if runtime.Validate() != nil || request.validate(request.Mode) != nil {
		return nil, "", ErrRegistryMaintenanceInvalid
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > maximumMaintenanceResultBytes {
		return nil, "", ErrRegistryMaintenanceInvalid
	}
	sum := sha256.Sum256(encoded)
	inputDigest := "sha256:" + hex.EncodeToString(sum[:])
	name := maintenanceJobName(request.Mode, request.ExecutionKey)
	readOnly := request.Mode == "checkpoint"
	volumes := []any{
		map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": runtime.PersistentVolumeClaim, "readOnly": readOnly}},
		map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
	}
	mounts := []any{
		map[string]any{"name": "data", "mountPath": managedRegistryStorageRoot, "readOnly": readOnly},
		map[string]any{"name": "tmp", "mountPath": "/tmp"},
	}
	if request.Mode == "gc" {
		volumes = append(volumes, map[string]any{"name": "config", "configMap": map[string]any{"name": runtime.RegistryConfigMap, "defaultMode": int64(292), "items": []any{map[string]any{"key": "config.yml", "path": "config.yml"}}}})
		mounts = append(mounts, map[string]any{"name": "config", "mountPath": managedRegistryConfigPath, "subPath": "config.yml", "readOnly": true})
	}
	labels := map[string]any{"app.kubernetes.io/name": "kuberploy", "app.kubernetes.io/component": "registry-maintenance",
		"kuberploy.io/registry-target": runtime.TargetID, "kuberploy.io/maintenance-mode": request.Mode}
	object := map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": runtime.Namespace, "labels": labels,
			"annotations": map[string]any{"kuberploy.io/input-digest": inputDigest, "kuberploy.io/execution-key": request.ExecutionKey}},
		"spec": map[string]any{"backoffLimit": int64(0), "activeDeadlineSeconds": int64(1800), "completions": int64(1), "parallelism": int64(1),
			"template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": map[string]any{
				"serviceAccountName": runtime.HelperServiceAccount, "automountServiceAccountToken": false, "restartPolicy": "Never", "enableServiceLinks": false,
				"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": int64(10000), "runAsGroup": int64(10000), "fsGroup": int64(10000), "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
				"containers": []any{map[string]any{"name": "maintenance", "image": runtime.HelperImage, "imagePullPolicy": "IfNotPresent",
					"command": []any{"/kuberploy-worker"}, "args": []any{"registry-maintenance-helper"},
					"env":                    []any{map[string]any{"name": RegistryMaintenanceHelperModeEnv, "value": request.Mode}, map[string]any{"name": RegistryMaintenanceHelperRequestEnv, "value": string(encoded)}},
					"terminationMessagePath": registryMaintenanceTerminationPath, "terminationMessagePolicy": "File",
					"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []any{"ALL"}}},
					"resources":       map[string]any{"requests": map[string]any{"cpu": "25m", "memory": "32Mi"}, "limits": map[string]any{"cpu": "500m", "memory": "256Mi"}},
					"volumeMounts":    mounts}}, "volumes": volumes}}},
	}
	return object, inputDigest, nil
}

func validateRegistryMaintenanceJob(actual, expected map[string]any, runtime RuntimeConfig, inputDigest string) error {
	if actual["apiVersion"] != "batch/v1" || actual["kind"] != "Job" {
		return ErrRegistryMaintenanceInvalid
	}
	actualMetadata, _ := actual["metadata"].(map[string]any)
	expectedMetadata, _ := expected["metadata"].(map[string]any)
	uid, _ := actualMetadata["uid"].(string)
	rv, _ := actualMetadata["resourceVersion"].(string)
	annotations, _ := actualMetadata["annotations"].(map[string]any)
	actualLabels, _ := actualMetadata["labels"].(map[string]any)
	expectedLabels, _ := expectedMetadata["labels"].(map[string]any)
	if actualMetadata["name"] != expectedMetadata["name"] || actualMetadata["namespace"] != runtime.Namespace || uid == "" || rv == "" ||
		annotations["kuberploy.io/input-digest"] != inputDigest || !equalRegistryJSON(actualLabels, expectedLabels) {
		return ErrRegistryMaintenanceInvalid
	}
	actualSpec, _ := actual["spec"].(map[string]any)
	expectedSpec, _ := expected["spec"].(map[string]any)
	// Server-populated selector and template labels are ignored; every
	// operator-controlled field is compared through a canonical closed view.
	if !equalMaintenanceJobSpec(actualSpec, expectedSpec) {
		return ErrRegistryMaintenanceInvalid
	}
	return nil
}

func equalMaintenanceJobSpec(actual, expected map[string]any) bool {
	keys := []string{"backoffLimit", "activeDeadlineSeconds", "completions", "parallelism"}
	for _, key := range keys {
		if actual[key] != expected[key] {
			return false
		}
	}
	actualTemplate, okA := actual["template"].(map[string]any)
	expectedTemplate, okE := expected["template"].(map[string]any)
	if !okA || !okE {
		return false
	}
	aSpec, okA := actualTemplate["spec"].(map[string]any)
	eSpec, okE := expectedTemplate["spec"].(map[string]any)
	if !okA || !okE {
		return false
	}
	view := func(template map[string]any, spec map[string]any) any {
		metadata, _ := template["metadata"].(map[string]any)
		return map[string]any{"serviceAccountName": spec["serviceAccountName"], "automountServiceAccountToken": spec["automountServiceAccountToken"],
			"restartPolicy": spec["restartPolicy"], "enableServiceLinks": spec["enableServiceLinks"], "securityContext": spec["securityContext"],
			"containers": spec["containers"], "volumes": spec["volumes"], "labels": metadata["labels"]}
	}
	left, _ := json.Marshal(view(actualTemplate, aSpec))
	right, _ := json.Marshal(view(expectedTemplate, eSpec))
	return bytes.Equal(left, right)
}

func equalRegistryJSON(left, right any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}

func maintenanceRequestFromJob(job map[string]any, runtime RuntimeConfig, mode string) (maintenanceHelperRequest, error) {
	spec, ok := nestedMap(job, "spec", "template", "spec")
	if !ok || spec["serviceAccountName"] != runtime.HelperServiceAccount || spec["automountServiceAccountToken"] != false {
		return maintenanceHelperRequest{}, ErrRegistryMaintenanceInvalid
	}
	containers, _ := spec["containers"].([]any)
	if len(containers) != 1 {
		return maintenanceHelperRequest{}, ErrRegistryMaintenanceInvalid
	}
	container, _ := containers[0].(map[string]any)
	environment, _ := container["env"].([]any)
	values := map[string]string{}
	for _, raw := range environment {
		entry, _ := raw.(map[string]any)
		name, _ := entry["name"].(string)
		value, _ := entry["value"].(string)
		values[name] = value
	}
	if len(values) != 2 || values[RegistryMaintenanceHelperModeEnv] != mode || len(values[RegistryMaintenanceHelperRequestEnv]) > maximumMaintenanceResultBytes {
		return maintenanceHelperRequest{}, ErrRegistryMaintenanceInvalid
	}
	var request maintenanceHelperRequest
	decoder := json.NewDecoder(strings.NewReader(values[RegistryMaintenanceHelperRequestEnv]))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || request.validate(mode) != nil {
		return maintenanceHelperRequest{}, ErrRegistryMaintenanceInvalid
	}
	expected, inputDigest, err := registryMaintenanceJob(runtime, request)
	metadata, _ := job["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	if err != nil || annotations["kuberploy.io/input-digest"] != inputDigest || validateRegistryMaintenanceJob(job, expected, runtime, inputDigest) != nil {
		return maintenanceHelperRequest{}, ErrRegistryMaintenanceInvalid
	}
	return request, nil
}

func (c *registryKubernetesClient) waitJob(ctx context.Context, runtime RuntimeConfig, request maintenanceHelperRequest, initial map[string]any) (maintenanceHelperResult, RegistryMaintenanceJobEvidence, error) {
	name := maintenanceJobName(request.Mode, request.ExecutionKey)
	job := initial
	expected, inputDigest, err := registryMaintenanceJob(runtime, request)
	if err != nil {
		return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, err
	}
	deadline := c.now().Add(c.timeout)
	for {
		if validateRegistryMaintenanceJob(job, expected, runtime, inputDigest) != nil {
			return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, ErrRegistryMaintenanceInvalid
		}
		failed, complete := registryJobState(job)
		if failed {
			return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, ErrRegistryGCSweepUnconfirmed
		}
		if complete {
			break
		}
		if !c.now().Before(deadline) {
			return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, ErrRegistryMaintenanceUnavailable
		}
		if err := waitRegistryPoll(ctx); err != nil {
			return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, err
		}
		var err error
		job, err = c.get(ctx, registryJobPath(runtime, name), nil)
		if err != nil {
			return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, err
		}
	}
	metadata, _ := job["metadata"].(map[string]any)
	jobUID, _ := metadata["uid"].(string)
	query := url.Values{"labelSelector": {"job-name=" + name}, "limit": {"2"}}
	podList, err := c.get(ctx, "/api/v1/namespaces/"+url.PathEscape(runtime.Namespace)+"/pods", query)
	if err != nil || podList["kind"] != "PodList" || nestedString(podList, "metadata", "continue") != "" {
		return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, ErrRegistryMaintenanceInvalid
	}
	items, _ := podList["items"].([]any)
	if len(items) != 1 {
		return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, ErrRegistryMaintenanceInvalid
	}
	pod, _ := items[0].(map[string]any)
	podMetadata, _ := pod["metadata"].(map[string]any)
	if !podOwnedByJob(podMetadata, name, jobUID) {
		return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, ErrRegistryMaintenanceInvalid
	}
	statuses, _ := nestedSlice(pod, "status", "containerStatuses")
	if len(statuses) != 1 {
		return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, ErrRegistryMaintenanceInvalid
	}
	status, _ := statuses[0].(map[string]any)
	terminated, ok := nestedMap(status, "state", "terminated")
	exitCode, exitOK := jsonInt32(terminated["exitCode"])
	message, _ := terminated["message"].(string)
	startedAt, startErr := time.Parse(time.RFC3339Nano, stringValue(terminated["startedAt"]))
	completedAt, completeErr := time.Parse(time.RFC3339Nano, stringValue(terminated["finishedAt"]))
	if !ok || !exitOK || exitCode != 0 || len(message) == 0 || len(message) > maximumMaintenanceResultBytes || startErr != nil || completeErr != nil || completedAt.Before(startedAt) {
		return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, ErrRegistryMaintenanceInvalid
	}
	result, err := decodeMaintenanceHelperResult(message, request.Mode)
	if err != nil {
		return maintenanceHelperResult{}, RegistryMaintenanceJobEvidence{}, ErrRegistryMaintenanceInvalid
	}
	return result, RegistryMaintenanceJobEvidence{Name: name, UID: jobUID, InputDigest: inputDigest, StartedAt: startedAt.UTC(), CompletedAt: completedAt.UTC()}, nil
}

func decodeMaintenanceHelperResult(message, mode string) (maintenanceHelperResult, error) {
	if len(message) == 0 || len(message) > maximumMaintenanceResultBytes || (mode != "checkpoint" && mode != "gc") {
		return maintenanceHelperResult{}, ErrRegistryMaintenanceInvalid
	}
	var result maintenanceHelperResult
	decoder := json.NewDecoder(strings.NewReader(message))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil {
		return maintenanceHelperResult{}, ErrRegistryMaintenanceInvalid
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || result.Version != 1 || result.Mode != mode ||
		(mode == "checkpoint" && (result.Checkpoint == nil || result.Sweep != nil)) ||
		(mode == "gc" && (result.Sweep == nil || result.Checkpoint != nil)) {
		return maintenanceHelperResult{}, ErrRegistryMaintenanceInvalid
	}
	return result, nil
}

func registryJobState(job map[string]any) (failed, complete bool) {
	conditions, _ := nestedSlice(job, "status", "conditions")
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["status"] != "True" {
			continue
		}
		switch condition["type"] {
		case "Failed":
			failed = true
		case "Complete":
			complete = true
		}
	}
	return failed, complete
}

func nestedSlice(root map[string]any, keys ...string) ([]any, bool) {
	if len(keys) == 0 {
		return nil, false
	}
	parent, ok := nestedMap(root, keys[:len(keys)-1]...)
	if !ok {
		return nil, false
	}
	value, ok := parent[keys[len(keys)-1]].([]any)
	return value, ok
}

func podOwnedByJob(metadata map[string]any, name, uid string) bool {
	references, _ := metadata["ownerReferences"].([]any)
	for _, raw := range references {
		reference, _ := raw.(map[string]any)
		if reference["apiVersion"] == "batch/v1" && reference["kind"] == "Job" && reference["name"] == name && reference["uid"] == uid && reference["controller"] == true {
			return true
		}
	}
	return false
}

func (c *registryKubernetesClient) DeleteJob(ctx context.Context, runtime RuntimeConfig, evidence RegistryMaintenanceJobEvidence) error {
	if runtime != c.runtime || !validRegistryMaintenanceJobName(evidence.Name) || evidence.UID == "" || evidence.InputDigest == "" {
		return ErrRegistryMaintenanceInvalid
	}
	job, err := c.get(ctx, registryJobPath(runtime, evidence.Name), nil)
	if errors.Is(err, errRegistryKubernetesNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	metadata, _ := job["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	if metadata["uid"] != evidence.UID || annotations["kuberploy.io/input-digest"] != evidence.InputDigest {
		return ErrRegistryMaintenanceInvalid
	}
	return c.deleteJobObject(ctx, runtime, job)
}

func (c *registryKubernetesClient) deleteJobObject(ctx context.Context, runtime RuntimeConfig, job map[string]any) error {
	metadata, _ := job["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	uid, _ := metadata["uid"].(string)
	rv, _ := metadata["resourceVersion"].(string)
	if !validRegistryMaintenanceJobName(name) || uid == "" || rv == "" {
		return ErrRegistryMaintenanceInvalid
	}
	body := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground",
		"preconditions": map[string]any{"uid": uid, "resourceVersion": rv}}
	if err := c.delete(ctx, registryJobPath(runtime, name), body); err != nil && !errors.Is(err, errRegistryKubernetesNotFound) {
		return err
	}
	deadline := c.now().Add(time.Minute)
	for c.now().Before(deadline) {
		_, err := c.get(ctx, registryJobPath(runtime, name), nil)
		if errors.Is(err, errRegistryKubernetesNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = waitRegistryPoll(ctx); err != nil {
			return err
		}
	}
	return ErrRegistryMaintenanceUnavailable
}

func registryDeploymentPath(runtime RuntimeConfig) string {
	return "/apis/apps/v1/namespaces/" + url.PathEscape(runtime.Namespace) + "/deployments/" + url.PathEscape(runtime.Deployment)
}

func registryJobsPath(runtime RuntimeConfig) string {
	return "/apis/batch/v1/namespaces/" + url.PathEscape(runtime.Namespace) + "/jobs"
}

func registryPVCPath(runtime RuntimeConfig) string {
	return "/api/v1/namespaces/" + url.PathEscape(runtime.Namespace) + "/persistentvolumeclaims/" + url.PathEscape(runtime.PersistentVolumeClaim)
}

func registryConfigMapPath(runtime RuntimeConfig) string {
	return "/api/v1/namespaces/" + url.PathEscape(runtime.Namespace) + "/configmaps/" + url.PathEscape(runtime.RegistryConfigMap)
}

func registryJobPath(runtime RuntimeConfig, name string) string {
	return registryJobsPath(runtime) + "/" + url.PathEscape(name)
}

func (c *registryKubernetesClient) get(ctx context.Context, path string, query url.Values) (map[string]any, error) {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.request(ctx, http.MethodGet, path, nil, http.StatusOK)
}

func (c *registryKubernetesClient) create(ctx context.Context, path string, object map[string]any) (map[string]any, error) {
	return c.request(ctx, http.MethodPost, path, object, http.StatusCreated)
}

func (c *registryKubernetesClient) put(ctx context.Context, path string, object map[string]any) (map[string]any, error) {
	return c.request(ctx, http.MethodPut, path, object, http.StatusOK)
}

func (c *registryKubernetesClient) delete(ctx context.Context, path string, object map[string]any) error {
	_, err := c.request(ctx, http.MethodDelete, path, object, http.StatusOK, http.StatusAccepted)
	return err
}

func (c *registryKubernetesClient) request(ctx context.Context, method, path string, object map[string]any, expected ...int) (map[string]any, error) {
	if c == nil || c.http == nil || !validRegistryKubernetesRequestPath(c.runtime, method, path) {
		return nil, ErrRegistryMaintenanceInvalid
	}
	tokenFile, err := os.Open(c.tokenPath)
	if err != nil {
		return nil, errors.New("read Kubernetes service account token")
	}
	tokenBytes, readErr := io.ReadAll(io.LimitReader(tokenFile, (32<<10)+1))
	closeErr := tokenFile.Close()
	defer zeroBytes(tokenBytes)
	token := strings.TrimSpace(string(tokenBytes))
	if readErr != nil || closeErr != nil || token == "" || len(token) != len(tokenBytes) || len(tokenBytes) > 32<<10 || strings.IndexFunc(token, func(r rune) bool { return r < 0x21 || r == 0x7f }) >= 0 {
		return nil, errors.New("Kubernetes service account token is invalid")
	}
	var body []byte
	if object != nil {
		body, err = json.Marshal(object)
		if err != nil || len(body) > 1<<20 {
			zeroBytes(body)
			return nil, ErrRegistryMaintenanceInvalid
		}
		defer zeroBytes(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("create Kubernetes API request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, errors.New("Kubernetes API request failed")
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maximumRegistryKubernetesBodyBytes+1))
	if err != nil || len(encoded) > maximumRegistryKubernetesBodyBytes {
		zeroBytes(encoded)
		return nil, errors.New("read bounded Kubernetes API response")
	}
	defer zeroBytes(encoded)
	if response.StatusCode == http.StatusNotFound {
		return nil, errRegistryKubernetesNotFound
	}
	if response.StatusCode == http.StatusConflict {
		return nil, errRegistryKubernetesConflict
	}
	accepted := false
	for _, status := range expected {
		accepted = accepted || response.StatusCode == status
	}
	if !accepted {
		return nil, fmt.Errorf("Kubernetes managed registry request returned HTTP %d", response.StatusCode)
	}
	if len(encoded) == 0 {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var result map[string]any
	if err = decoder.Decode(&result); err != nil {
		return nil, ErrRegistryMaintenanceInvalid
	}
	return normalizeRegistryKubernetesJSON(result).(map[string]any), nil
}

func validRegistryKubernetesRequestPath(runtime RuntimeConfig, method, path string) bool {
	parsed, err := url.ParseRequestURI(path)
	if err != nil || parsed.Fragment != "" {
		return false
	}
	deployment := registryDeploymentPath(runtime)
	if parsed.Path == deployment && method == http.MethodGet || parsed.Path == deployment+"/scale" && method == http.MethodPut {
		return parsed.RawQuery == ""
	}
	if (parsed.Path == registryPVCPath(runtime) || parsed.Path == registryConfigMapPath(runtime)) && method == http.MethodGet {
		return parsed.RawQuery == ""
	}
	jobs := registryJobsPath(runtime)
	if parsed.Path == jobs {
		return method == http.MethodPost && parsed.RawQuery == ""
	}
	if strings.HasPrefix(parsed.Path, jobs+"/") {
		name, unescapeErr := url.PathUnescape(strings.TrimPrefix(parsed.Path, jobs+"/"))
		return unescapeErr == nil && validRegistryMaintenanceJobName(name) && !strings.Contains(name, "/") && parsed.RawQuery == "" && (method == http.MethodGet || method == http.MethodDelete)
	}
	pods := "/api/v1/namespaces/" + url.PathEscape(runtime.Namespace) + "/pods"
	if parsed.Path != pods || method != http.MethodGet {
		return false
	}
	query := parsed.Query()
	if len(query) != 2 || len(query["labelSelector"]) != 1 || len(query["limit"]) != 1 || query.Get("limit") != "2" {
		return false
	}
	selector := query.Get("labelSelector")
	registrySelector := "app.kubernetes.io/name=kuberploy-registry,app.kubernetes.io/instance=" + runtime.Deployment + ",app.kubernetes.io/component=registry"
	if selector == registrySelector {
		return true
	}
	return strings.HasPrefix(selector, "job-name=") && validRegistryMaintenanceJobName(strings.TrimPrefix(selector, "job-name="))
}

func validRegistryMaintenanceJobName(value string) bool {
	return registryMaintenanceJobNameRE.MatchString(value)
}

func normalizeRegistryKubernetesJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			typed[key] = normalizeRegistryKubernetesJSON(item)
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = normalizeRegistryKubernetesJSON(item)
		}
		return typed
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer
		}
		return typed.String()
	default:
		return value
	}
}

func jsonInt32(value any) (int32, bool) {
	integer, ok := jsonInt64(value)
	return int32(integer), ok && integer >= -2147483648 && integer <= 2147483647
}

func jsonInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case string:
		value, err := strconv.ParseInt(typed, 10, 64)
		return value, err == nil
	default:
		return 0, false
	}
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

var _ RegistryMaintenanceWorkloads = (*registryKubernetesClient)(nil)
