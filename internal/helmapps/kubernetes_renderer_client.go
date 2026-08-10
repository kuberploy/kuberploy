package helmapps

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
	"strconv"
	"strings"
	"time"
)

const (
	rendererServiceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"
	maximumRendererAPIResponse      = 4 << 20
	maximumRendererRequest          = 1 << 20
)

// InClusterRendererKubernetesAPI is a path-closed Kubernetes client. It can
// reach only ConfigMaps, Jobs, NetworkPolicies, the Job-selected Pod list, and
// the fixed renderer log subresource in its configured namespace.
type InClusterRendererKubernetesAPI struct {
	baseURL   string
	http      *http.Client
	tokenPath string
	namespace string
}

func NewInClusterRendererKubernetesAPI(namespace string) (*InClusterRendererKubernetesAPI, error) {
	if !dnsLabelRE.MatchString(namespace) {
		return nil, ErrInvalid
	}
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes in-cluster service environment is missing")
	}
	caPEM, err := os.ReadFile(rendererServiceAccountDirectory + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes service CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("Kubernetes service CA is invalid")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: pool,
	}}
	client := &http.Client{
		Timeout: 15 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Kubernetes API redirects are not allowed")
		},
	}
	return &InClusterRendererKubernetesAPI{
		baseURL: "https://" + net.JoinHostPort(host, port), http: client,
		tokenPath: rendererServiceAccountDirectory + "/token", namespace: namespace,
	}, nil
}

func (c *InClusterRendererKubernetesAPI) Get(ctx context.Context, resource rendererKubernetesResource, namespace, name string) (map[string]any, error) {
	path, err := c.objectPath(resource, namespace, name)
	if err != nil {
		return nil, err
	}
	status, body, err := c.request(ctx, http.MethodGet, path, nil, maximumRendererAPIResponse)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, ErrRendererObjectNotFound
	}
	if status < 200 || status >= 300 {
		return nil, rendererHTTPError(status, body)
	}
	return decodeRendererObject(body)
}

func (c *InClusterRendererKubernetesAPI) Create(ctx context.Context, resource rendererKubernetesResource, namespace string, object map[string]any) (map[string]any, error) {
	path, err := c.collectionPath(resource, namespace)
	if err != nil || rendererObjectName(object) == "" {
		return nil, ErrInvalid
	}
	body, err := json.Marshal(object)
	if err != nil || len(body) == 0 || len(body) > maximumRendererRequest {
		return nil, ErrInvalid
	}
	status, response, err := c.request(ctx, http.MethodPost, path, body, maximumRendererAPIResponse)
	if err != nil {
		return nil, err
	}
	if status == http.StatusConflict {
		return nil, ErrRendererObjectConflict
	}
	if status < 200 || status >= 300 {
		return nil, rendererHTTPError(status, response)
	}
	return decodeRendererObject(response)
}

func (c *InClusterRendererKubernetesAPI) Delete(ctx context.Context, resource rendererKubernetesResource, namespace, name string, preconditions RendererDeletePreconditions) error {
	path, err := c.objectPath(resource, namespace, name)
	if err != nil || preconditions.UID == "" || len(preconditions.UID) > 256 ||
		preconditions.ResourceVersion == "" || len(preconditions.ResourceVersion) > 256 ||
		(preconditions.PropagationPolicy != "Foreground" && preconditions.PropagationPolicy != "Background") {
		return ErrInvalid
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "DeleteOptions",
		"propagationPolicy": preconditions.PropagationPolicy,
		"preconditions": map[string]any{
			"uid": preconditions.UID, "resourceVersion": preconditions.ResourceVersion,
		},
	})
	if err != nil {
		return ErrInvalid
	}
	status, response, err := c.request(ctx, http.MethodDelete, path, body, 256<<10)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return ErrRendererObjectNotFound
	}
	if status == http.StatusConflict {
		return ErrRendererObjectConflict
	}
	if status < 200 || status >= 300 {
		return rendererHTTPError(status, response)
	}
	return nil
}

func (c *InClusterRendererKubernetesAPI) ListJobPods(ctx context.Context, namespace, jobName string) ([]map[string]any, error) {
	if namespace != c.namespace || !dnsLabelRE.MatchString(jobName) {
		return nil, ErrInvalid
	}
	query := url.Values{}
	query.Set("labelSelector", "batch.kubernetes.io/job-name="+jobName)
	query.Set("limit", "2")
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods?" + query.Encode()
	status, body, err := c.request(ctx, http.MethodGet, path, nil, maximumRendererAPIResponse)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, rendererHTTPError(status, body)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err = decoder.Decode(&list); err != nil || len(list.Items) > 2 {
		return nil, ErrConflict
	}
	return list.Items, nil
}

func (c *InClusterRendererKubernetesAPI) PodLogs(ctx context.Context, namespace, pod, container string, limit int64) ([]byte, error) {
	if namespace != c.namespace || !dnsLabelRE.MatchString(pod) ||
		container != RendererContainerName || limit != MaximumOutputSize+1 {
		return nil, ErrInvalid
	}
	query := url.Values{}
	query.Set("container", RendererContainerName)
	query.Set("limitBytes", strconv.FormatInt(limit, 10))
	query.Set("timestamps", "false")
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods/" +
		url.PathEscape(pod) + "/log?" + query.Encode()
	status, body, err := c.request(ctx, http.MethodGet, path, nil, limit)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, rendererHTTPError(status, body)
	}
	return body, nil
}

func (c *InClusterRendererKubernetesAPI) collectionPath(resource rendererKubernetesResource, namespace string) (string, error) {
	if c == nil || namespace != c.namespace || !dnsLabelRE.MatchString(namespace) {
		return "", ErrInvalid
	}
	switch resource {
	case rendererConfigMaps:
		return "/api/v1/namespaces/" + url.PathEscape(namespace) + "/configmaps", nil
	case rendererJobs:
		return "/apis/batch/v1/namespaces/" + url.PathEscape(namespace) + "/jobs", nil
	case rendererNetworkPolicies:
		return "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(namespace) + "/networkpolicies", nil
	default:
		return "", ErrInvalid
	}
}

func (c *InClusterRendererKubernetesAPI) objectPath(resource rendererKubernetesResource, namespace, name string) (string, error) {
	prefix, err := c.collectionPath(resource, namespace)
	if err != nil || !dnsLabelRE.MatchString(name) {
		return "", ErrInvalid
	}
	return prefix + "/" + url.PathEscape(name), nil
}

func (c *InClusterRendererKubernetesAPI) request(ctx context.Context, method, path string, body []byte, responseLimit int64) (int, []byte, error) {
	if c == nil || c.http == nil || c.baseURL == "" || c.tokenPath == "" ||
		responseLimit < 1 || responseLimit > maximumRendererAPIResponse ||
		(method != http.MethodGet && method != http.MethodPost && method != http.MethodDelete) {
		return 0, nil, ErrInvalid
	}
	token, err := os.ReadFile(c.tokenPath)
	if err != nil || len(token) == 0 || len(token) > 64<<10 {
		return 0, nil, errors.New("read bounded Kubernetes service account token")
	}
	defer clear(token)
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if method == http.MethodPost || method == http.MethodDelete {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil {
		return 0, nil, err
	}
	if int64(len(responseBody)) > responseLimit {
		return 0, nil, errors.New("Kubernetes renderer API response exceeded its bound")
	}
	return response.StatusCode, responseBody, nil
}

func decodeRendererObject(body []byte) (map[string]any, error) {
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, ErrConflict
	}
	return object, nil
}

func rendererHTTPError(status int, body []byte) error {
	detail := strings.TrimSpace(string(body))
	if len(detail) > 2048 {
		detail = detail[:2048]
	}
	return fmt.Errorf("Kubernetes renderer API returned HTTP %d: %s", status, detail)
}
