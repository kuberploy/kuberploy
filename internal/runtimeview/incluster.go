package runtimeview

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
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	runtimeServiceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"
	runtimeKubernetesMaxJSON       = int64(8 << 20)
	runtimeKubernetesMaxItems      = 1_000
)

// InClusterClient is a deliberately narrow, read-only Kubernetes adapter for
// the runtime view service. It has no generic request method and cannot read
// Secrets, exec, attach, port-forward, or mutate cluster resources.
type InClusterClient struct {
	baseURL   string
	http      *http.Client
	tokenPath string
}

var _ KubernetesClient = (*InClusterClient)(nil)

// NewInClusterClient pins the API server to Kubernetes' injected service
// endpoint and verifies it against the projected cluster CA. Proxy settings
// and redirects are disabled so a bearer token cannot leave that endpoint.
func NewInClusterClient() (*InClusterClient, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes in-cluster service environment is missing")
	}
	caPEM, err := os.ReadFile(runtimeServiceAccountDirectory + "/ca.crt")
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
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
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
		tokenPath: runtimeServiceAccountDirectory + "/token",
	}, nil
}

func (c *InClusterClient) Security() ClientSecurity {
	return ClientSecurity{TLSVerified: true, InsecureSkipTLSVerify: false}
}

// Probe proves that the fixed in-cluster TLS endpoint and projected service
// account token can reach Kubernetes discovery. It performs no namespaced
// read and cannot be redirected to a caller-selected resource.
func (c *InClusterClient) Probe(ctx context.Context) error {
	var discovery struct {
		Versions []string `json:"versions"`
	}
	if err := c.getJSON(ctx, "/api", nil, &discovery); err != nil {
		return err
	}
	for _, version := range discovery.Versions {
		if version == "v1" {
			return nil
		}
	}
	return ErrScopeViolation
}

func (c *InClusterClient) GetDeployment(ctx context.Context, namespace, name string) (Deployment, error) {
	if !validKubeObject(namespace, name) {
		return Deployment{}, ErrInvalidRequest
	}
	var object deploymentObject
	if err := c.getJSON(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(namespace)+"/deployments/"+url.PathEscape(name), nil, &object); err != nil {
		return Deployment{}, err
	}
	return decodeDeployment(object, namespace, name)
}

func (c *InClusterClient) ListReplicaSets(ctx context.Context, namespace string, selector LabelSelector) ([]ReplicaSet, error) {
	if !kubeNamePattern.MatchString(namespace) {
		return nil, ErrInvalidRequest
	}
	encodedSelector, err := encodeSelector(selector)
	if err != nil {
		return nil, err
	}
	query := url.Values{"labelSelector": {encodedSelector}, "limit": {strconv.Itoa(runtimeKubernetesMaxItems + 1)}}
	var list replicaSetList
	if err = c.getJSON(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(namespace)+"/replicasets", query, &list); err != nil {
		return nil, err
	}
	if list.Metadata.Continue != "" || len(list.Items) > runtimeKubernetesMaxItems {
		return nil, ErrResponseLimitReached
	}
	result := make([]ReplicaSet, 0, len(list.Items))
	for _, object := range list.Items {
		value, decodeErr := decodeReplicaSet(object, namespace)
		if decodeErr != nil {
			return nil, decodeErr
		}
		result = append(result, value)
	}
	return result, nil
}

func (c *InClusterClient) ListPods(ctx context.Context, namespace string, selector LabelSelector) ([]Pod, error) {
	if !kubeNamePattern.MatchString(namespace) {
		return nil, ErrInvalidRequest
	}
	encodedSelector, err := encodeSelector(selector)
	if err != nil {
		return nil, err
	}
	query := url.Values{"labelSelector": {encodedSelector}, "limit": {strconv.Itoa(runtimeKubernetesMaxItems + 1)}}
	var list podList
	if err = c.getJSON(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/pods", query, &list); err != nil {
		return nil, err
	}
	if list.Metadata.Continue != "" || len(list.Items) > runtimeKubernetesMaxItems {
		return nil, ErrTooManySources
	}
	result := make([]Pod, 0, len(list.Items))
	for _, object := range list.Items {
		value, decodeErr := decodePod(object, namespace, "")
		if decodeErr != nil {
			return nil, decodeErr
		}
		result = append(result, value)
	}
	return result, nil
}

func (c *InClusterClient) GetPod(ctx context.Context, namespace, name string) (Pod, error) {
	if !validKubeObject(namespace, name) {
		return Pod{}, ErrInvalidRequest
	}
	var object podObject
	if err := c.getJSON(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/pods/"+url.PathEscape(name), nil, &object); err != nil {
		return Pod{}, err
	}
	return decodePod(object, namespace, name)
}

func (c *InClusterClient) OpenPodLogs(ctx context.Context, request PodLogRequest) (io.ReadCloser, error) {
	if !validKubeObject(request.Namespace, request.PodName) || !uidPattern.MatchString(request.PodUID) || !containerPattern.MatchString(request.Options.Container) ||
		request.Options.TailLines < 1 || request.Options.TailLines > 2_000 || request.Options.LimitBytes < 1 || request.Options.LimitBytes > 5<<20 {
		return nil, ErrInvalidRequest
	}
	// Bind the subresource request to the exact Pod instance immediately before
	// opening it. A delete/recreate race therefore fails instead of returning a
	// different tenant process with the same Pod name.
	live, err := c.GetPod(ctx, request.Namespace, request.PodName)
	if err != nil {
		return nil, err
	}
	if live.UID != request.PodUID {
		return nil, ErrGone
	}
	query := url.Values{
		"container":  {request.Options.Container},
		"tailLines":  {strconv.FormatInt(request.Options.TailLines, 10)},
		"limitBytes": {strconv.FormatInt(request.Options.LimitBytes, 10)},
		"timestamps": {strconv.FormatBool(request.Options.Timestamps)},
		"follow":     {strconv.FormatBool(request.Options.Follow)},
	}
	if request.Options.Previous {
		query.Set("previous", "true")
	}
	if request.Options.SinceTime != nil {
		query.Set("sinceTime", request.Options.SinceTime.UTC().Format(time.RFC3339Nano))
	}
	response, err := c.request(ctx, "/api/v1/namespaces/"+url.PathEscape(request.Namespace)+"/pods/"+url.PathEscape(request.PodName)+"/log", query, "text/plain, application/json")
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusNotFound {
		response.Body.Close()
		return nil, ErrNotFound
	}
	if response.StatusCode == http.StatusBadRequest && request.Options.Previous {
		response.Body.Close()
		return nil, ErrPreviousUnavailable
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("Kubernetes logs returned HTTP %d", response.StatusCode)
	}
	return &boundedReadCloser{Reader: io.LimitReader(response.Body, request.Options.LimitBytes+1), closer: response.Body}, nil
}

func (c *InClusterClient) ListEvents(ctx context.Context, namespace string, query EventQuery) ([]KubernetesEvent, error) {
	// Service.Events asks for one extra item so it can report truncation while
	// the public hard limit remains 200.
	if !kubeNamePattern.MatchString(namespace) || query.Limit < 1 || query.Limit > 201 || len(query.InvolvedUIDs) < 1 || len(query.InvolvedUIDs) > runtimeKubernetesMaxItems {
		return nil, ErrInvalidRequest
	}
	seenUID := make(map[string]struct{}, len(query.InvolvedUIDs))
	seenEvent := map[string]struct{}{}
	result := make([]KubernetesEvent, 0, query.Limit)
	for _, involvedUID := range query.InvolvedUIDs {
		if !uidPattern.MatchString(involvedUID) {
			return nil, ErrInvalidRequest
		}
		if _, duplicate := seenUID[involvedUID]; duplicate {
			continue
		}
		seenUID[involvedUID] = struct{}{}
		values := url.Values{
			"fieldSelector": {"involvedObject.uid=" + involvedUID},
			"limit":         {strconv.Itoa(query.Limit)},
		}
		var list eventList
		if err := c.getJSON(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/events", values, &list); err != nil {
			return nil, err
		}
		for _, object := range list.Items {
			event, decodeErr := decodeEvent(object, namespace, involvedUID)
			if decodeErr != nil {
				return nil, decodeErr
			}
			if _, duplicate := seenEvent[event.UID]; duplicate {
				continue
			}
			seenEvent[event.UID] = struct{}{}
			result = append(result, event)
			if len(result) == query.Limit {
				break
			}
		}
		if len(result) == query.Limit {
			break
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].LastSeen.Equal(result[j].LastSeen) {
			return result[i].LastSeen.Before(result[j].LastSeen)
		}
		return result[i].UID < result[j].UID
	})
	return result, nil
}

func (c *InClusterClient) getJSON(ctx context.Context, path string, query url.Values, destination any) error {
	response, err := c.request(ctx, path, query, "application/json")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Kubernetes API returned HTTP %d", response.StatusCode)
	}
	limited := io.LimitReader(response.Body, runtimeKubernetesMaxJSON+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return errors.New("read bounded Kubernetes API response")
	}
	defer clear(body)
	if int64(len(body)) > runtimeKubernetesMaxJSON {
		return ErrResponseLimitReached
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err = decoder.Decode(destination); err != nil {
		return ErrScopeViolation
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		return ErrScopeViolation
	}
	return nil
}

func (c *InClusterClient) request(ctx context.Context, path string, query url.Values, accept string) (*http.Response, error) {
	if c == nil || c.http == nil || !strings.HasPrefix(c.baseURL, "https://") || !strings.HasPrefix(path, "/api") || strings.ContainsAny(path, "\x00\r\n") {
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
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Accept", accept)
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	response, err := c.http.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("Kubernetes API request failed: %w", err)
	}
	return response, nil
}

func encodeSelector(selector LabelSelector) (string, error) {
	if len(selector.MatchLabels) < 1 || len(selector.MatchLabels) > 8 {
		return "", ErrSelectorNotAllowed
	}
	keys := make([]string, 0, len(selector.MatchLabels))
	for key, value := range selector.MatchLabels {
		if _, allowed := allowedSelectorKeys[key]; !allowed || !validLabelValue(value) || strings.ContainsAny(key+value, ",=()!\x00\r\n") {
			return "", ErrSelectorNotAllowed
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+selector.MatchLabels[key])
	}
	return strings.Join(parts, ","), nil
}

func validKubeObject(namespace, name string) bool {
	return kubeNamePattern.MatchString(namespace) && kubeNamePattern.MatchString(name)
}

type boundedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *boundedReadCloser) Close() error { return r.closer.Close() }

type objectMetadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	Annotations       map[string]string `json:"annotations"`
	OwnerReferences   []ownerObject     `json:"ownerReferences"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
}

type listMetadata struct {
	Continue string `json:"continue"`
}

type ownerObject struct {
	UID        string `json:"uid"`
	Kind       string `json:"kind"`
	Controller *bool  `json:"controller"`
}

type deploymentObject struct {
	Metadata objectMetadata `json:"metadata"`
	Spec     struct {
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
	} `json:"spec"`
}

type replicaSetObject struct {
	Metadata objectMetadata `json:"metadata"`
	Status   struct {
		ReadyReplicas int32 `json:"readyReplicas"`
	} `json:"status"`
}

type replicaSetList struct {
	Metadata listMetadata       `json:"metadata"`
	Items    []replicaSetObject `json:"items"`
}

type namedContainer struct {
	Name string `json:"name"`
}

type containerStatus struct {
	Name         string `json:"name"`
	RestartCount int32  `json:"restartCount"`
}

type podCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

type podObject struct {
	Metadata objectMetadata `json:"metadata"`
	Spec     struct {
		Containers     []namedContainer `json:"containers"`
		InitContainers []namedContainer `json:"initContainers"`
	} `json:"spec"`
	Status struct {
		ContainerStatuses     []containerStatus `json:"containerStatuses"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
		Conditions            []podCondition    `json:"conditions"`
	} `json:"status"`
}

type podList struct {
	Metadata listMetadata `json:"metadata"`
	Items    []podObject  `json:"items"`
}

type kubernetesEventObject struct {
	Metadata objectMetadata `json:"metadata"`
	Involved struct {
		UID  string `json:"uid"`
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"involvedObject"`
	Type           string    `json:"type"`
	Reason         string    `json:"reason"`
	Message        string    `json:"message"`
	Count          int32     `json:"count"`
	FirstTimestamp time.Time `json:"firstTimestamp"`
	LastTimestamp  time.Time `json:"lastTimestamp"`
	EventTime      time.Time `json:"eventTime"`
	Series         *struct {
		Count            int32     `json:"count"`
		LastObservedTime time.Time `json:"lastObservedTime"`
	} `json:"series"`
}

type eventList struct {
	Metadata listMetadata            `json:"metadata"`
	Items    []kubernetesEventObject `json:"items"`
}

func decodeDeployment(object deploymentObject, namespace, name string) (Deployment, error) {
	if object.Metadata.Namespace != namespace || object.Metadata.Name != name || !uidPattern.MatchString(object.Metadata.UID) {
		return Deployment{}, ErrScopeViolation
	}
	return Deployment{Namespace: namespace, Name: name, UID: object.Metadata.UID, Selector: LabelSelector{MatchLabels: cloneStrings(object.Spec.Selector.MatchLabels)}}, nil
}

func decodeReplicaSet(object replicaSetObject, namespace string) (ReplicaSet, error) {
	if object.Metadata.Namespace != namespace || !kubeNamePattern.MatchString(object.Metadata.Name) || !uidPattern.MatchString(object.Metadata.UID) {
		return ReplicaSet{}, ErrScopeViolation
	}
	return ReplicaSet{Namespace: namespace, Name: object.Metadata.Name, UID: object.Metadata.UID, Owners: decodeOwners(object.Metadata.OwnerReferences), Revision: object.Metadata.Annotations["deployment.kubernetes.io/revision"], Ready: object.Status.ReadyReplicas > 0}, nil
}

func decodePod(object podObject, namespace, expectedName string) (Pod, error) {
	if object.Metadata.Namespace != namespace || !kubeNamePattern.MatchString(object.Metadata.Name) || expectedName != "" && object.Metadata.Name != expectedName || !uidPattern.MatchString(object.Metadata.UID) {
		return Pod{}, ErrScopeViolation
	}
	statuses := map[string]int32{}
	for _, status := range append(append([]containerStatus(nil), object.Status.InitContainerStatuses...), object.Status.ContainerStatuses...) {
		statuses[status.Name] = status.RestartCount
	}
	containers := make([]Container, 0, len(object.Spec.InitContainers)+len(object.Spec.Containers))
	for _, container := range object.Spec.InitContainers {
		if !containerPattern.MatchString(container.Name) {
			return Pod{}, ErrScopeViolation
		}
		containers = append(containers, Container{Name: container.Name, Kind: ContainerInit, RestartCount: statuses[container.Name]})
	}
	for _, container := range object.Spec.Containers {
		if !containerPattern.MatchString(container.Name) {
			return Pod{}, ErrScopeViolation
		}
		containers = append(containers, Container{Name: container.Name, Kind: ContainerRegular, RestartCount: statuses[container.Name]})
	}
	ready := false
	for _, condition := range object.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			ready = true
		}
	}
	return Pod{Namespace: namespace, Name: object.Metadata.Name, UID: object.Metadata.UID, Owners: decodeOwners(object.Metadata.OwnerReferences), Containers: containers, Ready: ready, Terminating: object.Metadata.DeletionTimestamp != nil}, nil
}

func decodeEvent(object kubernetesEventObject, namespace, expectedUID string) (KubernetesEvent, error) {
	if object.Metadata.Namespace != namespace || !uidPattern.MatchString(object.Metadata.UID) || object.Involved.UID != expectedUID || !uidPattern.MatchString(object.Involved.UID) || !kubeNamePattern.MatchString(object.Involved.Name) || object.Involved.Kind == "" {
		return KubernetesEvent{}, ErrScopeViolation
	}
	first, last := object.FirstTimestamp, object.LastTimestamp
	if first.IsZero() {
		first = object.EventTime
	}
	if object.Series != nil {
		last = object.Series.LastObservedTime
		if object.Series.Count > object.Count {
			object.Count = object.Series.Count
		}
	}
	if last.IsZero() {
		last = object.EventTime
	}
	if first.IsZero() {
		first = last
	}
	return KubernetesEvent{Namespace: namespace, UID: object.Metadata.UID, InvolvedUID: object.Involved.UID, InvolvedKind: object.Involved.Kind, InvolvedName: object.Involved.Name, Type: object.Type, Reason: object.Reason, Message: object.Message, Count: object.Count, FirstSeen: first, LastSeen: last}, nil
}

func decodeOwners(values []ownerObject) []OwnerReference {
	result := make([]OwnerReference, 0, len(values))
	for _, value := range values {
		result = append(result, OwnerReference{UID: value.UID, Kind: value.Kind, Controller: value.Controller != nil && *value.Controller})
	}
	return result
}

func cloneStrings(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
