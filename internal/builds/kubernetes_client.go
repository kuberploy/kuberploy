package builds

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
	"regexp"
	"strings"
	"time"
)

const (
	buildServiceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"
	maximumKubernetesObjectBytes = 4 << 20
)

var kubernetesOpaqueIDRE = regexp.MustCompile(`^[A-Za-z0-9_.:/+=-]{1,256}$`)

type inClusterBuildResources struct {
	baseURL   string
	http      *http.Client
	tokenPath string
}

// NewInClusterKubernetesAdapter creates the production adapter from the
// worker's projected ServiceAccount credential. The adapter remains pinned to
// one configured builder namespace.
func NewInClusterKubernetesAdapter(namespace string, nodeIsolation bool) (*KubernetesAdapter, error) {
	resources, err := newInClusterBuildResources()
	if err != nil {
		return nil, err
	}
	return newKubernetesAdapterWithIsolation(resources, namespace, nodeIsolation)
}

func newInClusterBuildResources() (*inClusterBuildResources, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes in-cluster service environment is missing")
	}
	caPEM, err := os.ReadFile(buildServiceAccountDirectory + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes service CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("Kubernetes service CA is invalid")
	}
	transport := &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
	}
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Kubernetes API redirects are not allowed")
		},
	}
	return &inClusterBuildResources{
		baseURL: "https://" + net.JoinHostPort(host, port), http: client,
		tokenPath: buildServiceAccountDirectory + "/token",
	}, nil
}

func (c *inClusterBuildResources) Get(ctx context.Context, resource kubernetesResource, namespace, name string) (map[string]any, error) {
	path, err := kubernetesResourcePath(resource, namespace, name)
	if err != nil {
		return nil, err
	}
	encoded, status, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer clear(encoded)
	if status == http.StatusNotFound {
		return nil, errKubernetesObjectNotFound
	}
	if status != http.StatusOK {
		return nil, kubernetesStatusError(http.MethodGet, resource, status)
	}
	return decodeKubernetesObject(encoded)
}

func (c *inClusterBuildResources) Create(ctx context.Context, resource kubernetesResource, namespace string, object map[string]any) (map[string]any, error) {
	if objectNamespace(object) != namespace {
		return nil, ErrInvalid
	}
	path, err := kubernetesResourcePath(resource, namespace, "")
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(object)
	if err != nil || len(body) > builderRequestLimit(resource) {
		clear(body)
		return nil, ErrInvalid
	}
	defer clear(body)
	encoded, status, err := c.request(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	defer clear(encoded)
	if status == http.StatusConflict {
		return nil, errKubernetesObjectConflict
	}
	if status != http.StatusCreated {
		return nil, kubernetesStatusError(http.MethodPost, resource, status)
	}
	return decodeKubernetesObject(encoded)
}

func (c *inClusterBuildResources) Delete(ctx context.Context, resource kubernetesResource, namespace, name string, preconditions deletePreconditions) error {
	if !kubernetesOpaqueIDRE.MatchString(preconditions.UID) || !kubernetesOpaqueIDRE.MatchString(preconditions.ResourceVersion) ||
		(preconditions.PropagationPolicy != "Foreground" && preconditions.PropagationPolicy != "Background") {
		return ErrInvalid
	}
	path, err := kubernetesResourcePath(resource, namespace, name)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": preconditions.PropagationPolicy,
		"preconditions": map[string]any{"uid": preconditions.UID, "resourceVersion": preconditions.ResourceVersion},
	})
	if err != nil {
		return err
	}
	defer clear(body)
	encoded, status, err := c.request(ctx, http.MethodDelete, path, body)
	clear(encoded)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return errKubernetesObjectNotFound
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return kubernetesStatusError(http.MethodDelete, resource, status)
	}
	return nil
}

func (c *inClusterBuildResources) ListBuildPods(ctx context.Context, namespace, operation, generation string, limit int64) ([]map[string]any, error) {
	if !buildOperationLabelRE.MatchString(operation) || !buildGenerationLabelRE.MatchString(generation) || limit != 2 {
		return nil, ErrInvalid
	}
	path, err := kubernetesResourcePath(resourcePods, namespace, "")
	if err != nil {
		return nil, err
	}
	query := url.Values{
		"labelSelector": {"kuberploy.io/build-operation=" + operation + ",kuberploy.io/build-generation=" + generation},
		"limit":         {"2"},
	}
	encoded, status, err := c.request(ctx, http.MethodGet, path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer clear(encoded)
	if status != http.StatusOK {
		return nil, kubernetesStatusError(http.MethodGet, resourcePods, status)
	}
	object, err := decodeKubernetesObject(encoded)
	if err != nil || object["apiVersion"] != "v1" || object["kind"] != "PodList" {
		return nil, ErrInfrastructure
	}
	items, ok := object["items"].([]any)
	metadata, metadataOK := object["metadata"].(map[string]any)
	continuation, _ := metadata["continue"].(string)
	if !ok || !metadataOK || continuation != "" || len(items) > int(limit) {
		return nil, ErrInfrastructure
	}
	pods := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		pod, ok := raw.(map[string]any)
		if !ok {
			return nil, ErrInfrastructure
		}
		apiVersion, hasAPIVersion := pod["apiVersion"]
		kind, hasKind := pod["kind"]
		if hasAPIVersion != hasKind || hasAPIVersion && (apiVersion != "v1" || kind != "Pod") {
			return nil, ErrInfrastructure
		}
		// Kubernetes list responses may omit TypeMeta from every item because
		// the already-validated v1/PodList envelope fixes the item resource.
		// Reconstruct only those two constants; never accept partial or
		// substituted item TypeMeta.
		if !hasAPIVersion {
			pod["apiVersion"], pod["kind"] = "v1", "Pod"
		}
		pods = append(pods, pod)
	}
	return pods, nil
}

func (c *inClusterBuildResources) ListBuilderNodes(ctx context.Context, limit int64) ([]map[string]any, error) {
	if limit != 100 {
		return nil, ErrInvalid
	}
	query := url.Values{"limit": {"100"}}
	encoded, status, err := c.request(ctx, http.MethodGet, "/api/v1/nodes?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer clear(encoded)
	if status != http.StatusOK {
		return nil, fmt.Errorf("Kubernetes API GET nodes returned HTTP %d", status)
	}
	object, err := decodeKubernetesObject(encoded)
	if err != nil || object["apiVersion"] != "v1" || object["kind"] != "NodeList" {
		return nil, ErrInfrastructure
	}
	items, ok := object["items"].([]any)
	metadata, metadataOK := object["metadata"].(map[string]any)
	continuation, _ := metadata["continue"].(string)
	if !ok || !metadataOK || continuation != "" || len(items) > int(limit) {
		return nil, ErrInfrastructure
	}
	nodes := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		node, ok := raw.(map[string]any)
		if !ok {
			return nil, ErrInfrastructure
		}
		apiVersion, hasAPIVersion := node["apiVersion"]
		kind, hasKind := node["kind"]
		if hasAPIVersion != hasKind || hasAPIVersion && (apiVersion != "v1" || kind != "Node") {
			return nil, ErrInfrastructure
		}
		if !hasAPIVersion {
			node["apiVersion"], node["kind"] = "v1", "Node"
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

func (c *inClusterBuildResources) request(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	if !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/apis/") {
		return nil, 0, ErrInvalid
	}
	tokenBytes, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return nil, 0, fmt.Errorf("read Kubernetes service account token: %w", err)
	}
	defer clear(tokenBytes)
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || len(token) > 32<<10 || strings.IndexFunc(token, func(r rune) bool { return r < 0x21 || r == 0x7f }) >= 0 {
		return nil, 0, errors.New("Kubernetes service account token is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("Kubernetes API request failed: %w", err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maximumKubernetesObjectBytes+1))
	if err != nil {
		clear(encoded)
		return nil, 0, errors.New("read bounded Kubernetes API response")
	}
	if len(encoded) > maximumKubernetesObjectBytes {
		clear(encoded)
		return nil, 0, errors.New("Kubernetes API response exceeded 4 MiB")
	}
	return encoded, response.StatusCode, nil
}

func kubernetesResourcePath(resource kubernetesResource, namespace, name string) (string, error) {
	if !kubeNameRE.MatchString(namespace) || name != "" && !kubeNameRE.MatchString(name) {
		return "", ErrInvalid
	}
	var prefix string
	switch resource {
	case resourceConfigMaps, resourceSecrets, resourcePods:
		prefix = "/api/v1/namespaces/" + url.PathEscape(namespace) + "/" + string(resource)
	case resourceJobs:
		prefix = "/apis/batch/v1/namespaces/" + url.PathEscape(namespace) + "/jobs"
	case resourceNetworkPolicies:
		prefix = "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(namespace) + "/networkpolicies"
	default:
		return "", ErrInvalid
	}
	if name != "" {
		prefix += "/" + url.PathEscape(name)
	}
	return prefix, nil
}

func decodeKubernetesObject(encoded []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, ErrInfrastructure
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, ErrInfrastructure
	}
	normalized, ok := normalizeKubernetesJSON(object).(map[string]any)
	if !ok {
		return nil, ErrInfrastructure
	}
	return normalized, nil
}

func normalizeKubernetesJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			typed[key] = normalizeKubernetesJSON(item)
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = normalizeKubernetesJSON(item)
		}
		return typed
	case json.Number:
		integer, err := typed.Int64()
		if err != nil {
			return typed
		}
		return integer
	default:
		return value
	}
}

func kubernetesStatusError(method string, resource kubernetesResource, status int) error {
	return fmt.Errorf("Kubernetes API %s %s returned HTTP %d", method, resource, status)
}

func builderRequestLimit(resource kubernetesResource) int {
	if resource == resourceSecrets {
		return 16 << 10
	}
	return 1 << 20
}
