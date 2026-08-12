package edge

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
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	edgeServiceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"
	edgeKubernetesMaxJSON       = int64(2 << 20)
)

// InClusterKubernetesReader exposes only the six GET operations required by
// edge readiness. It has no generic path, list, watch, proxy, redirect,
// subresource, Secret, exec, or mutation surface.
type InClusterKubernetesReader struct {
	baseURL   string
	http      *http.Client
	tokenPath string
}

func NewInClusterKubernetesReader() (*InClusterKubernetesReader, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	portNumber, portErr := strconv.Atoi(port)
	validHost := net.ParseIP(host) != nil || validDNSName(host)
	if !validHost || portErr != nil || portNumber < 1 || portNumber > 65535 || strconv.Itoa(portNumber) != port {
		return nil, errors.New("Kubernetes in-cluster service environment is missing")
	}
	caPEM, err := os.ReadFile(edgeServiceAccountDirectory + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes service CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("Kubernetes service CA is invalid")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second, KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       time.Minute,
	}
	return &InClusterKubernetesReader{
		baseURL: "https://" + net.JoinHostPort(host, port),
		http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Kubernetes API redirects are not allowed")
		}},
		tokenPath: edgeServiceAccountDirectory + "/token",
	}, nil
}

func (c *InClusterKubernetesReader) Deployment(ctx context.Context, namespace, name, containerName string) (DeploymentSnapshot, error) {
	if !dnsLabelPattern.MatchString(namespace) || !validObjectName(name) || !dnsLabelPattern.MatchString(containerName) {
		return DeploymentSnapshot{}, ErrInvalid
	}
	var object deploymentObject
	if err := c.getJSON(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(namespace)+"/deployments/"+url.PathEscape(name), &object); err != nil {
		return DeploymentSnapshot{}, err
	}
	if object.Metadata.Name != name || object.Metadata.Namespace != namespace {
		return DeploymentSnapshot{}, ErrConflict
	}
	var spec deploymentSpec
	if err := decodeRaw(object.Spec, &spec); err != nil || spec.Replicas == nil || *spec.Replicas < 1 {
		return DeploymentSnapshot{}, ErrConflict
	}
	selected := deploymentContainer{}
	found := false
	for _, container := range spec.Template.Spec.Containers {
		if container.Name != containerName {
			continue
		}
		if found {
			return DeploymentSnapshot{}, ErrConflict
		}
		selected, found = container, true
	}
	if !found {
		return DeploymentSnapshot{}, ErrNotFound
	}
	specDigest := canonicalJSONDigest(object.Spec)
	if annotated := object.Metadata.Annotations["kuberploy.io/edge-spec-digest"]; validDigest(annotated) {
		specDigest = annotated
	}
	return DeploymentSnapshot{
		ObjectSnapshot: ObjectSnapshot{Name: name, Namespace: namespace, UID: object.Metadata.UID, ResourceVersion: object.Metadata.ResourceVersion,
			Generation: object.Metadata.Generation, SpecDigest: specDigest},
		ObservedGeneration: object.Status.ObservedGeneration, Version: observedDeploymentVersion(object.Metadata.Labels, selected.Image),
		DesiredReplicas: *spec.Replicas, AvailableReplicas: object.Status.AvailableReplicas,
		ContainerName: selected.Name, ContainerImage: selected.Image, ContainerArguments: append([]string(nil), selected.Args...),
		ContainerSecretRefs:    deploymentSecretRefs(selected.Env, selected.EnvFrom),
		ContainerConfigMapRefs: deploymentConfigMapRefs(selected.EnvFrom),
	}, nil
}

func observedDeploymentVersion(labels map[string]string, image string) string {
	if version := labels["app.kubernetes.io/version"]; versionPattern.MatchString(version) {
		return version
	}
	separator := strings.LastIndexByte(image, ':')
	lastSlash := strings.LastIndexByte(image, '/')
	if separator <= lastSlash || separator == len(image)-1 {
		return ""
	}
	version := image[separator+1:]
	if versionPattern.MatchString(version) {
		return version
	}
	return ""
}

func deploymentSecretRefs(values []deploymentEnv, from []deploymentEnvFrom) []string {
	refs := make([]string, 0, len(values))
	for _, value := range values {
		if value.ValueFrom.SecretKeyRef.Name != "" {
			refs = append(refs, value.ValueFrom.SecretKeyRef.Name)
		}
	}
	for _, value := range from {
		if value.SecretRef.Name != "" {
			refs = append(refs, value.SecretRef.Name)
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

func deploymentConfigMapRefs(from []deploymentEnvFrom) []string {
	refs := make([]string, 0, len(from))
	for _, value := range from {
		if value.ConfigMapRef.Name != "" {
			refs = append(refs, value.ConfigMapRef.Name)
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

func (c *InClusterKubernetesReader) Service(ctx context.Context, namespace, name string) (ServiceSnapshot, error) {
	if !dnsLabelPattern.MatchString(namespace) || !validObjectName(name) {
		return ServiceSnapshot{}, ErrInvalid
	}
	var object serviceObject
	if err := c.getJSON(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/services/"+url.PathEscape(name), &object); err != nil {
		return ServiceSnapshot{}, err
	}
	if object.Metadata.Name != name || object.Metadata.Namespace != namespace {
		return ServiceSnapshot{}, ErrConflict
	}
	var spec struct {
		Type string `json:"type"`
	}
	if err := decodeRaw(object.Spec, &spec); err != nil {
		return ServiceSnapshot{}, ErrConflict
	}
	normalizedSpec, err := normalizeServiceSpec(object.Spec)
	if err != nil {
		return ServiceSnapshot{}, ErrConflict
	}
	if len(object.Status.LoadBalancer.Ingress) > 16 {
		return ServiceSnapshot{}, ErrConflict
	}
	ingresses := make([]LoadBalancerIngress, 0, len(object.Status.LoadBalancer.Ingress))
	for _, raw := range object.Status.LoadBalancer.Ingress {
		var ingress struct {
			IP       string `json:"ip"`
			Hostname string `json:"hostname"`
		}
		if err := decodeRaw(raw, &ingress); err != nil {
			return ServiceSnapshot{}, ErrConflict
		}
		value := LoadBalancerIngress{IP: ingress.IP, Hostname: strings.ToLower(strings.TrimSuffix(ingress.Hostname, "."))}
		if value.validate() != nil {
			return ServiceSnapshot{}, ErrConflict
		}
		ingresses = append(ingresses, value)
	}
	return ServiceSnapshot{ObjectSnapshot: ObjectSnapshot{Name: name, Namespace: namespace, UID: object.Metadata.UID,
		ResourceVersion: object.Metadata.ResourceVersion, Generation: object.Metadata.Generation, SpecDigest: canonicalJSONDigest(normalizedSpec)},
		Type: spec.Type, LoadBalancerReady: len(ingresses) > 0, LoadBalancerIngress: ingresses}, nil
}

// normalizeServiceSpec removes only Kubernetes-allocated fields and known
// server defaults. The profile digest is produced from the Helm-rendered
// Service spec before admission, so cluster IPs and NodePorts can never be
// stable inputs. Non-default operator choices remain part of the digest.
func normalizeServiceSpec(raw json.RawMessage) ([]byte, error) {
	var spec map[string]any
	if err := decodeRaw(raw, &spec); err != nil {
		return nil, err
	}
	for _, key := range []string{"clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy"} {
		delete(spec, key)
	}
	defaults := map[string]any{
		"allocateLoadBalancerNodePorts": true,
		"externalTrafficPolicy":         "Cluster",
		"internalTrafficPolicy":         "Cluster",
		"sessionAffinity":               "None",
	}
	for key, value := range defaults {
		if current, ok := spec[key]; ok && current == value {
			delete(spec, key)
		}
	}
	ports, ok := spec["ports"].([]any)
	if !ok || len(ports) == 0 {
		return nil, ErrConflict
	}
	for _, rawPort := range ports {
		port, ok := rawPort.(map[string]any)
		if !ok {
			return nil, ErrConflict
		}
		delete(port, "nodePort")
	}
	return json.Marshal(spec)
}

func (c *InClusterKubernetesReader) IngressClass(ctx context.Context, name string) (ObjectSnapshot, error) {
	if !validObjectName(name) {
		return ObjectSnapshot{}, ErrInvalid
	}
	var object genericSpecObject
	if err := c.getJSON(ctx, "/apis/networking.k8s.io/v1/ingressclasses/"+url.PathEscape(name), &object); err != nil {
		return ObjectSnapshot{}, err
	}
	return decodeGenericSpec(object, name, "")
}

func (c *InClusterKubernetesReader) CustomResourceDefinition(ctx context.Context, name string) (ObjectSnapshot, error) {
	if !validObjectName(name) {
		return ObjectSnapshot{}, ErrInvalid
	}
	var object genericSpecObject
	if err := c.getJSON(ctx, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"+url.PathEscape(name), &object); err != nil {
		return ObjectSnapshot{}, err
	}
	return decodeGenericSpec(object, name, "")
}

func (c *InClusterKubernetesReader) ConfigMap(ctx context.Context, namespace, name string) (ConfigMapSnapshot, error) {
	if !dnsLabelPattern.MatchString(namespace) || !validObjectName(name) {
		return ConfigMapSnapshot{}, ErrInvalid
	}
	var object configMapObject
	if err := c.getJSON(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/configmaps/"+url.PathEscape(name), &object); err != nil {
		return ConfigMapSnapshot{}, err
	}
	if object.Metadata.Name != name || object.Metadata.Namespace != namespace || len(object.Data) > 64 || len(object.BinaryData) > 0 {
		return ConfigMapSnapshot{}, ErrConflict
	}
	for key, value := range object.Data {
		if len(key) == 0 || len(key) > 253 || len(value) > 4096 || strings.ContainsAny(key, "\x00\r\n") {
			return ConfigMapSnapshot{}, ErrConflict
		}
	}
	return ConfigMapSnapshot{ObjectSnapshot: ObjectSnapshot{Name: name, Namespace: namespace, UID: object.Metadata.UID,
		ResourceVersion: object.Metadata.ResourceVersion, Generation: object.Metadata.Generation, SpecDigest: digestStringMap(object.Data)},
		Data: cloneStringMap(object.Data), BinaryData: false}, nil
}

func (c *InClusterKubernetesReader) NetworkPolicy(ctx context.Context, namespace, name string) (ObjectSnapshot, error) {
	if !dnsLabelPattern.MatchString(namespace) || !validObjectName(name) {
		return ObjectSnapshot{}, ErrInvalid
	}
	var object struct {
		Metadata objectMetadata  `json:"metadata"`
		Spec     json.RawMessage `json:"spec"`
	}
	if err := c.getJSON(ctx, "/apis/networking.k8s.io/v1/namespaces/"+url.PathEscape(namespace)+"/networkpolicies/"+url.PathEscape(name), &object); err != nil {
		return ObjectSnapshot{}, err
	}
	if object.Metadata.Name != name || object.Metadata.Namespace != namespace {
		return ObjectSnapshot{}, ErrConflict
	}
	return ObjectSnapshot{Name: name, Namespace: namespace, UID: object.Metadata.UID, ResourceVersion: object.Metadata.ResourceVersion, Generation: object.Metadata.Generation, SpecDigest: canonicalJSONDigest(object.Spec)}, nil
}

func (c *InClusterKubernetesReader) ClusterIssuer(ctx context.Context, name string) (IssuerSnapshot, error) {
	if !validObjectName(name) {
		return IssuerSnapshot{}, ErrInvalid
	}
	var object issuerObject
	if err := c.getJSON(ctx, "/apis/cert-manager.io/v1/clusterissuers/"+url.PathEscape(name), &object); err != nil {
		return IssuerSnapshot{}, err
	}
	if object.Metadata.Name != name || object.Metadata.Namespace != "" {
		return IssuerSnapshot{}, ErrConflict
	}
	ready, observedGeneration, foundReady := false, int64(0), false
	for _, condition := range object.Status.Conditions {
		if condition.Type != "Ready" {
			continue
		}
		if foundReady {
			return IssuerSnapshot{}, ErrConflict
		}
		foundReady = true
		ready, observedGeneration = condition.Status == "True", condition.ObservedGeneration
	}
	return IssuerSnapshot{Name: name, UID: object.Metadata.UID, ResourceVersion: object.Metadata.ResourceVersion,
		Generation: object.Metadata.Generation, ObservedGeneration: observedGeneration, Ready: ready}, nil
}

func (c *InClusterKubernetesReader) getJSON(ctx context.Context, path string, destination any) error {
	if c == nil || c.http == nil || !strings.HasPrefix(c.baseURL, "https://") ||
		!validEdgeKubernetesPath(path) {
		return ErrInvalid
	}
	tokenBytes, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return ErrUnavailable
	}
	defer clearBytes(tokenBytes)
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || len(token) > 32<<10 || strings.IndexFunc(token, func(value rune) bool { return value < 0x21 || value == 0x7f }) >= 0 {
		return ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return ErrInvalid
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%w: Kubernetes API request failed", ErrUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, edgeKubernetesMaxJSON+1))
	if err != nil {
		return ErrUnavailable
	}
	defer clearBytes(body)
	if int64(len(body)) > edgeKubernetesMaxJSON {
		return ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err = decoder.Decode(destination); err != nil {
		return ErrConflict
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		return ErrConflict
	}
	return nil
}

func validEdgeKubernetesPath(path string) bool {
	if strings.ContainsAny(path, "?\x00\r\n") || !strings.HasPrefix(path, "/") {
		return false
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case len(segments) == 7 && segments[0] == "apis" && segments[1] == "apps" && segments[2] == "v1" &&
		segments[3] == "namespaces" && segments[5] == "deployments":
		return dnsLabelPattern.MatchString(segments[4]) && validObjectName(segments[6])
	case len(segments) == 6 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "namespaces" &&
		(segments[4] == "services" || segments[4] == "configmaps"):
		return dnsLabelPattern.MatchString(segments[3]) && validObjectName(segments[5])
	case len(segments) == 5 && segments[0] == "apis" && segments[1] == "networking.k8s.io" &&
		segments[2] == "v1" && segments[3] == "ingressclasses":
		return validObjectName(segments[4])
	case len(segments) == 7 && segments[0] == "apis" && segments[1] == "networking.k8s.io" && segments[2] == "v1" && segments[3] == "namespaces" && segments[5] == "networkpolicies":
		return dnsLabelPattern.MatchString(segments[4]) && validObjectName(segments[6])
	case len(segments) == 5 && segments[0] == "apis" && segments[1] == "apiextensions.k8s.io" &&
		segments[2] == "v1" && segments[3] == "customresourcedefinitions":
		return validObjectName(segments[4])
	case len(segments) == 5 && segments[0] == "apis" && segments[1] == "cert-manager.io" &&
		segments[2] == "v1" && segments[3] == "clusterissuers":
		return validObjectName(segments[4])
	default:
		return false
	}
}

type objectMetadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	UID             string            `json:"uid"`
	ResourceVersion string            `json:"resourceVersion"`
	Generation      int64             `json:"generation"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
}

type deploymentObject struct {
	Metadata objectMetadata  `json:"metadata"`
	Spec     json.RawMessage `json:"spec"`
	Status   struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		AvailableReplicas  int32 `json:"availableReplicas"`
	} `json:"status"`
}

type deploymentSpec struct {
	Replicas *int32 `json:"replicas"`
	Template struct {
		Spec struct {
			Containers []deploymentContainer `json:"containers"`
		} `json:"spec"`
	} `json:"template"`
}

type deploymentContainer struct {
	Name    string              `json:"name"`
	Image   string              `json:"image"`
	Args    []string            `json:"args"`
	Env     []deploymentEnv     `json:"env"`
	EnvFrom []deploymentEnvFrom `json:"envFrom"`
}

type deploymentEnvFrom struct {
	SecretRef struct {
		Name string `json:"name"`
	} `json:"secretRef"`
	ConfigMapRef struct {
		Name string `json:"name"`
	} `json:"configMapRef"`
}

type deploymentEnv struct {
	ValueFrom struct {
		SecretKeyRef struct {
			Name string `json:"name"`
		} `json:"secretKeyRef"`
	} `json:"valueFrom"`
}

type serviceObject struct {
	Metadata objectMetadata  `json:"metadata"`
	Spec     json.RawMessage `json:"spec"`
	Status   struct {
		LoadBalancer struct {
			Ingress []json.RawMessage `json:"ingress"`
		} `json:"loadBalancer"`
	} `json:"status"`
}

type genericSpecObject struct {
	Metadata objectMetadata  `json:"metadata"`
	Spec     json.RawMessage `json:"spec"`
}

type configMapObject struct {
	Metadata   objectMetadata    `json:"metadata"`
	Data       map[string]string `json:"data"`
	BinaryData map[string]string `json:"binaryData"`
}

type issuerObject struct {
	Metadata objectMetadata `json:"metadata"`
	Status   struct {
		Conditions []struct {
			Type               string `json:"type"`
			Status             string `json:"status"`
			ObservedGeneration int64  `json:"observedGeneration"`
		} `json:"conditions"`
	} `json:"status"`
}

func decodeGenericSpec(object genericSpecObject, name, namespace string) (ObjectSnapshot, error) {
	if object.Metadata.Name != name || object.Metadata.Namespace != namespace || len(object.Spec) == 0 {
		return ObjectSnapshot{}, ErrConflict
	}
	return ObjectSnapshot{Name: name, Namespace: namespace, UID: object.Metadata.UID, ResourceVersion: object.Metadata.ResourceVersion,
		Generation: object.Metadata.Generation, SpecDigest: canonicalJSONDigest(object.Spec)}, nil
}

func decodeRaw(raw json.RawMessage, destination any) error {
	if len(raw) == 0 || len(raw) > int(edgeKubernetesMaxJSON) {
		return ErrConflict
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return ErrConflict
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrConflict
	}
	return nil
}

func canonicalJSONDigest(raw json.RawMessage) string {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return digestBytes(encoded)
}

func cloneStringMap(value map[string]string) map[string]string {
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var _ KubernetesReader = (*InClusterKubernetesReader)(nil)
