package certissuers

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
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	issuerServiceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"
	issuerKubernetesMaxJSON       = int64(1 << 20)
	issuerDigestAnnotation        = "kuberploy.io/certificate-issuer-spec-digest"
	issuerRevisionAnnotation      = "kuberploy.io/certificate-issuer-revision"
	issuerManagedByLabel          = "app.kubernetes.io/managed-by"
	issuerProfileLabel            = "kuberploy.io/certificate-issuer-profile"
	http01ExternalDNSAnnotation   = "external-dns.alpha.kubernetes.io/ingress-hostname-source"
	http01ExternalDNSExclude      = "external-dns.alpha.kubernetes.io/exclude"
)

type ClusterIssuerSnapshot struct {
	Name, UID, ResourceVersion, AnnotatedSpecDigest string
	AnnotatedRevision                               int64
	Generation, ReadyObservedGeneration             int64
	Ready                                           bool
	SpecDigest                                      string
	Solver                                          SolverType
	Spec                                            Spec
}

type ClusterIssuerReader interface {
	ClusterIssuer(context.Context, string) (ClusterIssuerSnapshot, error)
}

// InClusterClusterIssuerReader has one exact named GET operation. The trusted
// controller supplies names from the bounded catalog and Kubernetes separately
// enforces per-issuer resourceNames RBAC. This type has no generic path, list,
// watch, proxy, redirect, subresource, Secret, exec, or mutation surface.
type InClusterClusterIssuerReader struct {
	baseURL   string
	http      *http.Client
	tokenPath string
}

func NewInClusterClusterIssuerReader(config ObserverConfig) (*InClusterClusterIssuerReader, error) {
	if config.Validate() != nil || !config.Enabled {
		return nil, ErrObservationUnavailable
	}
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	portNumber, portErr := strconv.Atoi(port)
	if (net.ParseIP(host) == nil && !validIssuerDNSName(host)) || portErr != nil || portNumber < 1 || portNumber > 65535 || strconv.Itoa(portNumber) != port {
		return nil, ErrObservationUnavailable
	}
	caPEM, err := os.ReadFile(issuerServiceAccountDirectory + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("%w: read Kubernetes service CA", ErrObservationUnavailable)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, ErrObservationUnavailable
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: config.RequestTimeout, IdleConnTimeout: time.Minute}
	client := &http.Client{Transport: transport, Timeout: config.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("Kubernetes API redirects are not allowed")
	}}
	return newClusterIssuerReader("https://"+net.JoinHostPort(host, port), client, issuerServiceAccountDirectory+"/token")
}

func newClusterIssuerReader(baseURL string, client *http.Client, tokenPath string) (*InClusterClusterIssuerReader, error) {
	if !strings.HasPrefix(baseURL, "https://") || client == nil || tokenPath == "" {
		return nil, ErrObservationUnavailable
	}
	closedClient := *client
	closedClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("Kubernetes API redirects are not allowed")
	}
	return &InClusterClusterIssuerReader{baseURL: strings.TrimSuffix(baseURL, "/"), http: &closedClient, tokenPath: tokenPath}, nil
}

func (c *InClusterClusterIssuerReader) ClusterIssuer(ctx context.Context, name string) (ClusterIssuerSnapshot, error) {
	if ctx == nil || c == nil || c.http == nil || !dnsLabelRE.MatchString(name) {
		return ClusterIssuerSnapshot{}, ErrObservationUnavailable
	}
	path := "/apis/cert-manager.io/v1/clusterissuers/" + name
	if !validClusterIssuerPath(path) {
		return ClusterIssuerSnapshot{}, ErrObservationUnavailable
	}
	var object liveClusterIssuer
	if err := c.getJSON(ctx, path, &object); err != nil {
		return ClusterIssuerSnapshot{}, err
	}
	return object.snapshot(name)
}

func (c *InClusterClusterIssuerReader) getJSON(ctx context.Context, path string, destination any) error {
	if ctx == nil || c == nil || c.http == nil || !strings.HasPrefix(c.baseURL, "https://") || !validClusterIssuerPath(path) {
		return ErrObservationUnavailable
	}
	tokenBytes, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return ErrObservationUnavailable
	}
	defer zeroBytes(tokenBytes)
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || len(token) > 32<<10 || strings.IndexFunc(token, func(value rune) bool { return value < 0x21 || value == 0x7f }) >= 0 {
		return ErrObservationUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return ErrObservationUnavailable
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.http.Do(request)
	if err != nil {
		return ErrObservationUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrObservationUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, issuerKubernetesMaxJSON+1))
	if err != nil || int64(len(body)) > issuerKubernetesMaxJSON {
		zeroBytes(body)
		return ErrObservationUnavailable
	}
	defer zeroBytes(body)
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err = decoder.Decode(destination); err != nil {
		return ErrObservationUnavailable
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrObservationUnavailable
	}
	return nil
}

func validClusterIssuerPath(path string) bool {
	if strings.ContainsAny(path, "?\x00\r\n") || !strings.HasPrefix(path, "/") {
		return false
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segments) != 5 || segments[0] != "apis" || segments[1] != "cert-manager.io" || segments[2] != "v1" || segments[3] != "clusterissuers" || !dnsLabelRE.MatchString(segments[4]) {
		return false
	}
	return true
}

type liveClusterIssuer struct {
	APIVersion string                    `json:"apiVersion"`
	Kind       string                    `json:"kind"`
	Metadata   liveClusterIssuerMetadata `json:"metadata"`
	Spec       json.RawMessage           `json:"spec"`
	Status     liveClusterIssuerStatus   `json:"status"`
}

type liveClusterIssuerMetadata struct {
	Name, Namespace, UID, ResourceVersion string
	Generation                            int64
	Annotations, Labels                   map[string]string
}

func (m *liveClusterIssuerMetadata) UnmarshalJSON(raw []byte) error {
	var value struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		UID             string            `json:"uid"`
		ResourceVersion string            `json:"resourceVersion"`
		Generation      int64             `json:"generation"`
		Annotations     map[string]string `json:"annotations"`
		Labels          map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*m = liveClusterIssuerMetadata(value)
	return nil
}

type liveClusterIssuerStatus struct {
	Conditions []liveClusterIssuerCondition `json:"conditions"`
	ACME       json.RawMessage              `json:"acme,omitempty"`
}

type liveClusterIssuerCondition struct {
	Type, Status, Reason, Message, LastTransitionTime string
	ObservedGeneration                                int64
}

type liveIssuerSpec struct {
	ACME liveACMESpec `json:"acme"`
}

type liveACMESpec struct {
	Email               string                  `json:"email"`
	Server              string                  `json:"server"`
	PrivateKeySecretRef liveSecretNameReference `json:"privateKeySecretRef"`
	Solvers             []liveACMESolver        `json:"solvers"`
}

type liveSecretNameReference struct {
	Name string `json:"name"`
}

type liveACMESolver struct {
	Selector *liveSelector `json:"selector,omitempty"`
	HTTP01   *liveHTTP01   `json:"http01,omitempty"`
	DNS01    *liveDNS01    `json:"dns01,omitempty"`
}

type liveSelector struct {
	DNSZones []string `json:"dnsZones,omitempty"`
}

type liveHTTP01 struct {
	Ingress liveHTTP01Ingress `json:"ingress"`
}

type liveHTTP01Ingress struct {
	IngressClassName string              `json:"ingressClassName"`
	IngressTemplate  liveIngressTemplate `json:"ingressTemplate"`
}

type liveIngressTemplate struct {
	Metadata liveIngressTemplateMetadata `json:"metadata"`
}

type liveIngressTemplateMetadata struct {
	Annotations map[string]string `json:"annotations"`
}

type liveDNS01 struct {
	Cloudflare liveCloudflare `json:"cloudflare"`
}

type liveCloudflare struct {
	APITokenSecretRef liveSecretKeyReference `json:"apiTokenSecretRef"`
}

type liveSecretKeyReference struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

func (o liveClusterIssuer) snapshot(expectedName string) (ClusterIssuerSnapshot, error) {
	if o.APIVersion != "cert-manager.io/v1" || o.Kind != "ClusterIssuer" || o.Metadata.Name != expectedName || o.Metadata.Namespace != "" ||
		!uuidRE.MatchString(o.Metadata.UID) || !validResourceVersion(o.Metadata.ResourceVersion) || o.Metadata.Generation < 1 ||
		len(o.Metadata.Annotations) > 64 || len(o.Metadata.Labels) > 64 || o.Metadata.Labels[issuerManagedByLabel] != "kuberploy" ||
		o.Metadata.Labels[issuerProfileLabel] != expectedName {
		return ClusterIssuerSnapshot{}, ErrObservationUnavailable
	}
	annotatedDigest := o.Metadata.Annotations[issuerDigestAnnotation]
	annotatedRevisionRaw := o.Metadata.Annotations[issuerRevisionAnnotation]
	annotatedRevision, err := strconv.ParseInt(annotatedRevisionRaw, 10, 64)
	if !digestRE.MatchString(annotatedDigest) || err != nil || annotatedRevision < 1 || strconv.FormatInt(annotatedRevision, 10) != annotatedRevisionRaw {
		return ClusterIssuerSnapshot{}, ErrObservationUnavailable
	}
	var live liveIssuerSpec
	if err = decodeStrict(o.Spec, &live); err != nil || len(live.ACME.Solvers) != 1 {
		return ClusterIssuerSnapshot{}, ErrObservationUnavailable
	}
	spec, solver, specDigest, err := normalizeLiveSpec(live)
	if err != nil {
		return ClusterIssuerSnapshot{}, ErrObservationUnavailable
	}
	ready, observedGeneration, foundReady := false, int64(0), false
	if len(o.Status.Conditions) > 32 {
		return ClusterIssuerSnapshot{}, ErrObservationUnavailable
	}
	for _, condition := range o.Status.Conditions {
		if len(condition.Reason) > 256 || len(condition.Message) > 4096 {
			return ClusterIssuerSnapshot{}, ErrObservationUnavailable
		}
		if condition.Type != "Ready" {
			continue
		}
		if foundReady || (condition.Status != "True" && condition.Status != "False" && condition.Status != "Unknown") {
			return ClusterIssuerSnapshot{}, ErrObservationUnavailable
		}
		foundReady = true
		ready = condition.Status == "True"
		observedGeneration = condition.ObservedGeneration
	}
	if !foundReady || observedGeneration < 1 {
		return ClusterIssuerSnapshot{}, ErrObservationUnavailable
	}
	return ClusterIssuerSnapshot{Name: expectedName, UID: o.Metadata.UID, ResourceVersion: o.Metadata.ResourceVersion,
		AnnotatedSpecDigest: annotatedDigest, AnnotatedRevision: annotatedRevision, Generation: o.Metadata.Generation,
		ReadyObservedGeneration: observedGeneration, Ready: ready, SpecDigest: specDigest, Solver: solver, Spec: spec}, nil
}

func normalizeLiveSpec(live liveIssuerSpec) (Spec, SolverType, string, error) {
	if len(live.ACME.Solvers) != 1 {
		return Spec{}, "", "", ErrObservationUnavailable
	}
	item := live.ACME.Solvers[0]
	spec := Spec{ACME: ACME{Email: live.ACME.Email, Server: live.ACME.Server, AccountPrivateKeySecretName: live.ACME.PrivateKeySecretRef.Name}}
	if item.HTTP01 != nil && item.DNS01 == nil {
		if item.Selector != nil && len(item.Selector.DNSZones) != 0 || item.HTTP01.Ingress.IngressClassName != "traefik" ||
			len(item.HTTP01.Ingress.IngressTemplate.Metadata.Annotations) != 2 ||
			item.HTTP01.Ingress.IngressTemplate.Metadata.Annotations[http01ExternalDNSAnnotation] != "annotation-only" ||
			item.HTTP01.Ingress.IngressTemplate.Metadata.Annotations[http01ExternalDNSExclude] != "true" {
			return Spec{}, "", "", ErrObservationUnavailable
		}
		spec.HTTP01 = &HTTP01Spec{}
	} else if item.DNS01 != nil && item.HTTP01 == nil {
		if item.Selector == nil || len(item.Selector.DNSZones) < 1 {
			return Spec{}, "", "", ErrObservationUnavailable
		}
		spec.Cloudflare = &CloudflareDNS01Spec{APITokenSecretName: item.DNS01.Cloudflare.APITokenSecretRef.Name,
			APITokenSecretKey: item.DNS01.Cloudflare.APITokenSecretRef.Key, DNSZones: append([]string(nil), item.Selector.DNSZones...)}
	} else {
		return Spec{}, "", "", ErrObservationUnavailable
	}
	original := cloneSpec(spec)
	clean, solver, digest, err := normalizeSpec(spec)
	if err != nil || clean.ACME != original.ACME || solver == HTTP01 && original.HTTP01 == nil || solver == DNS01Cloudflare &&
		(original.Cloudflare == nil || clean.Cloudflare.APITokenSecretName != original.Cloudflare.APITokenSecretName ||
			clean.Cloudflare.APITokenSecretKey != original.Cloudflare.APITokenSecretKey || !slices.Equal(clean.Cloudflare.DNSZones, original.Cloudflare.DNSZones)) {
		return Spec{}, "", "", ErrObservationUnavailable
	}
	return clean, solver, digest, nil
}

func decodeStrict(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrObservationUnavailable
	}
	return nil
}

func validResourceVersion(value string) bool {
	return len(value) >= 1 && len(value) <= 128 && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, func(character rune) bool { return character < 0x21 || character == 0x7f }) < 0
}

func validIssuerDNSName(value string) bool {
	if len(value) < 1 || len(value) > 253 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if !dnsLabelRE.MatchString(label) {
			return false
		}
	}
	return true
}

func zeroBytes(values []byte) {
	for index := range values {
		values[index] = 0
	}
}
