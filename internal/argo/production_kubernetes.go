package argo

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"
)

const maximumArgoRuntimeResponseBytes = int64(2 << 20)

const argoHardRefreshAnnotation = "argocd.argoproj.io/refresh"

// InClusterProductionClient exposes only two Kubernetes authority surfaces:
// server-side apply/delete for the deterministic Argo repository credential
// Secret family and exact-name GET for the installer-owned root Application.
// It has no Secret get/list/watch, generic resource, proxy, exec, log, or Argo
// mutation/sync API.
type InClusterProductionClient struct {
	baseURL   string
	http      *http.Client
	tokenPath string
}

func NewInClusterProductionClient() (*InClusterProductionClient, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes in-cluster service environment is missing")
	}
	caPEM, err := os.ReadFile(argoServiceAccountDirectory + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes service CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("Kubernetes service CA is invalid")
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       60 * time.Second,
	}
	return &InClusterProductionClient{baseURL: "https://" + net.JoinHostPort(host, port), http: &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Kubernetes API redirects are not allowed")
		},
	}, tokenPath: argoServiceAccountDirectory + "/token"}, nil
}

type repositorySecretApplyWire struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Type string            `json:"type"`
	Data map[string][]byte `json:"data"`
}

type partialObjectMetadataWire struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		UID             string            `json:"uid"`
		ResourceVersion string            `json:"resourceVersion"`
		Labels          map[string]string `json:"labels"`
		Annotations     map[string]string `json:"annotations"`
	} `json:"metadata"`
}

func (c *InClusterProductionClient) ApplyRepositoryCredential(ctx context.Context, apply RepositoryCredentialApply, now time.Time) (RepositoryCredentialObservation, error) {
	if c == nil || c.http == nil || apply.validate() != nil || now.IsZero() {
		return RepositoryCredentialObservation{}, ErrInvalid
	}
	wire := repositorySecretApplyWire{APIVersion: "v1", Kind: "Secret", Type: "Opaque"}
	wire.Metadata.Name, wire.Metadata.Namespace = apply.Name, apply.Namespace
	wire.Metadata.Labels = map[string]string{
		"app.kubernetes.io/managed-by":   "kuberploy",
		"app.kubernetes.io/part-of":      "kuberploy",
		"argocd.argoproj.io/secret-type": "repository",
		"kuberploy.io/git-binding-id":    apply.BindingID,
	}
	wire.Metadata.Annotations = map[string]string{
		"kuberploy.io/repository-spec-digest": apply.SpecDigest,
		"kuberploy.io/credential-contract":    "github-app-repository-v1",
	}
	wire.Data = map[string][]byte{
		"type":                    []byte("git"),
		"url":                     []byte(apply.RepositoryURL),
		"githubAppID":             []byte(canonicalPositiveInteger(apply.GitHubAppID)),
		"githubAppInstallationID": []byte(canonicalPositiveInteger(apply.InstallationID)),
		"githubAppPrivateKey":     append([]byte(nil), apply.PrivateKey...),
	}
	defer func() {
		for _, value := range wire.Data {
			clearBytes(value)
		}
	}()
	body, err := json.Marshal(wire)
	if err != nil || int64(len(body)) > maximumArgoRuntimeResponseBytes {
		clearBytes(body)
		return RepositoryCredentialObservation{}, ErrInvalid
	}
	defer clearBytes(body)
	query := url.Values{"fieldManager": {RepositoryCredentialFieldManager}, "force": {"true"}}
	requestPath := "/api/v1/namespaces/" + url.PathEscape(apply.Namespace) + "/secrets/" + url.PathEscape(apply.Name) + "?" + query.Encode()
	response, err := c.request(ctx, http.MethodPatch, requestPath, body, "application/apply-patch+yaml",
		"application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1")
	if err != nil {
		return RepositoryCredentialObservation{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return RepositoryCredentialObservation{}, fmt.Errorf("Kubernetes repository credential apply returned HTTP %d", response.StatusCode)
	}
	var metadata partialObjectMetadataWire
	if err = decodeBoundedJSON(response.Body, maximumArgoRuntimeResponseBytes, &metadata, false); err != nil {
		return RepositoryCredentialObservation{}, err
	}
	if metadata.Metadata.Name != apply.Name || metadata.Metadata.Namespace != apply.Namespace ||
		metadata.Metadata.Labels["app.kubernetes.io/managed-by"] != "kuberploy" ||
		metadata.Metadata.Labels["argocd.argoproj.io/secret-type"] != "repository" ||
		metadata.Metadata.Labels["kuberploy.io/git-binding-id"] != apply.BindingID ||
		metadata.Metadata.Annotations["kuberploy.io/repository-spec-digest"] != apply.SpecDigest ||
		metadata.Metadata.Annotations["kuberploy.io/credential-contract"] != "github-app-repository-v1" {
		return RepositoryCredentialObservation{}, ErrRepositoryCredentialNotReady
	}
	observation := RepositoryCredentialObservation{BindingID: apply.BindingID, Namespace: apply.Namespace, Name: apply.Name,
		UID: metadata.Metadata.UID, ResourceVersion: metadata.Metadata.ResourceVersion, SpecDigest: apply.SpecDigest, ObservedAt: now.UTC()}
	if observation.validateFor(apply, now.UTC()) != nil {
		return RepositoryCredentialObservation{}, ErrRepositoryCredentialNotReady
	}
	return observation, nil
}

func (c *InClusterProductionClient) DeleteRepositoryCredential(ctx context.Context, namespace, name, bindingID string, now time.Time) (RepositoryCredentialRevocationObservation, error) {
	expected, err := RepositoryCredentialName(bindingID)
	if c == nil || c.http == nil || err != nil || !kubeRE.MatchString(namespace) || name != expected || now.IsZero() {
		return RepositoryCredentialRevocationObservation{}, ErrInvalid
	}
	body := []byte(`{"apiVersion":"v1","kind":"DeleteOptions","propagationPolicy":"Background"}`)
	defer clearBytes(body)
	response, err := c.request(ctx, http.MethodDelete,
		"/api/v1/namespaces/"+url.PathEscape(namespace)+"/secrets/"+url.PathEscape(name), body, "application/json", "application/json")
	if err != nil {
		return RepositoryCredentialRevocationObservation{}, err
	}
	defer response.Body.Close()
	observation := RepositoryCredentialRevocationObservation{BindingID: bindingID, Namespace: namespace, Name: name,
		Absent: response.StatusCode == http.StatusNotFound, ObservedAt: now.UTC()}
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusOK || response.StatusCode == http.StatusAccepted {
		if observation.validate(bindingID, namespace, name, now.UTC()) != nil {
			return RepositoryCredentialRevocationObservation{}, ErrRepositoryCredentialNotReady
		}
		return observation, nil
	}
	return RepositoryCredentialRevocationObservation{}, fmt.Errorf("Kubernetes repository credential delete returned HTTP %d", response.StatusCode)
}

type rootApplicationDirectoryWire struct {
	Recurse bool `json:"recurse"`
}

type rootApplicationSourceWire struct {
	RepoURL        string                       `json:"repoURL"`
	TargetRevision string                       `json:"targetRevision"`
	Path           string                       `json:"path"`
	Directory      rootApplicationDirectoryWire `json:"directory"`
}

type rootApplicationDestinationWire struct {
	Server    string `json:"server"`
	Namespace string `json:"namespace"`
}

type rootApplicationAutomatedWire struct {
	AllowEmpty bool `json:"allowEmpty"`
	Prune      bool `json:"prune"`
	SelfHeal   bool `json:"selfHeal"`
}

type rootApplicationSyncPolicyWire struct {
	Automated   rootApplicationAutomatedWire `json:"automated"`
	SyncOptions []string                     `json:"syncOptions"`
}

type rootApplicationSpecWire struct {
	Project     string                         `json:"project"`
	Source      rootApplicationSourceWire      `json:"source"`
	Destination rootApplicationDestinationWire `json:"destination"`
	SyncPolicy  rootApplicationSyncPolicyWire  `json:"syncPolicy"`
}

type rootApplicationEnvelopeWire struct {
	Metadata struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		UID             string            `json:"uid"`
		ResourceVersion string            `json:"resourceVersion"`
		Labels          map[string]string `json:"labels"`
		Annotations     map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec   json.RawMessage `json:"spec"`
	Status struct {
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
	} `json:"status"`
}

// RefreshPlatformRootApplication performs one closed metadata-only patch and
// then reads the exact root back. Acceptance requires the immutable root spec,
// the verified provider revision, and Synced/Healthy status. It cannot select
// another Application and does not invoke Argo's sync API; the installer-owned
// automated policy remains the sole reconciliation executor. A stale read is a
// retryable failure, so the durable command issues another hard refresh.
func (c *InClusterProductionClient) RefreshPlatformRootApplication(ctx context.Context, expectation PlatformRootApplicationExpectation, now time.Time) error {
	expectedDigest, digestErr := expectation.expectedSpecDigest()
	if c == nil || c.http == nil || digestErr != nil || expectedDigest != expectation.SpecDigest || now.IsZero() {
		return ErrInvalid
	}
	body := []byte(`{"metadata":{"annotations":{"argocd.argoproj.io/refresh":"hard"}}}`)
	requestPath := "/apis/argoproj.io/v1alpha1/namespaces/" + url.PathEscape(expectation.Namespace) +
		"/applications/" + url.PathEscape(expectation.Name)
	response, err := c.request(ctx, http.MethodPatch, requestPath, body, "application/merge-patch+json",
		"application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Kubernetes root Application refresh returned HTTP %d", response.StatusCode)
	}
	var metadata partialObjectMetadataWire
	if err = decodeBoundedJSON(response.Body, maximumArgoRuntimeResponseBytes, &metadata, false); err != nil {
		return err
	}
	if metadata.Metadata.Namespace != expectation.Namespace || metadata.Metadata.Name != expectation.Name || metadata.Metadata.UID == "" ||
		metadata.Metadata.ResourceVersion == "" || metadata.Metadata.Annotations[argoHardRefreshAnnotation] != "hard" {
		return ErrPlatformRootNotReady
	}
	_, err = c.ObservePlatformRootApplication(ctx, expectation, now.UTC())
	return err
}

func (c *InClusterProductionClient) ObservePlatformRootApplication(ctx context.Context, expectation PlatformRootApplicationExpectation, now time.Time) (PlatformRootApplicationObservation, error) {
	expectedDigest, digestErr := expectation.expectedSpecDigest()
	if c == nil || c.http == nil || digestErr != nil || expectedDigest != expectation.SpecDigest || now.IsZero() {
		return PlatformRootApplicationObservation{}, ErrInvalid
	}
	requestPath := "/apis/argoproj.io/v1alpha1/namespaces/" + url.PathEscape(expectation.Namespace) +
		"/applications/" + url.PathEscape(expectation.Name)
	response, err := c.request(ctx, http.MethodGet, requestPath, nil, "", "application/json")
	if err != nil {
		return PlatformRootApplicationObservation{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return PlatformRootApplicationObservation{}, ErrPlatformRootNotReady
	}
	if response.StatusCode != http.StatusOK {
		return PlatformRootApplicationObservation{}, fmt.Errorf("Kubernetes root Application returned HTTP %d", response.StatusCode)
	}
	var envelope rootApplicationEnvelopeWire
	if err = decodeBoundedJSON(response.Body, maximumArgoRuntimeResponseBytes, &envelope, false); err != nil {
		return PlatformRootApplicationObservation{}, err
	}
	var spec rootApplicationSpecWire
	if err = decodeStrictJSON(envelope.Spec, &spec); err != nil {
		return PlatformRootApplicationObservation{}, ErrPlatformRootNotReady
	}
	wantSpec := platformRootApplicationSpec(expectation)
	if !reflect.DeepEqual(spec, wantSpec) || envelope.Metadata.Name != expectation.Name || envelope.Metadata.Namespace != expectation.Namespace ||
		envelope.Metadata.Annotations["kuberploy.io/repository-secret"] != expectation.RepositoryCredentialName ||
		envelope.Metadata.Labels["app.kubernetes.io/part-of"] != "kuberploy" {
		return PlatformRootApplicationObservation{}, ErrPlatformRootNotReady
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return PlatformRootApplicationObservation{}, ErrPlatformRootNotReady
	}
	digest := contentDigest(encoded)
	observation := PlatformRootApplicationObservation{Namespace: envelope.Metadata.Namespace, Name: envelope.Metadata.Name,
		UID: envelope.Metadata.UID, ResourceVersion: envelope.Metadata.ResourceVersion, SpecDigest: digest,
		ObservedRevision: strings.ToLower(strings.TrimSpace(envelope.Status.Sync.Revision)), SyncStatus: envelope.Status.Sync.Status,
		HealthStatus: envelope.Status.Health.Status, ObservedAt: now.UTC()}
	if observation.validateFor(expectation, now.UTC()) != nil {
		return PlatformRootApplicationObservation{}, ErrPlatformRootNotReady
	}
	return observation, nil
}

func (c *InClusterProductionClient) request(ctx context.Context, method, requestPath string, body []byte, contentType, accept string) (*http.Response, error) {
	if c == nil || c.http == nil || c.baseURL == "" || c.tokenPath == "" || !strings.HasPrefix(requestPath, "/") ||
		strings.ContainsAny(requestPath, "\x00\r\n") {
		return nil, ErrInvalid
	}
	tokenBytes, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes service token: %w", err)
	}
	defer clearBytes(tokenBytes)
	if len(tokenBytes) < 1 || len(tokenBytes) > maximumArgoTokenBytes {
		return nil, errors.New("Kubernetes service token is invalid")
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || strings.ContainsAny(token, "\x00\r\n") {
		return nil, errors.New("Kubernetes service token is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+requestPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", accept)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	return c.http.Do(request)
}

func decodeBoundedJSON(reader io.Reader, maximum int64, destination any, strict bool) error {
	limited := io.LimitReader(reader, maximum+1)
	body, err := io.ReadAll(limited)
	if err != nil || int64(len(body)) > maximum || len(body) == 0 {
		return ErrInvalid
	}
	if strict {
		return decodeStrictJSON(body, destination)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err = decoder.Decode(destination); err != nil {
		return ErrInvalid
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrInvalid
	}
	return nil
}

func decodeStrictJSON(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return ErrInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrInvalid
	}
	return nil
}

var _ RepositoryCredentialKubernetes = (*InClusterProductionClient)(nil)
var _ PlatformRootApplicationSource = (*InClusterProductionClient)(nil)
var _ PlatformRootRefresher = (*InClusterProductionClient)(nil)
