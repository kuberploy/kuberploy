package upgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/releases"
)

const (
	serviceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"
	jobSpecDigestAnnotation = "kuberploy.io/spec-sha256"
	manifestAnnotation      = "kuberploy.io/manifest-digest"
	versionAnnotation       = "kuberploy.io/target-version"
	operationLabel          = "kuberploy.io/operation-id"
	actionAnnotation        = "kuberploy.io/upgrade-action"
	helmRevisionAnnotation  = "kuberploy.io/helm-revision"
)

var (
	errJobNotFound = errors.New("Kubernetes Job not found")
	errJobConflict = errors.New("Kubernetes Job already exists")
	uuidRE         = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	dnsLabelRE     = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
	kubeVersionRE  = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)(?:[.+-]|$)`)
)

// JobAPI is the deliberately narrow Kubernetes seam used by the upgrade
// runner. Implementations can only get or create namespaced Jobs.
type JobAPI interface {
	ServerVersion(context.Context) (string, error)
	GetJob(context.Context, string, string) (KubernetesJob, error)
	CreateJob(context.Context, string, KubernetesJob) (KubernetesJob, error)
}

type KubernetesRunner struct {
	Jobs                  JobAPI
	Namespace             string
	ServiceAccount        string
	ReleaseName           string
	ActiveDeadlineSeconds int64
	PollInterval          time.Duration
}

func (r KubernetesRunner) Run(ctx context.Context, request ExecutableRequest) (Result, error) {
	desired, manifest, err := r.desiredJobAndManifest(request)
	if err != nil {
		return Result{}, err
	}
	serverVersion, err := r.Jobs.ServerVersion(ctx)
	if err != nil {
		return reconcilePending(request.JobName, "KubernetesVersionUnavailable", fmt.Sprintf("read Kubernetes server version: %v", err)), nil
	}
	if !supportedKubernetesVersion(serverVersion) {
		return Result{}, fmt.Errorf("Kubernetes server version %q is outside manifest constraint %s", serverVersion, manifest.Compatibility.Kubernetes.Constraint)
	}
	job, err := r.Jobs.GetJob(ctx, request.Namespace, request.JobName)
	if errors.Is(err, errJobNotFound) {
		job, err = r.Jobs.CreateJob(ctx, request.Namespace, desired)
		if errors.Is(err, errJobConflict) {
			job, err = r.Jobs.GetJob(ctx, request.Namespace, request.JobName)
		}
	}
	if err != nil {
		return reconcilePending(request.JobName, "JobReconcileUnavailable", fmt.Sprintf("reconcile Kubernetes upgrade Job: %v", err)), nil
	}
	if err = validateExistingJob(job, desired); err != nil {
		return Result{RunnerRef: request.JobName}, err
	}
	poll := r.PollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		if result, terminal, terminalErr := terminalJobResult(job); terminal {
			return result, terminalErr
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return reconcilePending(request.JobName, "JobObservationInterrupted", fmt.Sprintf("wait for Kubernetes upgrade Job: %v", ctx.Err())), nil
		case <-timer.C:
		}
		job, err = r.Jobs.GetJob(ctx, request.Namespace, request.JobName)
		if err != nil {
			return reconcilePending(request.JobName, "JobObservationUnavailable", fmt.Sprintf("re-read Kubernetes upgrade Job: %v", err)), nil
		}
		if err = validateExistingJob(job, desired); err != nil {
			return Result{RunnerRef: request.JobName}, err
		}
	}
}

func (r KubernetesRunner) desiredJob(request ExecutableRequest) (KubernetesJob, error) {
	job, _, err := r.desiredJobAndManifest(request)
	return job, err
}

func (r KubernetesRunner) desiredJobAndManifest(request ExecutableRequest) (KubernetesJob, domain.ReleaseManifest, error) {
	if r.Jobs == nil {
		return KubernetesJob{}, domain.ReleaseManifest{}, errors.New("Kubernetes Job API is not configured")
	}
	if request.Namespace != r.Namespace || request.ReleaseName != r.ReleaseName {
		return KubernetesJob{}, domain.ReleaseManifest{}, errors.New("upgrade request namespace or Helm release does not match runner configuration")
	}
	if !uuidRE.MatchString(request.OperationID) || request.Generation < 1 || request.JobName != JobName(request.OperationID, request.Generation) {
		return KubernetesJob{}, domain.ReleaseManifest{}, errors.New("upgrade request has an invalid operation identity")
	}
	if !validDNSLabel(request.Namespace, 63) || !validDNSLabel(request.ReleaseName, 53) || !validDNSLabel(r.ServiceAccount, 63) {
		return KubernetesJob{}, domain.ReleaseManifest{}, errors.New("upgrade runner namespace, release, or ServiceAccount is invalid")
	}
	manifest, err := releases.ParseExactManifest(request.ManifestBytes, request.ManifestDigest)
	if err != nil {
		return KubernetesJob{}, domain.ReleaseManifest{}, fmt.Errorf("validate exact persisted release manifest: %w", err)
	}
	if request.TargetVersion != manifest.Release.Version {
		return KubernetesJob{}, domain.ReleaseManifest{}, errors.New("upgrade target does not match its verified manifest identity")
	}
	if request.Action == "" {
		request.Action = "upgrade"
	}
	if request.Action != "upgrade" && request.Action != "rollback" {
		return KubernetesJob{}, domain.ReleaseManifest{}, errors.New("upgrade request has an invalid action")
	}
	if request.Action == "rollback" && (request.HelmRevision < 1 || request.HelmRevision > 1_000_000) {
		return KubernetesJob{}, domain.ReleaseManifest{}, errors.New("rollback request has an invalid Helm revision")
	}
	if request.Action == "upgrade" && request.HelmRevision != 0 {
		return KubernetesJob{}, domain.ReleaseManifest{}, errors.New("upgrade request must not select a Helm revision")
	}
	deadline := r.ActiveDeadlineSeconds
	if deadline == 0 {
		deadline = 900
	}
	if deadline < 60 || deadline > 900 {
		return KubernetesJob{}, domain.ReleaseManifest{}, errors.New("upgrade active deadline must be between 60 and 900 seconds")
	}
	images := manifest.Artifacts.Images
	imageRefs := make(map[string]string, len(images))
	for _, image := range images {
		imageRefs[image.Component] = image.Reference + "@" + image.Digest
	}
	chart := manifest.Artifacts.Chart
	chartRepository := strings.TrimSuffix(chart.OCIReference, ":"+chart.Version)
	chartReference := "oci://" + chartRepository + "@" + chart.OCIDigest
	timeout := strconv.FormatInt(deadline-30, 10) + "s"
	args := []string{"rollback", request.ReleaseName, strconv.FormatInt(request.HelmRevision, 10), "--namespace", request.Namespace, "--wait", "--wait-for-jobs", "--cleanup-on-fail", "--timeout", timeout}
	if request.Action == "upgrade" {
		args = []string{
			"upgrade", request.ReleaseName, chartReference,
			"--namespace", request.Namespace,
			"--reuse-values",
			"--set-string", "global.requireImageDigest=true",
			"--set-string", "components.api.image.reference=" + imageRefs["api"],
			"--set-string", "components.worker.image.reference=" + imageRefs["worker"],
			"--set-string", "components.web.image.reference=" + imageRefs["web"],
			"--set-string", "components.migration.image.reference=" + imageRefs["migration"],
			"--set-string", "upgrade.image.reference=" + imageRefs["upgrader"],
			"--set-string", "builder.builderAgentImage=" + imageRefs["builder-agent"],
			"--wait", "--wait-for-jobs", "--cleanup-on-fail", "--rollback-on-failure",
			"--timeout", timeout, "--history-max", "10",
		}
	}
	falseValue, trueValue := false, true
	runAsUser := int64(65532)
	backoff := int32(0)
	tokenExpiration := int64(900)
	terminationGrace := int64(30)
	job := KubernetesJob{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: ObjectMeta{
			Name:      request.JobName,
			Namespace: request.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kuberploy",
				"app.kubernetes.io/instance":   request.ReleaseName,
				"app.kubernetes.io/component":  "upgrade",
				"app.kubernetes.io/managed-by": "kuberploy-worker",
				operationLabel:                 request.OperationID,
			},
			Annotations: map[string]string{
				manifestAnnotation: request.ManifestDigest,
				versionAnnotation:  request.TargetVersion,
				actionAnnotation:   request.Action,
			},
		},
		Spec: JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: PodTemplateSpec{
				Metadata: ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "kuberploy", "app.kubernetes.io/instance": request.ReleaseName, "app.kubernetes.io/component": "upgrade", operationLabel: request.OperationID}},
				Spec: PodSpec{
					RestartPolicy:                 "Never",
					ServiceAccountName:            r.ServiceAccount,
					AutomountServiceAccountToken:  &falseValue,
					EnableServiceLinks:            &falseValue,
					TerminationGracePeriodSeconds: &terminationGrace,
					SecurityContext:               PodSecurityContext{RunAsNonRoot: &trueValue, RunAsUser: &runAsUser, RunAsGroup: &runAsUser, SeccompProfile: &SeccompProfile{Type: "RuntimeDefault"}},
					Containers: []Container{{
						Name:            "upgrade",
						Image:           imageRefs["upgrader"],
						ImagePullPolicy: "IfNotPresent",
						Args:            args,
						Env: []EnvVar{
							{Name: "HOME", Value: "/tmp/helm"},
							{Name: "HELM_CACHE_HOME", Value: "/tmp/helm/cache"},
							{Name: "HELM_CONFIG_HOME", Value: "/tmp/helm/config"},
							{Name: "HELM_DATA_HOME", Value: "/tmp/helm/data"},
						},
						SecurityContext: SecurityContext{AllowPrivilegeEscalation: &falseValue, Privileged: &falseValue, ReadOnlyRootFilesystem: &trueValue, RunAsNonRoot: &trueValue, Capabilities: Capabilities{Drop: []string{"ALL"}}},
						VolumeMounts: []VolumeMount{
							{Name: "helm-home", MountPath: "/tmp/helm"},
							{Name: "kube-api-access", MountPath: serviceAccountDirectory, ReadOnly: true},
						},
						Resources: ResourceRequirements{Requests: map[string]string{"cpu": "25m", "memory": "64Mi"}, Limits: map[string]string{"cpu": "500m", "memory": "256Mi"}},
					}},
					Volumes: []Volume{
						{Name: "helm-home", EmptyDir: &EmptyDirVolumeSource{}},
						{Name: "kube-api-access", Projected: &ProjectedVolumeSource{DefaultMode: int32Pointer(0o420), Sources: []VolumeProjection{
							{ServiceAccountToken: &ServiceAccountTokenProjection{Audience: "https://kubernetes.default.svc.cluster.local", ExpirationSeconds: &tokenExpiration, Path: "token"}},
							{ConfigMap: &ConfigMapProjection{Name: "kube-root-ca.crt", Items: []KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
							{DownwardAPI: &DownwardAPIProjection{Items: []DownwardAPIVolumeFile{{Path: "namespace", FieldRef: ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
						}}},
					},
				},
			},
		},
	}
	if request.Action == "rollback" {
		job.Metadata.Annotations[helmRevisionAnnotation] = strconv.FormatInt(request.HelmRevision, 10)
	}
	fingerprintBody, err := json.Marshal(job.Spec)
	if err != nil {
		return KubernetesJob{}, domain.ReleaseManifest{}, err
	}
	fingerprint := sha256.Sum256(fingerprintBody)
	job.Metadata.Annotations[jobSpecDigestAnnotation] = "sha256:" + hex.EncodeToString(fingerprint[:])
	return job, manifest, nil
}

func reconcilePending(jobName, code, detail string) Result {
	return Result{RunnerRef: jobName, Pending: true, Details: map[string]any{"code": code, "detail": detail}}
}

func validateExistingJob(actual, desired KubernetesJob) error {
	if actual.APIVersion != desired.APIVersion || actual.Kind != desired.Kind || actual.Metadata.Name != desired.Metadata.Name || actual.Metadata.Namespace != desired.Metadata.Namespace {
		return errors.New("existing upgrade Job identity does not match the durable operation")
	}
	for key, value := range desired.Metadata.Labels {
		if actual.Metadata.Labels[key] != value {
			return fmt.Errorf("existing upgrade Job label %s does not match", key)
		}
	}
	for key, value := range desired.Metadata.Annotations {
		if actual.Metadata.Annotations[key] != value {
			return fmt.Errorf("existing upgrade Job annotation %s does not match", key)
		}
	}
	for key, value := range desired.Spec.Template.Metadata.Labels {
		if actual.Spec.Template.Metadata.Labels[key] != value {
			return fmt.Errorf("existing upgrade Job template label %s does not match", key)
		}
	}
	for key := range actual.Spec.Template.Metadata.Labels {
		if _, wanted := desired.Spec.Template.Metadata.Labels[key]; !wanted && !knownControllerTemplateLabel(key) {
			return fmt.Errorf("existing upgrade Job has unexpected template label %s", key)
		}
	}
	for key, value := range desired.Spec.Template.Metadata.Annotations {
		if actual.Spec.Template.Metadata.Annotations[key] != value {
			return fmt.Errorf("existing upgrade Job template annotation %s does not match", key)
		}
	}
	for key := range actual.Spec.Template.Metadata.Annotations {
		if _, wanted := desired.Spec.Template.Metadata.Annotations[key]; !wanted && key != "batch.kubernetes.io/job-tracking" {
			return fmt.Errorf("existing upgrade Job has unexpected template annotation %s", key)
		}
	}
	// Kubernetes injects the allowlisted controller metadata above. Compare the
	// entire remaining security- and execution-relevant spec.
	actualSpec := actual.Spec
	actualSpec.Template.Metadata = desired.Spec.Template.Metadata
	if !reflect.DeepEqual(actualSpec, desired.Spec) {
		return errors.New("existing upgrade Job spec differs from the verified durable operation")
	}
	return nil
}

func knownControllerTemplateLabel(key string) bool {
	switch key {
	case "batch.kubernetes.io/controller-uid", "batch.kubernetes.io/job-name", "controller-uid", "job-name":
		return true
	default:
		return false
	}
}

func terminalJobResult(job KubernetesJob) (Result, bool, error) {
	for _, condition := range job.Status.Conditions {
		if condition.Status != "True" {
			continue
		}
		switch condition.Type {
		case "Complete":
			return Result{RunnerRef: job.Metadata.Name, Details: map[string]any{"namespace": job.Metadata.Namespace, "completionTime": job.Status.CompletionTime}}, true, nil
		case "Failed":
			detail := strings.TrimSpace(strings.Join([]string{condition.Reason, condition.Message}, ": "))
			return Result{RunnerRef: job.Metadata.Name}, true, fmt.Errorf("Kubernetes upgrade Job %s failed: %s", job.Metadata.Name, detail)
		}
	}
	if job.Status.Succeeded > 0 {
		return Result{RunnerRef: job.Metadata.Name, Details: map[string]any{"namespace": job.Metadata.Namespace, "completionTime": job.Status.CompletionTime}}, true, nil
	}
	return Result{}, false, nil
}

func validDNSLabel(value string, maximum int) bool {
	return len(value) > 0 && len(value) <= maximum && dnsLabelRE.MatchString(value)
}

func supportedKubernetesVersion(value string) bool {
	parts := kubeVersionRE.FindStringSubmatch(strings.TrimSpace(value))
	if parts == nil || parts[1] != "1" {
		return false
	}
	minor, err := strconv.Atoi(parts[2])
	return err == nil && minor >= 34 && minor < 37
}

func int32Pointer(value int32) *int32 { return &value }

// The following small structs model only fields Kuberploy owns. Unknown
// Kubernetes defaulted fields are intentionally ignored when decoding.
type KubernetesJob struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       JobSpec    `json:"spec"`
	Status     JobStatus  `json:"status,omitempty"`
}
type ObjectMeta struct {
	Name        string            `json:"name,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}
type JobSpec struct {
	BackoffLimit          *int32          `json:"backoffLimit,omitempty"`
	ActiveDeadlineSeconds *int64          `json:"activeDeadlineSeconds,omitempty"`
	Template              PodTemplateSpec `json:"template"`
}
type PodTemplateSpec struct {
	Metadata ObjectMeta `json:"metadata,omitempty"`
	Spec     PodSpec    `json:"spec"`
}
type PodSpec struct {
	RestartPolicy                 string             `json:"restartPolicy"`
	ServiceAccountName            string             `json:"serviceAccountName"`
	AutomountServiceAccountToken  *bool              `json:"automountServiceAccountToken"`
	EnableServiceLinks            *bool              `json:"enableServiceLinks"`
	TerminationGracePeriodSeconds *int64             `json:"terminationGracePeriodSeconds"`
	SecurityContext               PodSecurityContext `json:"securityContext"`
	Containers                    []Container        `json:"containers"`
	Volumes                       []Volume           `json:"volumes"`
}
type PodSecurityContext struct {
	RunAsNonRoot   *bool           `json:"runAsNonRoot"`
	RunAsUser      *int64          `json:"runAsUser"`
	RunAsGroup     *int64          `json:"runAsGroup"`
	SeccompProfile *SeccompProfile `json:"seccompProfile"`
}
type SeccompProfile struct {
	Type string `json:"type"`
}
type Container struct {
	Name            string               `json:"name"`
	Image           string               `json:"image"`
	ImagePullPolicy string               `json:"imagePullPolicy"`
	Args            []string             `json:"args"`
	Env             []EnvVar             `json:"env"`
	SecurityContext SecurityContext      `json:"securityContext"`
	VolumeMounts    []VolumeMount        `json:"volumeMounts"`
	Resources       ResourceRequirements `json:"resources"`
}
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type SecurityContext struct {
	AllowPrivilegeEscalation *bool        `json:"allowPrivilegeEscalation"`
	Privileged               *bool        `json:"privileged"`
	ReadOnlyRootFilesystem   *bool        `json:"readOnlyRootFilesystem"`
	RunAsNonRoot             *bool        `json:"runAsNonRoot"`
	Capabilities             Capabilities `json:"capabilities"`
}
type Capabilities struct {
	Drop []string `json:"drop"`
}
type ResourceRequirements struct {
	Requests map[string]string `json:"requests"`
	Limits   map[string]string `json:"limits"`
}
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}
type Volume struct {
	Name      string                 `json:"name"`
	EmptyDir  *EmptyDirVolumeSource  `json:"emptyDir,omitempty"`
	Projected *ProjectedVolumeSource `json:"projected,omitempty"`
}
type EmptyDirVolumeSource struct{}
type ProjectedVolumeSource struct {
	Sources     []VolumeProjection `json:"sources"`
	DefaultMode *int32             `json:"defaultMode,omitempty"`
}
type VolumeProjection struct {
	ServiceAccountToken *ServiceAccountTokenProjection `json:"serviceAccountToken,omitempty"`
	ConfigMap           *ConfigMapProjection           `json:"configMap,omitempty"`
	DownwardAPI         *DownwardAPIProjection         `json:"downwardAPI,omitempty"`
}
type ServiceAccountTokenProjection struct {
	Audience          string `json:"audience"`
	ExpirationSeconds *int64 `json:"expirationSeconds"`
	Path              string `json:"path"`
}
type ConfigMapProjection struct {
	Name  string      `json:"name"`
	Items []KeyToPath `json:"items"`
}
type KeyToPath struct {
	Key  string `json:"key"`
	Path string `json:"path"`
}
type DownwardAPIProjection struct {
	Items []DownwardAPIVolumeFile `json:"items"`
}
type DownwardAPIVolumeFile struct {
	Path     string              `json:"path"`
	FieldRef ObjectFieldSelector `json:"fieldRef"`
}
type ObjectFieldSelector struct {
	APIVersion string `json:"apiVersion"`
	FieldPath  string `json:"fieldPath"`
}
type JobStatus struct {
	Succeeded      int32          `json:"succeeded,omitempty"`
	Failed         int32          `json:"failed,omitempty"`
	CompletionTime string         `json:"completionTime,omitempty"`
	Conditions     []JobCondition `json:"conditions,omitempty"`
}
type JobCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type InClusterJobAPI struct {
	baseURL   string
	http      *http.Client
	tokenPath string
}

func NewInClusterJobAPI() (*InClusterJobAPI, string, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	if host == "" || port == "" {
		return nil, "", errors.New("Kubernetes in-cluster service environment is missing")
	}
	caPEM, err := os.ReadFile(serviceAccountDirectory + "/ca.crt")
	if err != nil {
		return nil, "", fmt.Errorf("read Kubernetes service CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, "", errors.New("Kubernetes service CA is invalid")
	}
	namespaceBytes, err := os.ReadFile(serviceAccountDirectory + "/namespace")
	if err != nil {
		return nil, "", fmt.Errorf("read pod namespace: %w", err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Kubernetes API redirects are not allowed")
		},
	}
	return &InClusterJobAPI{baseURL: "https://" + net.JoinHostPort(host, port), http: client, tokenPath: serviceAccountDirectory + "/token"}, strings.TrimSpace(string(namespaceBytes)), nil
}

func (c *InClusterJobAPI) GetJob(ctx context.Context, namespace, name string) (KubernetesJob, error) {
	return c.request(ctx, http.MethodGet, "/apis/batch/v1/namespaces/"+url.PathEscape(namespace)+"/jobs/"+url.PathEscape(name), nil)
}
func (c *InClusterJobAPI) ServerVersion(ctx context.Context) (string, error) {
	token, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return "", fmt.Errorf("read Kubernetes service account token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/version", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	response, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil {
		return "", err
	}
	if len(body) > 64<<10 {
		return "", errors.New("Kubernetes version response exceeded 64 KiB")
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Kubernetes version endpoint returned HTTP %d", response.StatusCode)
	}
	var result struct {
		GitVersion string `json:"gitVersion"`
	}
	if err = json.Unmarshal(body, &result); err != nil || result.GitVersion == "" {
		return "", errors.New("Kubernetes version endpoint returned an invalid response")
	}
	return result.GitVersion, nil
}
func (c *InClusterJobAPI) CreateJob(ctx context.Context, namespace string, job KubernetesJob) (KubernetesJob, error) {
	body, err := json.Marshal(job)
	if err != nil {
		return KubernetesJob{}, err
	}
	return c.request(ctx, http.MethodPost, "/apis/batch/v1/namespaces/"+url.PathEscape(namespace)+"/jobs", body)
}
func (c *InClusterJobAPI) request(ctx context.Context, method, path string, body []byte) (KubernetesJob, error) {
	token, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return KubernetesJob{}, fmt.Errorf("read Kubernetes service account token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return KubernetesJob{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return KubernetesJob{}, err
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if readErr != nil {
		return KubernetesJob{}, readErr
	}
	if len(responseBody) > 1<<20 {
		return KubernetesJob{}, errors.New("Kubernetes API response exceeded 1 MiB")
	}
	if response.StatusCode == http.StatusNotFound {
		return KubernetesJob{}, errJobNotFound
	}
	if response.StatusCode == http.StatusConflict {
		return KubernetesJob{}, errJobConflict
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail := strings.TrimSpace(string(responseBody))
		if len(detail) > 2048 {
			detail = detail[:2048]
		}
		return KubernetesJob{}, fmt.Errorf("Kubernetes API returned HTTP %d: %s", response.StatusCode, detail)
	}
	var job KubernetesJob
	if err = json.Unmarshal(responseBody, &job); err != nil {
		return KubernetesJob{}, fmt.Errorf("decode Kubernetes Job: %w", err)
	}
	return job, nil
}
