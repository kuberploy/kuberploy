// Package helmapps defines the closed, admin-approved external Helm/OCI
// application contract. It deliberately has no API, Argo, Kubernetes, Git, or
// ambient credential dependency; those integrations consume its validated,
// immutable commands in later layers.
package helmapps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	DescriptorDocument = "app.yaml"
	ValuesDocument     = "values.yaml"

	HelmVersion           = "4.2.3"
	RendererContract      = "external-helm-renderer.v1"
	PolicyVersion         = "external-helm-p0.v1"
	RendererImage         = "docker.io/alpine/helm:4.2.3"
	MaximumAttempts       = 10
	MaximumDescriptorSize = 32 << 10
	MaximumValuesSize     = 256 << 10
	MaximumSchemaSize     = 512 << 10
	MaximumChartSize      = 8 << 20
	MaximumOutputSize     = 2 << 20
	MaximumResources      = 128
	MaximumYAMLNodes      = 8192
	MaximumYAMLDepth      = 32
	MaximumChartFiles     = 512
	MaximumFileSize       = 2 << 20
	MaximumExpandSize     = 16 << 20
	MaximumTemporarySize  = 16 << 20
	RenderTimeout         = 30 * time.Second
)

var (
	ErrInvalid             = errors.New("approved Helm application metadata is invalid")
	ErrNotFound            = errors.New("approved Helm application metadata was not found")
	ErrConflict            = errors.New("approved Helm application metadata conflicts with durable state")
	ErrUnavailable         = errors.New("approved Helm renderer is unavailable")
	ErrLeaseLost           = errors.New("approved Helm render lease was lost")
	ErrUnsafeChart         = errors.New("Helm chart violates the approved P0 boundary")
	ErrUnsafeYAML          = errors.New("YAML violates the approved P0 boundary")
	ErrNondeterministic    = errors.New("approved Helm chart rendered non-deterministically")
	ErrFoundationNotReady  = errors.New("exact environment foundation is not ready for Helm publication")
	ErrArgoProjectNotReady = errors.New("exact Argo environment project is not ready for Helm publication")

	uuidRE          = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	dnsLabelRE      = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	idempotencyRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)
	workerIDRE      = idempotencyRE
	failureCodeRE   = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,62}$`)
	semverRE        = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	ociRepositoryRE = regexp.MustCompile(`^oci://(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?::[1-9][0-9]{0,4})?/[a-z0-9]+(?:(?:[._-][a-z0-9]+)|(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*))*$`)
)

type ApprovalKey struct {
	ID       string
	Revision int64
}

func (k ApprovalKey) Validate() error {
	if !uuidRE.MatchString(k.ID) || k.Revision <= 0 {
		return ErrInvalid
	}
	return nil
}

// Approval is one immutable administrator decision. Repository coordinates
// contain no username, token, credential reference, floating tag, or range.
type Approval struct {
	ApprovalKey
	OCIRepository      string
	ChartVersion       string
	ManifestDigest     string
	PackageDigest      string
	ValuesSchemaDigest string
	RendererImage      string
	RendererVersion    string
	PolicyVersion      string
	CreatedBy          string
	IdempotencyKey     string
	CreatedAt          time.Time
}

func (a Approval) Validate() error {
	if a.ApprovalKey.Validate() != nil || !canonicalOCIRepository(a.OCIRepository) || !semverRE.MatchString(a.ChartVersion) ||
		!validDigest(a.ManifestDigest) || !validDigest(a.PackageDigest) || !validDigest(a.ValuesSchemaDigest) ||
		a.RendererImage != RendererImage || a.RendererVersion != HelmVersion || a.PolicyVersion != PolicyVersion ||
		!uuidRE.MatchString(a.CreatedBy) || !idempotencyRE.MatchString(a.IdempotencyKey) || a.CreatedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

func (a Approval) IdentityDigest() (string, error) {
	if a.Validate() != nil {
		return "", ErrInvalid
	}
	return digestJSON(struct {
		Contract           string `json:"contract"`
		ID                 string `json:"id"`
		Revision           int64  `json:"revision"`
		Repository         string `json:"repository"`
		Version            string `json:"version"`
		ManifestDigest     string `json:"manifestDigest"`
		PackageDigest      string `json:"packageDigest"`
		ValuesSchemaDigest string `json:"valuesSchemaDigest"`
		RendererImage      string `json:"rendererImage"`
		RendererVersion    string `json:"rendererVersion"`
		PolicyVersion      string `json:"policyVersion"`
	}{"helm-approval.v1", a.ID, a.Revision, a.OCIRepository, a.ChartVersion, a.ManifestDigest,
		a.PackageDigest, a.ValuesSchemaDigest, a.RendererImage, a.RendererVersion, a.PolicyVersion})
}

func (a Approval) replayEqual(other Approval) bool {
	left, leftErr := a.IdentityDigest()
	right, rightErr := other.IdentityDigest()
	return leftErr == nil && rightErr == nil && left == right && a.CreatedBy == other.CreatedBy && a.IdempotencyKey == other.IdempotencyKey
}

type DestinationIdentity struct {
	ProjectID       string
	EnvironmentID   string
	ApplicationID   string
	ApplicationSlug string
	Namespace       string
}

func (d DestinationIdentity) Validate() error {
	if !uuidRE.MatchString(d.ProjectID) || !uuidRE.MatchString(d.EnvironmentID) || !uuidRE.MatchString(d.ApplicationID) ||
		!dnsLabelRE.MatchString(d.ApplicationSlug) || !dnsLabelRE.MatchString(d.Namespace) {
		return ErrInvalid
	}
	return nil
}

// Descriptor is constructed from durable identities and Approval. There is no
// decoder for caller input by design: callers edit only values.yaml.
type Descriptor struct {
	Approval           ApprovalKey
	Repository         string
	Version            string
	ManifestDigest     string
	PackageDigest      string
	ValuesSchemaDigest string
	RendererImage      string
	RendererVersion    string
	PolicyVersion      string
	Destination        DestinationIdentity
	ReleaseName        string
}

func NewDescriptor(approval Approval, destination DestinationIdentity) (Descriptor, error) {
	if approval.Validate() != nil || destination.Validate() != nil {
		return Descriptor{}, ErrInvalid
	}
	descriptor := Descriptor{
		Approval: approval.ApprovalKey, Repository: approval.OCIRepository, Version: approval.ChartVersion,
		ManifestDigest: approval.ManifestDigest, PackageDigest: approval.PackageDigest,
		ValuesSchemaDigest: approval.ValuesSchemaDigest, RendererImage: approval.RendererImage,
		RendererVersion: approval.RendererVersion, PolicyVersion: approval.PolicyVersion,
		Destination: destination, ReleaseName: destination.ApplicationSlug,
	}
	if descriptor.Validate() != nil {
		return Descriptor{}, ErrInvalid
	}
	return descriptor, nil
}

func (d Descriptor) Validate() error {
	if d.Approval.Validate() != nil || d.Destination.Validate() != nil || d.ReleaseName != d.Destination.ApplicationSlug ||
		!canonicalOCIRepository(d.Repository) || !semverRE.MatchString(d.Version) || !validDigest(d.ManifestDigest) ||
		!validDigest(d.PackageDigest) || !validDigest(d.ValuesSchemaDigest) || d.RendererImage != RendererImage ||
		d.RendererVersion != HelmVersion || d.PolicyVersion != PolicyVersion {
		return ErrInvalid
	}
	return nil
}

func (d Descriptor) approvalIdentityDigest() (string, error) {
	if d.Validate() != nil {
		return "", ErrInvalid
	}
	return digestJSON(struct {
		Contract           string `json:"contract"`
		ID                 string `json:"id"`
		Revision           int64  `json:"revision"`
		Repository         string `json:"repository"`
		Version            string `json:"version"`
		ManifestDigest     string `json:"manifestDigest"`
		PackageDigest      string `json:"packageDigest"`
		ValuesSchemaDigest string `json:"valuesSchemaDigest"`
		RendererImage      string `json:"rendererImage"`
		RendererVersion    string `json:"rendererVersion"`
		PolicyVersion      string `json:"policyVersion"`
	}{"helm-approval.v1", d.Approval.ID, d.Approval.Revision, d.Repository, d.Version, d.ManifestDigest,
		d.PackageDigest, d.ValuesSchemaDigest, d.RendererImage, d.RendererVersion, d.PolicyVersion})
}

func (d Descriptor) RequiredLabels() map[string]string {
	if d.Validate() != nil {
		return nil
	}
	return map[string]string{
		"app.kubernetes.io/instance": d.ReleaseName,
		"app.kubernetes.io/name":     d.Destination.ApplicationSlug,
		"kuberploy.io/application":   d.Destination.ApplicationID,
		"kuberploy.io/environment":   d.Destination.EnvironmentID,
		"kuberploy.io/project":       d.Destination.ProjectID,
	}
}

func (d Descriptor) YAML() ([]byte, error) {
	if d.Validate() != nil {
		return nil, ErrInvalid
	}
	type metadata struct {
		ID   string `yaml:"id"`
		Name string `yaml:"name"`
	}
	type source struct {
		ApprovalID         string `yaml:"approvalId"`
		ApprovalRevision   int64  `yaml:"approvalRevision"`
		Repository         string `yaml:"repository"`
		Version            string `yaml:"version"`
		ManifestDigest     string `yaml:"manifestDigest"`
		PackageDigest      string `yaml:"packageDigest"`
		ValuesSchemaDigest string `yaml:"valuesSchemaDigest"`
		ValuesFile         string `yaml:"valuesFile"`
	}
	type renderer struct {
		Image         string `yaml:"image"`
		HelmVersion   string `yaml:"helmVersion"`
		PolicyVersion string `yaml:"policyVersion"`
	}
	type destination struct {
		Namespace   string `yaml:"namespace"`
		ReleaseName string `yaml:"releaseName"`
	}
	type spec struct {
		ProjectID      string            `yaml:"projectId"`
		EnvironmentID  string            `yaml:"environmentId"`
		ApplicationID  string            `yaml:"applicationId"`
		Source         source            `yaml:"source"`
		Renderer       renderer          `yaml:"renderer"`
		Destination    destination       `yaml:"destination"`
		IdentityLabels map[string]string `yaml:"identityLabels"`
	}
	document := struct {
		APIVersion string   `yaml:"apiVersion"`
		Kind       string   `yaml:"kind"`
		Metadata   metadata `yaml:"metadata"`
		Spec       spec     `yaml:"spec"`
	}{
		APIVersion: "config.kuberploy.io/v1alpha1", Kind: "ExternalHelmApplication",
		Metadata: metadata{ID: d.Destination.ApplicationID, Name: d.Destination.ApplicationSlug},
		Spec: spec{
			ProjectID: d.Destination.ProjectID, EnvironmentID: d.Destination.EnvironmentID,
			ApplicationID: d.Destination.ApplicationID,
			Source: source{d.Approval.ID, d.Approval.Revision, d.Repository, d.Version, d.ManifestDigest,
				d.PackageDigest, d.ValuesSchemaDigest, ValuesDocument},
			Renderer:    renderer{d.RendererImage, d.RendererVersion, d.PolicyVersion},
			Destination: destination{d.Destination.Namespace, d.ReleaseName}, IdentityLabels: d.RequiredLabels(),
		},
	}
	encoded, err := yaml.Marshal(document)
	if err != nil || len(encoded) == 0 || len(encoded) > MaximumDescriptorSize {
		return nil, ErrInvalid
	}
	return encoded, nil
}

type DesiredRender struct {
	ID               string
	IdempotencyScope string
	IdempotencyKey   string
	Approval         ApprovalKey
	Descriptor       Descriptor
	DescriptorYAML   []byte
	ValuesYAML       []byte
	DescriptorDigest string
	ValuesDigest     string
	InputDigest      string
}

func NewDesiredRender(id, scope, key string, approval Approval, destination DestinationIdentity, values []byte) (DesiredRender, error) {
	descriptor, err := NewDescriptor(approval, destination)
	if err != nil || !uuidRE.MatchString(id) || !uuidRE.MatchString(scope) || !idempotencyRE.MatchString(key) {
		return DesiredRender{}, ErrInvalid
	}
	parsed, err := ParseValues(values)
	if err != nil {
		return DesiredRender{}, err
	}
	descriptorYAML, err := descriptor.YAML()
	if err != nil {
		return DesiredRender{}, err
	}
	descriptorDigest := digestBytes(descriptorYAML)
	valuesDigest := digestBytes(parsed.Raw)
	approvalDigest, _ := approval.IdentityDigest()
	inputDigest, err := desiredInputDigest(approvalDigest, descriptorDigest, valuesDigest)
	if err != nil {
		return DesiredRender{}, ErrInvalid
	}
	return DesiredRender{ID: id, IdempotencyScope: scope, IdempotencyKey: key, Approval: approval.ApprovalKey,
		Descriptor: descriptor, DescriptorYAML: descriptorYAML, ValuesYAML: parsed.Raw,
		DescriptorDigest: descriptorDigest, ValuesDigest: valuesDigest, InputDigest: inputDigest}, nil
}

func (d DesiredRender) Validate() error {
	if !uuidRE.MatchString(d.ID) || !uuidRE.MatchString(d.IdempotencyScope) || !idempotencyRE.MatchString(d.IdempotencyKey) ||
		d.Approval.Validate() != nil || d.Descriptor.Validate() != nil || d.Descriptor.Approval != d.Approval ||
		d.DescriptorDigest != digestBytes(d.DescriptorYAML) || d.ValuesDigest != digestBytes(d.ValuesYAML) || !validDigest(d.InputDigest) {
		return ErrInvalid
	}
	expectedDescriptor, err := d.Descriptor.YAML()
	if err != nil || !equalBytes(expectedDescriptor, d.DescriptorYAML) {
		return ErrInvalid
	}
	if _, err = ParseValues(d.ValuesYAML); err != nil {
		return ErrInvalid
	}
	approvalDigest, err := d.Descriptor.approvalIdentityDigest()
	if err != nil {
		return ErrInvalid
	}
	expectedInput, err := desiredInputDigest(approvalDigest, d.DescriptorDigest, d.ValuesDigest)
	if err != nil || d.InputDigest != expectedInput {
		return ErrInvalid
	}
	return nil
}

func desiredInputDigest(approvalDigest, descriptorDigest, valuesDigest string) (string, error) {
	if !validDigest(approvalDigest) || !validDigest(descriptorDigest) || !validDigest(valuesDigest) {
		return "", ErrInvalid
	}
	return digestJSON(struct {
		Contract         string `json:"contract"`
		ApprovalDigest   string `json:"approvalDigest"`
		DescriptorDigest string `json:"descriptorDigest"`
		ValuesDigest     string `json:"valuesDigest"`
		LimitsDigest     string `json:"limitsDigest"`
	}{"external-helm-render-input.v1", approvalDigest, descriptorDigest, valuesDigest, LimitsDigest()})
}

type RenderState string

const (
	StateQueued     RenderState = "queued"
	StateProcessing RenderState = "processing"
	StateSucceeded  RenderState = "succeeded"
	StateFailed     RenderState = "failed"
)

type RuntimeIdentity struct {
	Contract        string
	RendererImage   string
	RendererVersion string
	PolicyVersion   string
	LimitsDigest    string
}

// RenderWorkerIdentity extends the immutable renderer implementation identity
// with the complete operator-owned Helm runtime configuration digest. The
// latter deliberately changes across rolling configuration updates even when
// the renderer binary and policy constants do not.
type RenderWorkerIdentity struct {
	RuntimeIdentity
	OperatorConfigDigest string
}

func ExpectedRenderWorkerIdentity(operatorConfigDigest string) RenderWorkerIdentity {
	return RenderWorkerIdentity{RuntimeIdentity: ExpectedRuntimeIdentity(), OperatorConfigDigest: operatorConfigDigest}
}

func (r RenderWorkerIdentity) Validate() error {
	if r.RuntimeIdentity.Validate() != nil || !validDigest(r.OperatorConfigDigest) {
		return ErrInvalid
	}
	return nil
}

func ExpectedRuntimeIdentity() RuntimeIdentity {
	return RuntimeIdentity{RendererContract, RendererImage, HelmVersion, PolicyVersion, LimitsDigest()}
}

func (r RuntimeIdentity) Validate() error {
	if r != ExpectedRuntimeIdentity() {
		return ErrInvalid
	}
	return nil
}

type RenderCommand struct {
	DesiredRender
	OperatorConfigDigest string
	State                RenderState
	AvailableAt          time.Time
	Attempts             int
	ConsecutiveFailures  int
	LastFailureCode      string
	LeaseOwner           string
	LeaseEpoch           int64
	LeaseUntil           *time.Time
	WorkerIdentity       RenderWorkerIdentity
	CreatedAt            time.Time
	UpdatedAt            time.Time
	CompletedAt          *time.Time
}

func (c RenderCommand) Validate() error {
	if c.DesiredRender.Validate() != nil || !validDigest(c.OperatorConfigDigest) ||
		(c.State != StateQueued && c.State != StateProcessing && c.State != StateSucceeded && c.State != StateFailed) ||
		c.AvailableAt.Before(c.CreatedAt) || c.UpdatedAt.Before(c.CreatedAt) || c.Attempts < 0 || c.Attempts > MaximumAttempts ||
		c.ConsecutiveFailures < 0 || c.ConsecutiveFailures > MaximumAttempts ||
		(c.LastFailureCode == "") != (c.ConsecutiveFailures == 0) || c.LastFailureCode != "" && !failureCodeRE.MatchString(c.LastFailureCode) {
		return ErrInvalid
	}
	leased := c.LeaseOwner != ""
	workerPresent := c.WorkerIdentity != (RenderWorkerIdentity{})
	if leased != (c.LeaseUntil != nil) || leased != workerPresent {
		return ErrInvalid
	}
	if leased && (!workerIDRE.MatchString(c.LeaseOwner) || c.LeaseEpoch <= 0 || c.WorkerIdentity.Validate() != nil ||
		c.WorkerIdentity.OperatorConfigDigest != c.OperatorConfigDigest ||
		c.LeaseUntil == nil || !c.LeaseUntil.After(c.UpdatedAt) || c.State != StateProcessing) {
		return ErrInvalid
	}
	terminal := c.State == StateSucceeded || c.State == StateFailed
	if terminal != (c.CompletedAt != nil) || terminal && c.CompletedAt.Before(c.CreatedAt) || terminal && leased ||
		c.State == StateQueued && leased || c.State == StateProcessing && !leased {
		return ErrInvalid
	}
	return nil
}

type RenderLease struct {
	Command RenderCommand
	Owner   string
	Epoch   int64
	Until   time.Time
}

func (l RenderLease) Validate(now time.Time) error {
	if l.Command.Validate() != nil || l.Command.State != StateProcessing || !workerIDRE.MatchString(l.Owner) || l.Epoch <= 0 ||
		!l.Until.After(now) || l.Command.LeaseOwner != l.Owner || l.Command.LeaseEpoch != l.Epoch ||
		l.Command.LeaseUntil == nil || !l.Command.LeaseUntil.Equal(l.Until) {
		return ErrInvalid
	}
	return nil
}

type RenderResult struct {
	CommandID            string
	OperatorConfigDigest string
	InputDigest          string
	ManifestDigest       string
	InventoryDigest      string
	RenderedManifests    []byte
	ResourceCount        int
	OutputBytes          int
	RendererImage        string
	RendererVersion      string
	PolicyVersion        string
	LimitsDigest         string
	CompletedAt          time.Time
}

func (r RenderResult) Validate(command RenderCommand) error {
	if command.DesiredRender.Validate() != nil || !uuidRE.MatchString(r.CommandID) || r.CommandID != command.ID ||
		r.OperatorConfigDigest != command.OperatorConfigDigest || !validDigest(r.OperatorConfigDigest) ||
		r.InputDigest != command.InputDigest || !validDigest(r.ManifestDigest) || r.ManifestDigest != digestBytes(r.RenderedManifests) ||
		!validDigest(r.InventoryDigest) || len(r.RenderedManifests) == 0 || len(r.RenderedManifests) > MaximumOutputSize ||
		r.ResourceCount < 1 || r.ResourceCount > MaximumResources || r.OutputBytes != len(r.RenderedManifests) ||
		r.RendererImage != RendererImage || r.RendererVersion != HelmVersion || r.PolicyVersion != PolicyVersion ||
		r.LimitsDigest != LimitsDigest() || r.CompletedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

type Readiness struct {
	WorkerID    string
	WorkerEpoch int64
	RenderWorkerIdentity
	StartedAt  time.Time
	ObservedAt time.Time
	LeaseUntil time.Time
}

func (r Readiness) Validate() error {
	if !workerIDRE.MatchString(r.WorkerID) || r.WorkerEpoch <= 0 || r.RenderWorkerIdentity.Validate() != nil ||
		r.StartedAt.IsZero() || r.ObservedAt.Before(r.StartedAt) || !r.LeaseUntil.After(r.ObservedAt) {
		return ErrInvalid
	}
	return nil
}

func LimitsDigest() string {
	value, _ := digestJSON(struct {
		Contract        string `json:"contract"`
		ValuesBytes     int    `json:"valuesBytes"`
		SchemaBytes     int    `json:"schemaBytes"`
		ChartBytes      int    `json:"chartBytes"`
		ExpandedBytes   int    `json:"expandedBytes"`
		ChartFiles      int    `json:"chartFiles"`
		FileBytes       int    `json:"fileBytes"`
		DescriptorBytes int    `json:"descriptorBytes"`
		YAMLNodes       int    `json:"yamlNodes"`
		YAMLDepth       int    `json:"yamlDepth"`
		TemporaryBytes  int    `json:"temporaryBytes"`
		OutputBytes     int    `json:"outputBytes"`
		ResourceCount   int    `json:"resourceCount"`
		TimeoutSeconds  int64  `json:"timeoutSeconds"`
		MaximumAttempts int    `json:"maximumAttempts"`
	}{"external-helm-limits.v1", MaximumValuesSize, MaximumSchemaSize, MaximumChartSize, MaximumExpandSize,
		MaximumChartFiles, MaximumFileSize, MaximumDescriptorSize, MaximumYAMLNodes, MaximumYAMLDepth,
		MaximumTemporarySize, MaximumOutputSize, MaximumResources, int64(RenderTimeout.Seconds()), MaximumAttempts})
	return value
}

func canonicalOCIRepository(value string) bool {
	return len(value) >= 12 && len(value) <= 512 && ociRepositoryRE.MatchString(value) && strings.ToLower(value) == value &&
		!strings.Contains(value, "//.") && !strings.HasSuffix(value, ".")
}

func validDigest(value string) bool {
	return len(value) == 71 && strings.HasPrefix(value, "sha256:") && strings.Trim(value[7:], "0123456789abcdef") == ""
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return digestBytes(encoded), nil
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var different byte
	for index := range left {
		different |= left[index] ^ right[index]
	}
	return different == 0
}
