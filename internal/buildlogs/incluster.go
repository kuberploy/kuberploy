package buildlogs

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
	"strconv"
	"strings"
	"time"
)

const (
	buildLogServiceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"
	buildLogMaxKubernetesJSON       = int64(4 << 20)
)

var (
	buildOperationPattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	buildGenerationPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
	jobPathPattern         = regexp.MustCompile(`^/apis/batch/v1/namespaces/[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?/jobs/[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$`)
	podPathPattern         = regexp.MustCompile(`^/api/v1/namespaces/[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?/pods(?:/[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?(?:/log)?)?$`)
)

// InClusterClient is a narrow, GET-only Kubernetes adapter. Its method set and
// path allowlist make mutations, generic proxying, Secret reads, exec, attach,
// and port-forward structurally unavailable.
type InClusterClient struct {
	baseURL   string
	http      *http.Client
	tokenPath string
}

var _ KubernetesClient = (*InClusterClient)(nil)

// NewInClusterClient pins Kubernetes' injected HTTPS service endpoint, rejects
// redirects and environment proxies, and verifies the projected cluster CA.
func NewInClusterClient() (*InClusterClient, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes in-cluster service environment is missing")
	}
	caPEM, err := os.ReadFile(buildLogServiceAccountDirectory + "/ca.crt")
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
	return &InClusterClient{
		baseURL: "https://" + net.JoinHostPort(host, port),
		http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Kubernetes API redirects are not allowed")
		}},
		tokenPath: buildLogServiceAccountDirectory + "/token",
	}, nil
}

func (c *InClusterClient) Security() ClientSecurity {
	return ClientSecurity{TLSVerified: true, InsecureSkipTLSVerify: false}
}

// Probe proves that the fixed in-cluster TLS endpoint and projected service
// account token can reach Kubernetes discovery. The path is fixed, carries no
// caller input, and cannot address a namespaced object or subresource.
func (c *InClusterClient) Probe(ctx context.Context) error {
	object, err := c.getObject(ctx, "/api", nil)
	if err != nil {
		return err
	}
	versions, ok := object["versions"].([]any)
	if !ok || len(versions) > 32 {
		return ErrScopeViolation
	}
	for _, version := range versions {
		if version == "v1" {
			return nil
		}
	}
	return ErrScopeViolation
}

func (c *InClusterClient) GetBuildJob(ctx context.Context, namespace, name string) (map[string]any, error) {
	if !validKubeObject(namespace, name) {
		return nil, ErrInvalidRequest
	}
	path := "/apis/batch/v1/namespaces/" + url.PathEscape(namespace) + "/jobs/" + url.PathEscape(name)
	object, err := c.getObject(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	if object["apiVersion"] != "batch/v1" || object["kind"] != "Job" || objectIdentity(object, "namespace") != namespace || objectIdentity(object, "name") != name {
		return nil, ErrScopeViolation
	}
	return object, nil
}

func (c *InClusterClient) ListBuildJobPods(ctx context.Context, query JobPodQuery) ([]map[string]any, error) {
	if !validKubeObject(query.Namespace, query.JobName) || !uidPattern.MatchString(query.JobUID) ||
		!buildOperationPattern.MatchString(query.OperationLabel) || !buildGenerationPattern.MatchString(query.GenerationLabel) {
		return nil, ErrInvalidRequest
	}
	values := url.Values{
		"labelSelector": {"kuberploy.io/build-operation=" + query.OperationLabel + ",kuberploy.io/build-generation=" + query.GenerationLabel},
		"limit":         {"2"},
	}
	path := "/api/v1/namespaces/" + url.PathEscape(query.Namespace) + "/pods"
	object, err := c.getObject(ctx, path, values)
	if err != nil {
		return nil, err
	}
	if object["apiVersion"] != "v1" || object["kind"] != "PodList" {
		return nil, ErrScopeViolation
	}
	metadata, _ := object["metadata"].(map[string]any)
	continuation, _ := metadata["continue"].(string)
	items, ok := object["items"].([]any)
	if !ok || continuation != "" || len(items) > 2 {
		return nil, ErrScopeViolation
	}
	result := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		pod, ok := raw.(map[string]any)
		if !ok || pod["apiVersion"] != "v1" || pod["kind"] != "Pod" || objectIdentity(pod, "namespace") != query.Namespace {
			return nil, ErrScopeViolation
		}
		result = append(result, pod)
	}
	return result, nil
}

func (c *InClusterClient) GetBuildPod(ctx context.Context, namespace, name string) (map[string]any, error) {
	if !validKubeObject(namespace, name) {
		return nil, ErrInvalidRequest
	}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods/" + url.PathEscape(name)
	object, err := c.getObject(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	if object["apiVersion"] != "v1" || object["kind"] != "Pod" || objectIdentity(object, "namespace") != namespace || objectIdentity(object, "name") != name {
		return nil, ErrScopeViolation
	}
	return object, nil
}

func (c *InClusterClient) OpenBuilderAgentLogs(ctx context.Context, ref ExactPodRef, options PodLogOptions) (io.ReadCloser, error) {
	if !validKubeObject(ref.Namespace, ref.Name) || !uidPattern.MatchString(ref.UID) || options.TailLines < 1 || options.TailLines > 2_000 ||
		options.LimitBytes < 1 || options.LimitBytes > 5<<20 || options.Follow && options.Previous {
		return nil, ErrInvalidRequest
	}
	// Bind the log subresource to the exact Pod instance immediately before the
	// request. Service also verifies the Job owner and Pod UID before this call.
	live, err := c.GetBuildPod(ctx, ref.Namespace, ref.Name)
	if err != nil {
		return nil, err
	}
	if objectIdentity(live, "uid") != ref.UID {
		return nil, ErrGone
	}
	values := url.Values{
		"container": {"agent"}, "tailLines": {strconv.FormatInt(options.TailLines, 10)},
		"limitBytes": {strconv.FormatInt(options.LimitBytes, 10)}, "timestamps": {strconv.FormatBool(options.Timestamps)},
		"follow": {strconv.FormatBool(options.Follow)},
	}
	if options.Previous {
		values.Set("previous", "true")
	}
	if options.SinceTime != nil {
		values.Set("sinceTime", options.SinceTime.UTC().Format(time.RFC3339Nano))
	}
	path := "/api/v1/namespaces/" + url.PathEscape(ref.Namespace) + "/pods/" + url.PathEscape(ref.Name) + "/log"
	response, err := c.request(ctx, path, values, "text/plain, application/json")
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		response.Body.Close()
		return nil, ErrGone
	}
	if response.StatusCode == http.StatusBadRequest && options.Previous {
		response.Body.Close()
		return nil, ErrPreviousUnavailable
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("Kubernetes logs returned HTTP %d", response.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if contentType != "" && contentType != "text/plain" {
		response.Body.Close()
		return nil, ErrScopeViolation
	}
	// Pod logs do not accept a UID precondition. Hold the response body unopened
	// and re-read the Pod after the log request has been accepted; a name reuse
	// between the preflight GET and subresource GET is therefore rejected before
	// any caller can consume bytes. Once accepted, Kubernetes has already bound
	// the open response to that container instance.
	confirmed, err := c.GetBuildPod(ctx, ref.Namespace, ref.Name)
	if err != nil {
		response.Body.Close()
		return nil, err
	}
	if objectIdentity(confirmed, "uid") != ref.UID {
		response.Body.Close()
		return nil, ErrGone
	}
	return &boundedReadCloser{Reader: io.LimitReader(response.Body, options.LimitBytes+1), closer: response.Body}, nil
}

func (c *InClusterClient) getObject(ctx context.Context, path string, query url.Values) (map[string]any, error) {
	response, err := c.request(ctx, path, query, "application/json")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if response.StatusCode == http.StatusGone {
		return nil, ErrGone
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Kubernetes API returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, buildLogMaxKubernetesJSON+1))
	if err != nil {
		return nil, errors.New("read bounded Kubernetes API response")
	}
	defer clear(body)
	if int64(len(body)) > buildLogMaxKubernetesJSON {
		return nil, ErrResponseLimitReached
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var object map[string]any
	if err = decoder.Decode(&object); err != nil || object == nil {
		return nil, ErrScopeViolation
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		return nil, ErrScopeViolation
	}
	normalized, ok := normalizeKubernetesJSON(object).(map[string]any)
	if !ok {
		return nil, ErrScopeViolation
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
		if err == nil {
			return integer
		}
		return typed
	default:
		return value
	}
}

func (c *InClusterClient) request(ctx context.Context, path string, query url.Values, accept string) (*http.Response, error) {
	if c == nil || c.http == nil || !strings.HasPrefix(c.baseURL, "https://") || !allowedBuildLogPath(path) || strings.ContainsAny(path, "\x00\r\n") {
		return nil, ErrInvalidRequest
	}
	tokenBytes, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return nil, errors.New("read Kubernetes service account token")
	}
	defer clear(tokenBytes)
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || len(token) > 32<<10 || strings.IndexFunc(token, func(r rune) bool { return r < 0x21 || r == 0x7f }) >= 0 {
		return nil, errors.New("Kubernetes service account token is invalid")
	}
	endpoint := c.baseURL + path
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Authorization", "Bearer "+token)
	return c.http.Do(request)
}

func allowedBuildLogPath(path string) bool {
	return path == "/api" || jobPathPattern.MatchString(path) || podPathPattern.MatchString(path)
}

func validKubeObject(namespace, name string) bool {
	return kubeNamePattern.MatchString(namespace) && kubeNamePattern.MatchString(name)
}

func objectIdentity(object map[string]any, field string) string {
	metadata, _ := object["metadata"].(map[string]any)
	value, _ := metadata[field].(string)
	return value
}

type boundedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *boundedReadCloser) Close() error { return r.closer.Close() }
