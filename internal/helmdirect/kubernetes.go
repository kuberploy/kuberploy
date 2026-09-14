package helmdirect

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

const serviceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"

type InClusterApplicationAPI struct {
	baseURL, tokenPath string
	http               *http.Client
}

func NewInClusterApplicationAPI() (*InClusterApplicationAPI, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes in-cluster service environment is missing")
	}
	ca, err := os.ReadFile(serviceAccountDirectory + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes service CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("Kubernetes service CA is invalid")
	}
	transport := &http.Transport{Proxy: nil,
		DialContext:     (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second, IdleConnTimeout: 60 * time.Second}
	return &InClusterApplicationAPI{baseURL: "https://" + net.JoinHostPort(host, port),
		tokenPath: serviceAccountDirectory + "/token", http: &http.Client{Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("Kubernetes API redirects are not allowed")
			}}}, nil
}

func (c *InClusterApplicationAPI) Apply(ctx context.Context, namespace, name string, manifest []byte) error {
	if c == nil || c.http == nil || !dnsLabelRE.MatchString(namespace) || !strings.HasPrefix(name, "kp-h-") || len(manifest) == 0 || len(manifest) > 512<<10 {
		return ErrInvalid
	}
	query := url.Values{"fieldManager": {"kuberploy-helm-apps"}, "force": {"true"}}
	response, err := c.request(ctx, http.MethodPatch, c.applicationPath(namespace, name)+"?"+query.Encode(), manifest, "application/apply-patch+yaml")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return fmt.Errorf("Kubernetes Argo Application apply returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (c *InClusterApplicationAPI) Delete(ctx context.Context, namespace, name string) error {
	if c == nil || c.http == nil || !dnsLabelRE.MatchString(namespace) || !strings.HasPrefix(name, "kp-h-") {
		return ErrInvalid
	}
	body := []byte(`{"apiVersion":"v1","kind":"DeleteOptions","propagationPolicy":"Foreground"}`)
	response, err := c.request(ctx, http.MethodDelete, c.applicationPath(namespace, name), body, "application/json")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("Kubernetes Argo Application delete returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (c *InClusterApplicationAPI) Observe(ctx context.Context, namespace, name string) (ApplicationState, error) {
	if c == nil || c.http == nil || !dnsLabelRE.MatchString(namespace) || !strings.HasPrefix(name, "kp-h-") {
		return ApplicationState{}, ErrInvalid
	}
	response, err := c.request(ctx, http.MethodGet, c.applicationPath(namespace, name), nil, "application/json")
	if err != nil {
		return ApplicationState{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return ApplicationState{}, nil
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return ApplicationState{}, fmt.Errorf("Kubernetes Argo Application read returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Metadata struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Project string `json:"project"`
		} `json:"spec"`
		Status struct {
			Sync struct {
				Status     string `json:"status"`
				ComparedTo struct {
					Source      map[string]any `json:"source"`
					Destination map[string]any `json:"destination"`
				} `json:"comparedTo"`
			} `json:"sync"`
			Health struct {
				Status string `json:"status"`
			} `json:"health"`
			OperationState struct {
				Phase string `json:"phase"`
			} `json:"operationState"`
			Conditions []struct {
				Type string `json:"type"`
			} `json:"conditions"`
			ReconciledAt string `json:"reconciledAt"`
		} `json:"status"`
	}
	limited := io.LimitReader(response.Body, 1<<20)
	if err = json.NewDecoder(limited).Decode(&payload); err != nil {
		return ApplicationState{}, fmt.Errorf("decode Kubernetes Argo Application: %w", err)
	}
	generation, _ := strconv.ParseInt(payload.Metadata.Annotations["kuberploy.io/helm-generation"], 10, 64)
	state := ApplicationState{
		Exists:              true,
		Generation:          generation,
		EnvironmentID:       payload.Metadata.Labels["kuberploy.io/environment-id"],
		ApplicationID:       payload.Metadata.Labels["kuberploy.io/application-id"],
		ProjectID:           payload.Metadata.Labels["kuberploy.io/project-id"],
		ArgoProject:         payload.Spec.Project,
		ObservedSource:      payload.Status.Sync.ComparedTo.Source,
		ObservedDestination: payload.Status.Sync.ComparedTo.Destination,
		Sync:                payload.Status.Sync.Status,
		Health:              payload.Status.Health.Status,
		Operation:           payload.Status.OperationState.Phase,
	}
	if reconciledAt, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(payload.Status.ReconciledAt)); parseErr == nil {
		state.ReconciledAt = &reconciledAt
	}
	for _, condition := range payload.Status.Conditions {
		switch condition.Type {
		case "InvalidSpecError", "ComparisonError", "SyncError", "UnknownError":
			state.ConditionType = condition.Type
			return state, nil
		}
	}
	return state, nil
}

func (c *InClusterApplicationAPI) applicationPath(namespace, name string) string {
	return "/apis/argoproj.io/v1alpha1/namespaces/" + url.PathEscape(namespace) + "/applications/" + url.PathEscape(name)
}

func (c *InClusterApplicationAPI) request(ctx context.Context, method, path string, body []byte, contentType string) (*http.Response, error) {
	if ctx == nil || !strings.HasPrefix(path, "/apis/argoproj.io/v1alpha1/namespaces/") ||
		!strings.Contains(path, "/applications/kp-h-") || strings.ContainsAny(path, "\x00\r\n") {
		return nil, ErrInvalid
	}
	tokenBytes, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes service token: %w", err)
	}
	defer clear(tokenBytes)
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || len(token) > 16<<10 || strings.ContainsAny(token, "\x00\r\n") {
		return nil, errors.New("Kubernetes service token is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "application/json")
	return c.http.Do(request)
}
