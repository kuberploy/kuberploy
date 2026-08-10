package imagepull

import (
	"context"
	"errors"
	"sync"
	"time"
)

type MaterialReader interface {
	ReadDockerConfig(context.Context, Profile) ([]byte, error)
}

type SecretAPI interface {
	EnsureImagePullSecret(context.Context, SecretRequest) (SecretObservation, error)
}

type SecretRequest struct {
	DesiredArtifact
	RegistryServer string
	DockerConfig   []byte `json:"-"`
}

func (r SecretRequest) Validate(profile Profile) error {
	if r.DesiredArtifact.Validate() != nil || profile.Validate() != nil || r.ProfileName != profile.Name ||
		r.RegistryTargetID != profile.TargetID || r.PullCredentialRef != profile.CredentialRef ||
		r.ProfileRevision != profile.Revision || r.RegistryServer != profile.RegistryServer ||
		ValidateDockerConfig(r.DockerConfig, profile) != nil {
		return ErrInvalid
	}
	return nil
}

type SecretObservation struct {
	Namespace       string
	Name            string
	UID             string
	ResourceVersion string
}

func (o SecretObservation) Validate(request SecretRequest) error {
	if o.Namespace != request.Namespace || o.Name != request.SecretName || !kubernetesUIDPattern.MatchString(o.UID) ||
		!resourceVersionPattern.MatchString(o.ResourceVersion) {
		return ErrInvalid
	}
	return nil
}

type RuntimeController struct {
	Store       Store
	Reader      MaterialReader
	Secrets     SecretAPI
	Config      RuntimeConfig
	WorkerID    string
	WorkerEpoch int64
	Now         func() time.Time
	ReportError func(string, error)
}

func (c *RuntimeController) Validate() error {
	if c == nil || c.Store == nil || c.Reader == nil || c.Secrets == nil || c.Config.Validate() != nil ||
		!c.Config.Enabled || !workerIDPattern.MatchString(c.WorkerID) || c.WorkerEpoch <= 0 {
		return ErrUnavailable
	}
	return nil
}

func (c *RuntimeController) Run(ctx context.Context) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := c.preflightProjectedCredentials(ctx); err != nil {
		return err
	}
	digest, err := c.Config.Digest()
	if err != nil {
		return ErrUnavailable
	}
	startedAt := c.now()
	if err = c.Store.RecordReadiness(ctx, Readiness{WorkerID: c.WorkerID, WorkerEpoch: c.WorkerEpoch,
		Contract: RuntimeContract, ConfigDigest: digest, ProfileCount: len(c.Config.Profiles),
		StartedAt: startedAt, ObservedAt: startedAt, LeaseUntil: startedAt.Add(c.Config.ReadinessMaxAge)}); err != nil {
		return err
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	readinessDone := make(chan error, 1)
	go func() {
		readinessDone <- c.runReadiness(runContext, digest, startedAt)
		cancel()
	}()
	reconcileErr := c.runReconciliation(runContext, digest)
	cancel()
	readinessErr := <-readinessDone
	if reconcileErr != nil && !errors.Is(reconcileErr, context.Canceled) {
		return reconcileErr
	}
	if readinessErr != nil && !errors.Is(readinessErr, context.Canceled) {
		return readinessErr
	}
	return ctx.Err()
}

// preflightProjectedCredentials proves that every advertised operator profile
// is mounted and structurally usable before the first readiness heartbeat.
// It deliberately does not retain a parsed credential or any credential hash.
func (c *RuntimeController) preflightProjectedCredentials(ctx context.Context) error {
	for _, profile := range c.Config.Profiles {
		material, err := c.Reader.ReadDockerConfig(ctx, profile)
		if err != nil {
			clearBytes(material)
			if c.ReportError != nil {
				c.ReportError("credential-source-unavailable", ErrUnavailable)
			}
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			return ErrUnavailable
		}
		valid := ValidateDockerConfig(material, profile) == nil
		clearBytes(material)
		if !valid {
			if c.ReportError != nil {
				c.ReportError("credential-source-invalid", ErrInvalid)
			}
			return ErrUnavailable
		}
	}
	return nil
}

func (c *RuntimeController) runReadiness(ctx context.Context, digest string, startedAt time.Time) error {
	ticker := time.NewTicker(c.Config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			now := c.now()
			if err := c.Store.RecordReadiness(ctx, Readiness{WorkerID: c.WorkerID, WorkerEpoch: c.WorkerEpoch,
				Contract: RuntimeContract, ConfigDigest: digest, ProfileCount: len(c.Config.Profiles),
				StartedAt: startedAt, ObservedAt: now, LeaseUntil: now.Add(c.Config.ReadinessMaxAge)}); err != nil {
				return err
			}
		}
	}
}

func (c *RuntimeController) runReconciliation(ctx context.Context, digest string) error {
	for {
		didWork, err := c.Reconcile(ctx, digest)
		if err != nil {
			return err
		}
		if didWork {
			continue
		}
		timer := time.NewTimer(c.Config.MinimumBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *RuntimeController) Reconcile(ctx context.Context, configDigest string) (bool, error) {
	if c.Validate() != nil || !digestPattern(configDigest) {
		return false, ErrUnavailable
	}
	expectedDigest, err := c.Config.Digest()
	if err != nil || configDigest != expectedDigest {
		return false, ErrUnavailable
	}
	lease, found, err := c.Store.ClaimArtifact(ctx, c.WorkerID, RuntimeContract, configDigest, c.now(), c.Config.WorkLease)
	if err != nil || !found {
		return found, err
	}
	profile, configured := c.Config.ProfileForTarget(lease.Artifact.RegistryTargetID)
	if !configured || !c.Config.AllowsNamespace(lease.Artifact.Namespace) || profile.Name != lease.Artifact.ProfileName ||
		profile.CredentialRef != lease.Artifact.PullCredentialRef || profile.Revision != lease.Artifact.ProfileRevision ||
		lease.Artifact.SecretName != SecretName(lease.Artifact.Namespace, lease.Artifact.RegistryTargetID, profile.Revision) {
		return c.permanentFailure(ctx, lease, "profile-mismatch")
	}
	workContext, heartbeat := c.startHeartbeat(ctx, lease)
	material, readErr := c.Reader.ReadDockerConfig(workContext, profile)
	if readErr != nil {
		latest, heartbeatErr := heartbeat.stop()
		clearBytes(material)
		if heartbeatErr != nil {
			return true, heartbeatErr
		}
		return c.infrastructureFailure(ctx, latest, "credential-source-unavailable")
	}
	request := SecretRequest{DesiredArtifact: lease.Artifact.DesiredArtifact, RegistryServer: profile.RegistryServer, DockerConfig: material}
	if request.Validate(profile) != nil {
		latest, heartbeatErr := heartbeat.stop()
		clearBytes(material)
		if heartbeatErr != nil {
			return true, heartbeatErr
		}
		return c.permanentFailure(ctx, latest, "credential-source-invalid")
	}
	observation, ensureErr := c.Secrets.EnsureImagePullSecret(workContext, request)
	latest, heartbeatErr := heartbeat.stop()
	clearBytes(material)
	request.DockerConfig = nil
	if heartbeatErr != nil {
		return true, heartbeatErr
	}
	if ensureErr != nil {
		if errors.Is(ensureErr, ErrConflict) || errors.Is(ensureErr, ErrInvalid) {
			return c.permanentFailure(ctx, latest, "secret-mutation")
		}
		return c.infrastructureFailure(ctx, latest, "kubernetes-unavailable")
	}
	if observation.Validate(request) != nil {
		return c.permanentFailure(ctx, latest, "secret-observation-mismatch")
	}
	now := c.now()
	_, err = c.Store.RecordArtifactReady(ctx, latest, observation.UID, observation.ResourceVersion, now, now.Add(c.Config.PollInterval))
	return true, err
}

func (c *RuntimeController) permanentFailure(ctx context.Context, lease Lease, code string) (bool, error) {
	now := c.now()
	_, err := c.Store.RecordArtifactRetry(context.WithoutCancel(ctx), lease, code, true, now.Add(c.Config.MaximumBackoff), now)
	if err == nil && c.ReportError != nil {
		c.ReportError(code, ErrInvalid)
	}
	return true, err
}

func (c *RuntimeController) infrastructureFailure(ctx context.Context, lease Lease, code string) (bool, error) {
	now := c.now()
	failures := min(30, lease.Artifact.ConsecutiveFailures+1)
	next := now.Add(exponentialBackoff(c.Config.MinimumBackoff, c.Config.MaximumBackoff, failures))
	if _, err := c.Store.RecordArtifactRetry(context.WithoutCancel(ctx), lease, code, false, next, now); err != nil {
		return true, err
	}
	if c.ReportError != nil {
		c.ReportError(code, ErrUnavailable)
	}
	// Credential projection and Kubernetes API failures affect the controller
	// boundary as a whole. Exit so the readiness lease expires instead of
	// advertising a worker that cannot safely materialize pull credentials.
	return true, ErrUnavailable
}

func exponentialBackoff(minimum, maximum time.Duration, failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := minimum
	for index := 1; index < failures && delay < maximum/2; index++ {
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func (c *RuntimeController) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

type workHeartbeat struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	lease  Lease
	err    error
}

func (c *RuntimeController) startHeartbeat(parent context.Context, lease Lease) (context.Context, *workHeartbeat) {
	ctx, cancel := context.WithCancel(parent)
	heartbeat := &workHeartbeat{cancel: cancel, done: make(chan struct{}), lease: lease}
	go func() {
		defer close(heartbeat.done)
		ticker := time.NewTicker(c.Config.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				heartbeat.mu.Lock()
				current := heartbeat.lease
				heartbeat.mu.Unlock()
				updated, err := c.Store.HeartbeatArtifact(ctx, current, c.now(), c.Config.WorkLease)
				if err != nil {
					heartbeat.mu.Lock()
					heartbeat.err = err
					heartbeat.mu.Unlock()
					cancel()
					return
				}
				heartbeat.mu.Lock()
				heartbeat.lease = updated
				heartbeat.mu.Unlock()
			}
		}
	}()
	return ctx, heartbeat
}

func (h *workHeartbeat) stop() (Lease, error) {
	h.cancel()
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lease, h.err
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
