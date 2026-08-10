package secrets

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
	secretServiceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"
	maximumSecretObjectBytes      = 1 << 20
	maximumServiceAccountToken    = 32 << 10
)

var (
	errSecretObjectNotFound = errors.New("Kubernetes runtime-secret object not found")
	errSecretObjectConflict = errors.New("Kubernetes runtime-secret object already exists")
	kubernetesObjectIDRE    = regexp.MustCompile(`^[A-Za-z0-9_.:/+=-]{1,256}$`)
)

type secretKubernetesResource uint8

const (
	resourceExternalSecret secretKubernetesResource = iota + 1
	resourceSecretStore
	resourceSealedSecret
)

type secretDeletePreconditions struct {
	UID             string
	ResourceVersion string
}

// secretKubernetesResources is intentionally incapable of listing, watching,
// updating, patching, proxying, reading Secret objects, reading logs or
// executing in Pods. Every operation names one of the three runtime-secret
// custom resources and an exact namespace/name.
type secretKubernetesResources interface {
	Get(context.Context, secretKubernetesResource, string, string) (map[string]any, error)
	Create(context.Context, secretKubernetesResource, string, map[string]any) (map[string]any, error)
	Delete(context.Context, secretKubernetesResource, string, string, secretDeletePreconditions) error
}

type inClusterSecretResources struct {
	baseURL   string
	http      *http.Client
	tokenPath string
}

func newInClusterSecretResources() (*inClusterSecretResources, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes in-cluster service environment is missing")
	}
	caPEM, err := os.ReadFile(secretServiceAccountDirectory + "/ca.crt")
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
		Timeout:   10 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Kubernetes API redirects are not allowed")
		},
	}
	return &inClusterSecretResources{
		baseURL:   "https://" + net.JoinHostPort(host, port),
		http:      client,
		tokenPath: secretServiceAccountDirectory + "/token",
	}, nil
}

func (c *inClusterSecretResources) Get(ctx context.Context, resource secretKubernetesResource, namespace, name string) (map[string]any, error) {
	path, err := secretResourcePath(resource, namespace, name)
	if err != nil {
		return nil, err
	}
	body, status, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer clear(body)
	if status == http.StatusNotFound {
		return nil, errSecretObjectNotFound
	}
	if status != http.StatusOK {
		return nil, secretKubernetesStatusError(http.MethodGet, resource, status)
	}
	return decodeSecretKubernetesObject(body)
}

func (c *inClusterSecretResources) Create(ctx context.Context, resource secretKubernetesResource, namespace string, object map[string]any) (map[string]any, error) {
	if objectNamespace(object) != namespace {
		return nil, ErrInvalid
	}
	path, err := secretResourcePath(resource, namespace, "")
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(object)
	if err != nil || len(body) == 0 || len(body) > maximumSecretObjectBytes {
		clear(body)
		return nil, ErrInvalid
	}
	defer clear(body)
	response, status, err := c.request(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	defer clear(response)
	if status == http.StatusConflict {
		return nil, errSecretObjectConflict
	}
	if status != http.StatusCreated {
		return nil, secretKubernetesStatusError(http.MethodPost, resource, status)
	}
	return decodeSecretKubernetesObject(response)
}

func (c *inClusterSecretResources) Delete(ctx context.Context, resource secretKubernetesResource, namespace, name string, preconditions secretDeletePreconditions) error {
	if !kubernetesObjectIDRE.MatchString(preconditions.UID) || !kubernetesObjectIDRE.MatchString(preconditions.ResourceVersion) {
		return ErrInvalid
	}
	path, err := secretResourcePath(resource, namespace, name)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion":        "v1",
		"kind":              "DeleteOptions",
		"propagationPolicy": "Background",
		"preconditions": map[string]any{
			"uid": preconditions.UID, "resourceVersion": preconditions.ResourceVersion,
		},
	})
	if err != nil {
		return ErrProviderOperation
	}
	defer clear(body)
	response, status, err := c.request(ctx, http.MethodDelete, path, body)
	clear(response)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return errSecretObjectNotFound
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return secretKubernetesStatusError(http.MethodDelete, resource, status)
	}
	return nil
}

func (c *inClusterSecretResources) request(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	if c == nil || c.http == nil || c.baseURL == "" || c.tokenPath == "" ||
		(!strings.HasPrefix(path, "/apis/external-secrets.io/v1/namespaces/") && !strings.HasPrefix(path, "/apis/bitnami.com/v1alpha1/namespaces/")) {
		return nil, 0, ErrInvalid
	}
	tokenFile, err := os.Open(c.tokenPath)
	if err != nil {
		return nil, 0, errors.New("read Kubernetes service account token")
	}
	tokenBytes, readErr := io.ReadAll(io.LimitReader(tokenFile, maximumServiceAccountToken+1))
	closeErr := tokenFile.Close()
	defer clear(tokenBytes)
	if readErr != nil || closeErr != nil || len(tokenBytes) == 0 || len(tokenBytes) > maximumServiceAccountToken {
		return nil, 0, errors.New("Kubernetes service account token is invalid")
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || strings.IndexFunc(token, func(r rune) bool { return r < 0x21 || r == 0x7f }) >= 0 {
		return nil, 0, errors.New("Kubernetes service account token is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, errors.New("create Kubernetes API request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, 0, errors.New("Kubernetes API request failed")
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maximumSecretObjectBytes+1))
	if err != nil {
		clear(encoded)
		return nil, 0, errors.New("read bounded Kubernetes API response")
	}
	if len(encoded) > maximumSecretObjectBytes {
		clear(encoded)
		return nil, 0, errors.New("Kubernetes API response exceeded 1 MiB")
	}
	return encoded, response.StatusCode, nil
}

func secretResourcePath(resource secretKubernetesResource, namespace, name string) (string, error) {
	if !dnsLabelRE.MatchString(namespace) || name != "" && !kubeNameRE.MatchString(name) {
		return "", ErrInvalid
	}
	var prefix string
	switch resource {
	case resourceExternalSecret:
		prefix = "/apis/external-secrets.io/v1/namespaces/" + url.PathEscape(namespace) + "/externalsecrets"
	case resourceSecretStore:
		prefix = "/apis/external-secrets.io/v1/namespaces/" + url.PathEscape(namespace) + "/secretstores"
	case resourceSealedSecret:
		prefix = "/apis/bitnami.com/v1alpha1/namespaces/" + url.PathEscape(namespace) + "/sealedsecrets"
	default:
		return "", ErrInvalid
	}
	if name != "" {
		prefix += "/" + url.PathEscape(name)
	}
	return prefix, nil
}

func decodeSecretKubernetesObject(encoded []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, ErrProviderOperation
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, ErrProviderOperation
	}
	value, ok := normalizeSecretKubernetesJSON(object).(map[string]any)
	if !ok {
		return nil, ErrProviderOperation
	}
	return value, nil
}

func normalizeSecretKubernetesJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			typed[key] = normalizeSecretKubernetesJSON(item)
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = normalizeSecretKubernetesJSON(item)
		}
		return typed
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer
		}
		return typed
	default:
		return value
	}
}

func secretKubernetesStatusError(method string, resource secretKubernetesResource, status int) error {
	return fmt.Errorf("Kubernetes API %s runtime-secret resource %d returned HTTP %d", method, resource, status)
}

func objectMetadata(object map[string]any) map[string]any {
	metadata, _ := object["metadata"].(map[string]any)
	return metadata
}

func objectNamespace(object map[string]any) string {
	namespace, _ := objectMetadata(object)["namespace"].(string)
	return namespace
}

func objectName(object map[string]any) string {
	name, _ := objectMetadata(object)["name"].(string)
	return name
}
