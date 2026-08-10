package observability

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
	"os"
	"strconv"
	"strings"
	"time"
)

const managedServiceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"

var managedKubernetesPaths = []string{
	"/api/v1/namespaces/kuberploy-monitoring/configmaps/monitoring-monitoring-profile",
	"/apis/apps/v1/namespaces/kuberploy-monitoring/deployments/kuberploy-prometheus-operator",
	"/apis/monitoring.coreos.com/v1/namespaces/kuberploy-monitoring/prometheusrules/monitoring-service-recording-rules",
}

// InClusterManagedMonitoringObserver has three fixed read-only Kubernetes API
// paths. It exposes no generic path, list, watch, proxy, Secret, or mutation
// surface.
type InClusterManagedMonitoringObserver struct {
	baseURL   string
	http      *http.Client
	tokenPath string
}

func NewInClusterManagedMonitoringObserver() (*InClusterManagedMonitoringObserver, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	portNumber, portErr := strconv.Atoi(port)
	if net.ParseIP(host) == nil || portErr != nil || portNumber < 1 || portNumber > 65535 || strconv.Itoa(portNumber) != port {
		return nil, errors.New("Kubernetes in-cluster service environment is missing")
	}
	caPEM, err := os.ReadFile(managedServiceAccountDirectory + "/ca.crt")
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
		IdleConnTimeout:       time.Minute,
	}
	return &InClusterManagedMonitoringObserver{
		baseURL: "https://" + net.JoinHostPort(host, port),
		http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Kubernetes API redirects are not allowed")
		}},
		tokenPath: managedServiceAccountDirectory + "/token",
	}, nil
}

func (c *InClusterManagedMonitoringObserver) ObserveManagedMonitoring(ctx context.Context) (ManagedMonitoringSnapshot, error) {
	var profile struct {
		Metadata   managedObjectMetadata `json:"metadata"`
		Immutable  *bool                 `json:"immutable"`
		Data       map[string]string     `json:"data"`
		BinaryData map[string]string     `json:"binaryData"`
	}
	if err := c.getJSON(ctx, managedKubernetesPaths[0], &profile); err != nil {
		return ManagedMonitoringSnapshot{}, err
	}
	var operator struct {
		Metadata managedObjectMetadata `json:"metadata"`
		Spec     struct {
			Replicas *int32 `json:"replicas"`
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string   `json:"name"`
						Image string   `json:"image"`
						Args  []string `json:"args"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64 `json:"observedGeneration"`
			AvailableReplicas  int32 `json:"availableReplicas"`
		} `json:"status"`
	}
	if err := c.getJSON(ctx, managedKubernetesPaths[1], &operator); err != nil {
		return ManagedMonitoringSnapshot{}, err
	}
	var rule struct {
		Metadata managedObjectMetadata `json:"metadata"`
		Spec     json.RawMessage       `json:"spec"`
	}
	if err := c.getJSON(ctx, managedKubernetesPaths[2], &rule); err != nil {
		return ManagedMonitoringSnapshot{}, err
	}
	if !validManagedMetadata(profile.Metadata, ManagedMonitoringProfileName) || profile.Immutable == nil || len(profile.BinaryData) != 0 || len(profile.Data) > 32 ||
		!validManagedMetadata(operator.Metadata, ManagedMonitoringOperatorName) || operator.Spec.Replicas == nil || len(operator.Spec.Template.Spec.Containers) > 16 ||
		!validManagedMetadata(rule.Metadata, ManagedMonitoringRuleName) {
		return ManagedMonitoringSnapshot{}, ErrUnsafeResponse
	}
	var selectedName, selectedImage string
	var selectedArgs []string
	selected := 0
	for _, container := range operator.Spec.Template.Spec.Containers {
		if container.Name == ManagedMonitoringOperatorContainer {
			selected++
			selectedName, selectedImage, selectedArgs = container.Name, container.Image, append([]string(nil), container.Args...)
		}
	}
	if selected != 1 {
		return ManagedMonitoringSnapshot{}, ErrUnsafeResponse
	}
	data := make(map[string]string, len(profile.Data))
	for key, value := range profile.Data {
		if key == "" || len(key) > 253 || len(value) > 4096 || strings.ContainsAny(key+value, "\x00\r") {
			return ManagedMonitoringSnapshot{}, ErrUnsafeResponse
		}
		data[key] = value
	}
	return ManagedMonitoringSnapshot{
		ProfileData: data, ProfileImmutable: *profile.Immutable,
		OperatorName: operator.Metadata.Name, OperatorContainer: selectedName, OperatorImage: selectedImage, OperatorArgumentsSHA256: digestJSON(selectedArgs),
		OperatorGeneration: operator.Metadata.Generation, OperatorObservedGeneration: operator.Status.ObservedGeneration,
		OperatorDesiredReplicas: *operator.Spec.Replicas, OperatorAvailableReplicas: operator.Status.AvailableReplicas,
		RuleName: rule.Metadata.Name, RuleGeneration: rule.Metadata.Generation, RuleSpecSHA256: canonicalRawJSONDigest(rule.Spec),
	}, nil
}

type managedObjectMetadata struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	Generation      int64  `json:"generation"`
}

func validManagedMetadata(metadata managedObjectMetadata, name string) bool {
	return metadata.Name == name && metadata.Namespace == ManagedMonitoringNamespace && metadata.UID != "" && metadata.ResourceVersion != ""
}

func (c *InClusterManagedMonitoringObserver) getJSON(ctx context.Context, path string, destination any) error {
	if c == nil || c.http == nil || !strings.HasPrefix(c.baseURL, "https://") || !managedPathAllowed(path) {
		return ErrUnavailable
	}
	tokenBytes, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return ErrUnavailable
	}
	defer erase(tokenBytes)
	token := strings.TrimSpace(string(tokenBytes))
	if !validBearerToken([]byte(token)) {
		return ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return ErrUnavailable
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.http.Do(request)
	if err != nil {
		return ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	if contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])); contentType != "application/json" {
		return ErrUnsafeResponse
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, defaultMaxBody+1))
	if err != nil || int64(len(body)) > defaultMaxBody {
		return ErrUnsafeResponse
	}
	defer erase(body)
	decoder := json.NewDecoder(bytes.NewReader(body))
	if decoder.Decode(destination) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return ErrUnsafeResponse
	}
	return nil
}

func managedPathAllowed(path string) bool {
	for _, allowed := range managedKubernetesPaths {
		if path == allowed {
			return true
		}
	}
	return false
}

var _ ManagedMonitoringObserver = (*InClusterManagedMonitoringObserver)(nil)
