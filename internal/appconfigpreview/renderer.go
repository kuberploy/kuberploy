// Package appconfigpreview renders the canonical, operator-owned runtime chart
// for synchronous AppConfig previews. The caller supplies values only: the
// executable, chart path, Kubernetes version, and argument vector are closed.
package appconfigpreview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/helmapps"
	"go.yaml.in/yaml/v3"
)

const (
	Contract             = "appconfig-rendered-preview.v1"
	ProductionChartPath  = "/opt/kuberploy/charts/kuberploy-runtime"
	ProductionHelmPath   = "/usr/local/bin/helm"
	MaximumInputBytes    = 256 << 10
	MaximumManifestBytes = helmapps.MaximumOutputSize
	MaximumResources     = helmapps.MaximumResources
	RenderTimeout        = helmapps.RenderTimeout
)

var (
	ErrInvalid     = errors.New("AppConfig rendered preview input is invalid")
	ErrUnavailable = errors.New("AppConfig rendered preview is unavailable")
	dnsLabelRE     = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	digestRE       = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

type Identity struct {
	Contract        string `json:"contract"`
	ChartName       string `json:"chartName"`
	ChartVersion    string `json:"chartVersion"`
	ChartDigest     string `json:"chartDigest"`
	RendererImage   string `json:"rendererImage"`
	RendererVersion string `json:"rendererVersion"`
	PolicyVersion   string `json:"policyVersion"`
}

func (i Identity) Validate() error {
	if i.Contract != Contract || i.ChartName != "kuberploy-runtime" || i.ChartVersion == "" || len(i.ChartVersion) > 64 ||
		!digestRE.MatchString(i.ChartDigest) || i.RendererImage != helmapps.RendererImage ||
		i.RendererVersion != helmapps.HelmVersion || i.PolicyVersion != helmapps.PolicyVersion {
		return ErrInvalid
	}
	return nil
}

func (i Identity) Digest() (string, error) {
	if i.Validate() != nil {
		return "", ErrInvalid
	}
	raw, err := json.Marshal(i)
	if err != nil {
		return "", ErrInvalid
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// PreviewTokenHash binds a bearer preview token to the immutable render
// authority. A chart or renderer rollout therefore invalidates every token
// issued by the previous identity without storing identity data in the token.
func PreviewTokenHash(raw []byte, identityDigest string) ([sha256.Size]byte, error) {
	if len(raw) != 32 || !digestRE.MatchString(identityDigest) {
		return [sha256.Size]byte{}, ErrInvalid
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("kuberploy-config-preview-token.v2\x00"))
	_, _ = hash.Write(raw)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(identityDigest))
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

type Request struct {
	Namespace, ReleaseName                  string
	ProjectID, EnvironmentID, ApplicationID string
	CurrentValues, CandidateValues          []byte
}

type Result struct {
	RenderedDiff   string
	Identity       Identity
	IdentityDigest string
}

type Service struct {
	identity            Identity
	helmPath, chartPath string
	run                 func(context.Context, string, ...string) ([]byte, error)
}

func NewProduction(identity Identity) (*Service, error) {
	return newService(identity, ProductionHelmPath, ProductionChartPath, nil)
}

// NewTestService exists only for hermetic package tests. Production always
// uses the fixed executable and chart paths above.
func NewTestService(identity Identity, run func(context.Context, string, ...string) ([]byte, error)) (*Service, error) {
	return newService(identity, ProductionHelmPath, ProductionChartPath, run)
}

func newService(identity Identity, helmPath, chartPath string, run func(context.Context, string, ...string) ([]byte, error)) (*Service, error) {
	if identity.Validate() != nil || filepath.Clean(helmPath) != helmPath || filepath.Clean(chartPath) != chartPath || !filepath.IsAbs(helmPath) || !filepath.IsAbs(chartPath) {
		return nil, ErrInvalid
	}
	if run == nil {
		run = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
			command := exec.CommandContext(ctx, binary, args...)
			command.Env = []string{"HOME=/tmp", "HELM_CACHE_HOME=/tmp/helm-cache", "HELM_CONFIG_HOME=/tmp/helm-config", "HELM_DATA_HOME=/tmp/helm-data"}
			output, err := command.Output()
			if err != nil {
				return nil, ErrUnavailable
			}
			return output, nil
		}
	}
	return &Service{identity: identity, helmPath: helmPath, chartPath: chartPath, run: run}, nil
}

func (s *Service) Identity() (Identity, string, error) {
	if s == nil || s.identity.Validate() != nil {
		return Identity{}, "", ErrUnavailable
	}
	digest, err := s.identity.Digest()
	return s.identity, digest, err
}

func (s *Service) Render(ctx context.Context, request Request) (Result, error) {
	identity, identityDigest, err := s.Identity()
	if err != nil || ctx == nil || !validRequest(request) {
		return Result{}, ErrInvalid
	}
	current, err := s.renderDeterministic(ctx, request, request.CurrentValues, "current")
	if err != nil {
		return Result{}, err
	}
	candidate, err := s.renderDeterministic(ctx, request, request.CandidateValues, "candidate")
	if err != nil {
		return Result{}, err
	}
	current, err = redactAndCanonicalize(current, request)
	if err != nil {
		return Result{}, err
	}
	candidate, err = redactAndCanonicalize(candidate, request)
	if err != nil {
		return Result{}, err
	}
	return Result{RenderedDiff: appconfig.GitDiff("rendered-manifests.yaml", current, candidate), Identity: identity, IdentityDigest: identityDigest}, nil
}

func validRequest(r Request) bool {
	return dnsLabelRE.MatchString(r.Namespace) && dnsLabelRE.MatchString(r.ReleaseName) &&
		r.ProjectID != "" && r.EnvironmentID != "" && r.ApplicationID != "" &&
		len(r.CurrentValues) > 0 && len(r.CurrentValues) <= MaximumInputBytes &&
		len(r.CandidateValues) > 0 && len(r.CandidateValues) <= MaximumInputBytes &&
		bytes.IndexByte(r.CurrentValues, 0) < 0 && bytes.IndexByte(r.CandidateValues, 0) < 0
}

func (s *Service) renderDeterministic(parent context.Context, request Request, values []byte, suffix string) ([]byte, error) {
	directory, err := os.MkdirTemp("", "kuberploy-appconfig-preview-")
	if err != nil {
		return nil, ErrUnavailable
	}
	defer os.RemoveAll(directory) //nolint:errcheck
	if err = os.Chmod(directory, 0o700); err != nil {
		return nil, ErrUnavailable
	}
	valuesPath := filepath.Join(directory, suffix+"-values.yaml")
	if err = os.WriteFile(valuesPath, values, 0o600); err != nil {
		return nil, ErrUnavailable
	}
	args := []string{"template", request.ReleaseName, s.chartPath, "--namespace", request.Namespace,
		"--values", valuesPath, "--kube-version", helmapps.RendererKubeVersion,
		"--set-string", "kuberployExpectedIdentity.projectId=" + request.ProjectID,
		"--set-string", "kuberployExpectedIdentity.environmentId=" + request.EnvironmentID,
		"--set-string", "kuberployExpectedIdentity.applicationId=" + request.ApplicationID}
	ctx, cancel := context.WithTimeout(parent, RenderTimeout)
	defer cancel()
	first, err := s.run(ctx, s.helmPath, args...)
	if err != nil || len(first) == 0 || len(first) > MaximumManifestBytes {
		return nil, ErrUnavailable
	}
	second, err := s.run(ctx, s.helmPath, args...)
	if err != nil || !bytes.Equal(first, second) {
		return nil, ErrUnavailable
	}
	return first, nil
}

var allowedKinds = map[string]bool{
	"v1/ConfigMap": true, "v1/Service": true, "v1/ServiceAccount": true,
	"apps/v1/Deployment": true, "networking.k8s.io/v1/Ingress": true,
	"networking.k8s.io/v1/NetworkPolicy": true,
	"traefik.io/v1alpha1/Middleware":     true,
}

func redactAndCanonicalize(raw []byte, request Request) ([]byte, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	documents := make([]map[string]any, 0, 8)
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || len(document) == 0 || len(documents) >= MaximumResources {
			return nil, ErrUnavailable
		}
		apiVersion, _ := document["apiVersion"].(string)
		kind, _ := document["kind"].(string)
		metadata, _ := document["metadata"].(map[string]any)
		name, _ := metadata["name"].(string)
		if !allowedKinds[apiVersion+"/"+kind] || !dnsLabelRE.MatchString(name) {
			return nil, ErrUnavailable
		}
		if namespace, present := metadata["namespace"]; present && namespace != request.Namespace {
			return nil, ErrUnavailable
		}
		labels, _ := metadata["labels"].(map[string]any)
		if labels["kuberploy.io/application"] != request.ApplicationID || labels["kuberploy.io/project"] != request.ProjectID || labels["kuberploy.io/environment"] != request.EnvironmentID {
			return nil, ErrUnavailable
		}
		if kind == "ConfigMap" || kind == "Secret" {
			for _, field := range []string{"data", "binaryData", "stringData"} {
				if values, ok := document[field].(map[string]any); ok {
					for key := range values {
						values[key] = "<redacted>"
					}
				}
			}
		}
		redactEnvValues(document)
		documents = append(documents, document)
	}
	if len(documents) == 0 {
		return nil, ErrUnavailable
	}
	sort.Slice(documents, func(i, j int) bool { return resourceKey(documents[i]) < resourceKey(documents[j]) })
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	for _, document := range documents {
		if err := encoder.Encode(document); err != nil {
			return nil, ErrUnavailable
		}
	}
	_ = encoder.Close()
	if output.Len() > MaximumManifestBytes {
		return nil, ErrUnavailable
	}
	return output.Bytes(), nil
}

func resourceKey(document map[string]any) string {
	metadata, _ := document["metadata"].(map[string]any)
	return fmt.Sprint(document["apiVersion"], "\x00", document["kind"], "\x00", metadata["namespace"], "\x00", metadata["name"])
}

func redactEnvValues(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if name, ok := typed["name"].(string); ok && name != "" {
			if _, present := typed["value"]; present {
				typed["value"] = "<redacted>"
			}
		}
		for _, child := range typed {
			redactEnvValues(child)
		}
	case []any:
		for _, child := range typed {
			redactEnvValues(child)
		}
	}
}
