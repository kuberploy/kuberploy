package imagepull

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
	"strings"
	"time"
)

const (
	serviceAccountRoot  = "/var/run/secrets/kubernetes.io/serviceaccount"
	maximumAPIBody      = 1 << 20
	maximumServiceToken = 32 << 10
	maximumJSONDepth    = 32
)

type inClusterSecretResources struct {
	baseURL   string
	http      *http.Client
	tokenPath string
}

func NewInClusterKubernetesSecretAPI() (*KubernetesSecretAPI, error) {
	resources, err := newInClusterSecretResources()
	if err != nil {
		return nil, err
	}
	return NewKubernetesSecretAPI(resources)
}

func newInClusterSecretResources() (*inClusterSecretResources, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	if host == "" || port == "" {
		return nil, ErrUnavailable
	}
	caPEM, err := os.ReadFile(serviceAccountRoot + "/ca.crt")
	if err != nil {
		return nil, ErrUnavailable
	}
	defer clearBytes(caPEM)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, ErrUnavailable
	}
	client := &http.Client{Timeout: 10 * time.Second,
		Transport:     &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Kubernetes API redirect rejected") },
	}
	return &inClusterSecretResources{baseURL: "https://" + net.JoinHostPort(host, port), http: client,
		tokenPath: serviceAccountRoot + "/token"}, nil
}

func (c *inClusterSecretResources) Get(ctx context.Context, namespace, name string) (kubernetesSecret, error) {
	path, err := imagePullSecretPath(namespace, name)
	if err != nil {
		return kubernetesSecret{}, err
	}
	body, status, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return kubernetesSecret{}, err
	}
	defer clearBytes(body)
	if status == http.StatusNotFound {
		return kubernetesSecret{}, errSecretNotFound
	}
	if status != http.StatusOK {
		return kubernetesSecret{}, ErrUnavailable
	}
	return decodeKubernetesSecret(body)
}

func (c *inClusterSecretResources) Create(ctx context.Context, namespace string, object kubernetesSecret) (kubernetesSecret, error) {
	if object.Metadata.Namespace != namespace || object.Metadata.Name == "" {
		return kubernetesSecret{}, ErrInvalid
	}
	path, err := imagePullSecretPath(namespace, "")
	if err != nil {
		return kubernetesSecret{}, err
	}
	body, err := json.Marshal(object)
	if err != nil || len(body) == 0 || len(body) > maximumAPIBody {
		clearBytes(body)
		return kubernetesSecret{}, ErrInvalid
	}
	defer clearBytes(body)
	response, status, err := c.request(ctx, http.MethodPost, path, body)
	if err != nil {
		return kubernetesSecret{}, err
	}
	defer clearBytes(response)
	if status == http.StatusConflict {
		return kubernetesSecret{}, errSecretConflict
	}
	if status != http.StatusCreated {
		return kubernetesSecret{}, ErrUnavailable
	}
	return decodeKubernetesSecret(response)
}

func (c *inClusterSecretResources) request(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	if c == nil || c.http == nil || !strings.HasPrefix(c.baseURL, "https://") || c.tokenPath != serviceAccountRoot+"/token" ||
		(method != http.MethodGet && method != http.MethodPost) || !strings.HasPrefix(path, "/api/v1/namespaces/") ||
		(!strings.HasSuffix(path, "/secrets") && !strings.Contains(path, "/secrets/")) {
		return nil, 0, ErrInvalid
	}
	tokenFile, err := os.Open(c.tokenPath)
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	tokenBytes, readErr := io.ReadAll(io.LimitReader(tokenFile, maximumServiceToken+1))
	closeErr := tokenFile.Close()
	defer clearBytes(tokenBytes)
	if readErr != nil || closeErr != nil || len(tokenBytes) == 0 || len(tokenBytes) > maximumServiceToken {
		return nil, 0, ErrUnavailable
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || len(token) != len(tokenBytes) || strings.IndexFunc(token, func(value rune) bool { return value < 0x21 || value == 0x7f }) >= 0 {
		return nil, 0, ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if len(body) != 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maximumAPIBody+1))
	if err != nil || len(encoded) > maximumAPIBody {
		clearBytes(encoded)
		return nil, 0, ErrUnavailable
	}
	return encoded, response.StatusCode, nil
}

func imagePullSecretPath(namespace, name string) (string, error) {
	if !dnsLabelPattern.MatchString(namespace) || name != "" && !dnsLabelPattern.MatchString(name) {
		return "", ErrInvalid
	}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/secrets"
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	return path, nil
}

func decodeKubernetesSecret(encoded []byte) (kubernetesSecret, error) {
	if validateSingleJSON(encoded) != nil {
		return kubernetesSecret{}, ErrUnavailable
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &top); err != nil {
		return kubernetesSecret{}, ErrUnavailable
	}
	for key := range top {
		if key != "apiVersion" && key != "kind" && key != "metadata" && key != "immutable" && key != "type" && key != "data" {
			return kubernetesSecret{}, ErrUnavailable
		}
	}
	for _, key := range []string{"apiVersion", "kind", "metadata", "immutable", "type", "data"} {
		if _, present := top[key]; !present {
			return kubernetesSecret{}, ErrUnavailable
		}
	}
	var object kubernetesSecret
	if err := json.Unmarshal(encoded, &object); err != nil {
		clearKubernetesSecret(&object)
		return kubernetesSecret{}, ErrUnavailable
	}
	return object, nil
}

func validateSingleJSON(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := validateJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrInvalid
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maximumJSONDepth {
		return ErrInvalid
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrInvalid
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			key, ok := keyToken.(string)
			if keyErr != nil || !ok {
				return ErrInvalid
			}
			if _, duplicate := seen[key]; duplicate {
				return ErrInvalid
			}
			seen[key] = struct{}{}
			if err = validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, endErr := decoder.Token()
		if endErr != nil || end != json.Delim('}') {
			return ErrInvalid
		}
	case '[':
		for decoder.More() {
			if err = validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, endErr := decoder.Token()
		if endErr != nil || end != json.Delim(']') {
			return ErrInvalid
		}
	default:
		return fmt.Errorf("%w: unexpected JSON delimiter", ErrInvalid)
	}
	return nil
}

var _ kubernetesSecretResources = (*inClusterSecretResources)(nil)
