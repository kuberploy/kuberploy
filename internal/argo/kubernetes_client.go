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
	"strconv"
	"strings"
	"time"
)

const (
	argoServiceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"
	maximumArgoResponseBytes    = int64(8 << 20)
	maximumArgoTokenBytes       = 16 << 10
)

// InClusterApplicationClient is a narrow read-only adapter. It can only list
// Argo Application custom resources in the namespace supplied by the caller,
// with the fixed Kuberploy ownership selector. It has no generic request,
// Secret, proxy, exec, log, patch, update, delete, or sync surface.
type InClusterApplicationClient struct {
	baseURL   string
	http      *http.Client
	tokenPath string
}

var _ KubernetesApplicationSource = (*InClusterApplicationClient)(nil)

func NewInClusterApplicationClient() (*InClusterApplicationClient, error) {
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
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, IdleConnTimeout: 60 * time.Second,
	}
	return &InClusterApplicationClient{
		baseURL: "https://" + net.JoinHostPort(host, port),
		http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Kubernetes API redirects are not allowed")
		}},
		tokenPath: argoServiceAccountDirectory + "/token",
	}, nil
}

func (c *InClusterApplicationClient) ListKuberployApplications(ctx context.Context, namespace, continuation string, limit int) (KubernetesApplicationPage, error) {
	if c == nil || c.http == nil || !kubeRE.MatchString(namespace) || limit < 1 || limit > 500 || len(continuation) > 1024 || strings.ContainsAny(continuation, "\x00\r\n") {
		return KubernetesApplicationPage{}, ErrInvalid
	}
	query := url.Values{
		"labelSelector": {KuberployApplicationSelector},
		"limit":         {strconv.Itoa(limit)},
	}
	if continuation != "" {
		query.Set("continue", continuation)
	}
	path := "/apis/argoproj.io/v1alpha1/namespaces/" + url.PathEscape(namespace) + "/applications?" + query.Encode()
	response, err := c.get(ctx, path)
	if err != nil {
		return KubernetesApplicationPage{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return KubernetesApplicationPage{}, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return KubernetesApplicationPage{}, fmt.Errorf("Kubernetes Applications returned HTTP %d", response.StatusCode)
	}
	limited := io.LimitReader(response.Body, maximumArgoResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return KubernetesApplicationPage{}, err
	}
	if int64(len(body)) > maximumArgoResponseBytes {
		return KubernetesApplicationPage{}, errors.New("Kubernetes Applications response exceeded its bound")
	}
	return decodeKubernetesApplicationPage(body, namespace)
}

func (c *InClusterApplicationClient) get(ctx context.Context, path string) (*http.Response, error) {
	if !strings.HasPrefix(path, "/apis/argoproj.io/v1alpha1/namespaces/") || strings.ContainsAny(path, "\x00\r\n") {
		return nil, ErrInvalid
	}
	tokenBytes, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes service token: %w", err)
	}
	defer clear(tokenBytes)
	if len(tokenBytes) == 0 || len(tokenBytes) > maximumArgoTokenBytes {
		return nil, errors.New("Kubernetes service token is invalid")
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || strings.ContainsAny(token, "\x00\r\n") {
		return nil, errors.New("Kubernetes service token is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	return c.http.Do(request)
}

type applicationListWire struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
		Continue        string `json:"continue"`
	} `json:"metadata"`
	Items []applicationWire `json:"items"`
}

type applicationWire struct {
	Metadata struct {
		UID             string            `json:"uid"`
		Namespace       string            `json:"namespace"`
		Name            string            `json:"name"`
		ResourceVersion string            `json:"resourceVersion"`
		Labels          map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Project     string `json:"project"`
		Destination struct {
			Server    string `json:"server"`
			Namespace string `json:"namespace"`
		} `json:"destination"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status    string   `json:"status"`
			Revision  string   `json:"revision"`
			Revisions []string `json:"revisions"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		OperationState struct {
			Phase      string `json:"phase"`
			SyncResult struct {
				Revision  string   `json:"revision"`
				Revisions []string `json:"revisions"`
			} `json:"syncResult"`
		} `json:"operationState"`
		ReconciledAt time.Time `json:"reconciledAt"`
	} `json:"status"`
}

func decodeKubernetesApplicationPage(body []byte, namespace string) (KubernetesApplicationPage, error) {
	if len(body) == 0 || int64(len(body)) > maximumArgoResponseBytes || !kubeRE.MatchString(namespace) {
		return KubernetesApplicationPage{}, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var wire applicationListWire
	if err := decoder.Decode(&wire); err != nil {
		return KubernetesApplicationPage{}, ErrInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return KubernetesApplicationPage{}, ErrInvalid
	}
	if len(wire.Items) > 500 {
		return KubernetesApplicationPage{}, errors.New("Kubernetes Applications page exceeded its bound")
	}
	page := KubernetesApplicationPage{ResourceVersion: wire.Metadata.ResourceVersion, Continue: wire.Metadata.Continue, Applications: make([]KubernetesApplication, 0, len(wire.Items))}
	for _, item := range wire.Items {
		if item.Metadata.Namespace != namespace || item.Metadata.Labels["app.kubernetes.io/managed-by"] != "kuberploy" {
			return KubernetesApplicationPage{}, ErrInvalid
		}
		revisions := make([]string, 0, 2+len(item.Status.Sync.Revisions)+len(item.Status.OperationState.SyncResult.Revisions))
		revisions = append(revisions, item.Status.Sync.Revision)
		revisions = append(revisions, item.Status.Sync.Revisions...)
		revisions = append(revisions, item.Status.OperationState.SyncResult.Revision)
		revisions = append(revisions, item.Status.OperationState.SyncResult.Revisions...)
		page.Applications = append(page.Applications, KubernetesApplication{
			UID: item.Metadata.UID, Namespace: item.Metadata.Namespace, Name: item.Metadata.Name,
			ResourceVersion: item.Metadata.ResourceVersion, Labels: cloneStringMap(item.Metadata.Labels),
			Project: item.Spec.Project, DestinationServer: item.Spec.Destination.Server, DestinationNamespace: item.Spec.Destination.Namespace,
			SyncStatus: item.Status.Sync.Status, SyncRevisions: revisions, HealthStatus: item.Status.Health.Status,
			OperationPhase: item.Status.OperationState.Phase, ReconciledAt: item.Status.ReconciledAt,
		})
	}
	return page, nil
}

func cloneStringMap(value map[string]string) map[string]string {
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}
