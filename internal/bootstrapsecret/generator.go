package bootstrapsecret

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
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
)

var (
	errInvalidConfig = errors.New("bootstrap secret generator configuration is invalid")
	dnsLabelRE       = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	secretKeyRE      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,253}$`)
)

type Config struct {
	APIServer, Namespace, SecretName, SecretKey string
	TokenFile, CAFile                           string
}

type Result struct {
	Created bool
	Token   string
}

func ConfigFromEnvironment() (Config, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = "443"
	}
	cfg := Config{
		APIServer:  "https://" + host + ":" + port,
		Namespace:  strings.TrimSpace(os.Getenv("KUBERPLOY_BOOTSTRAP_NAMESPACE")),
		SecretName: strings.TrimSpace(os.Getenv("KUBERPLOY_BOOTSTRAP_SECRET_NAME")),
		SecretKey:  strings.TrimSpace(os.Getenv("KUBERPLOY_BOOTSTRAP_SECRET_KEY")),
		TokenFile:  "/var/run/secrets/kubernetes.io/serviceaccount/token",
		CAFile:     "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	endpoint, err := url.Parse(c.APIServer)
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil || endpoint.Path != "" ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Hostname() == "" {
		return errInvalidConfig
	}
	_, port, splitErr := net.SplitHostPort(endpoint.Host)
	portNumber, portErr := strconv.Atoi(port)
	if splitErr != nil || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return errInvalidConfig
	}
	if !dnsLabelRE.MatchString(c.Namespace) ||
		!dnsLabelRE.MatchString(c.SecretName) || !secretKeyRE.MatchString(c.SecretKey) ||
		strings.TrimSpace(c.TokenFile) == "" || strings.TrimSpace(c.CAFile) == "" {
		return errInvalidConfig
	}
	return nil
}

func Generate(ctx context.Context, cfg Config) (Result, error) {
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	bearer, err := os.ReadFile(cfg.TokenFile)
	if err != nil || strings.TrimSpace(string(bearer)) == "" {
		return Result{}, errors.New("bootstrap generator service-account token unavailable")
	}
	ca, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return Result{}, errors.New("bootstrap generator cluster CA unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return Result{}, errors.New("bootstrap generator cluster CA is invalid")
	}
	raw := make([]byte, 32)
	if _, err = io.ReadFull(rand.Reader, raw); err != nil {
		return Result{}, errors.New("bootstrap token generation failed")
	}
	token := "kp_bootstrap_" + base64.RawURLEncoding.EncodeToString(raw)
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": cfg.SecretName, "namespace": cfg.Namespace,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "kuberploy-bootstrap", "kuberploy.io/purpose": "first-administrator"},
		},
		"type": "Opaque",
		"data": map[string]string{cfg.SecretKey: base64.StdEncoding.EncodeToString([]byte(token))},
	})
	if err != nil {
		return Result{}, errors.New("bootstrap secret encoding failed")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cfg.APIServer+"/api/v1/namespaces/"+cfg.Namespace+"/secrets", bytes.NewReader(body))
	if err != nil {
		return Result{}, errors.New("bootstrap secret request construction failed")
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(bearer)))
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}}
	response, err := client.Do(request)
	if err != nil {
		return Result{}, errors.New("bootstrap secret Kubernetes request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	switch response.StatusCode {
	case http.StatusCreated:
		return Result{Created: true, Token: token}, nil
	case http.StatusConflict:
		return Result{}, nil
	default:
		return Result{}, fmt.Errorf("bootstrap secret Kubernetes request returned HTTP %d", response.StatusCode)
	}
}
