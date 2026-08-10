package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/kuberploy/kuberploy/internal/domain"
)

type Result struct {
	RunnerRef string         `json:"runnerRef"`
	Pending   bool           `json:"pending,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}
type Runner interface {
	Run(context.Context, domain.Operation, domain.PlatformUpgrade) (Result, error)
}

type Unavailable struct{}

func (Unavailable) Run(context.Context, domain.Operation, domain.PlatformUpgrade) (Result, error) {
	return Result{}, errors.New("Kubernetes upgrade Job adapter is not configured")
}

type Executable struct{ Path, Namespace, ReleaseName string }

// ExecutableRequest is the closed worker-to-runner protocol. All mutable
// artifact inputs come from the already-verified, durably persisted manifest;
// the protocol intentionally has no URL or arbitrary command field.
type ExecutableRequest struct {
	OperationID    string `json:"operationId"`
	Generation     int64  `json:"generation"`
	JobName        string `json:"jobName"`
	Namespace      string `json:"namespace"`
	ReleaseName    string `json:"releaseName"`
	TargetVersion  string `json:"targetVersion"`
	ManifestDigest string `json:"manifestDigest"`
	ManifestBytes  []byte `json:"manifestBytes"`
}

func (e Executable) Run(ctx context.Context, op domain.Operation, u domain.PlatformUpgrade) (Result, error) {
	if strings.TrimSpace(e.Path) == "" {
		return Result{}, errors.New("upgrade runner executable path is empty")
	}
	jobName := JobName(op.ID, op.Generation)
	request := ExecutableRequest{OperationID: op.ID, Generation: op.Generation, JobName: jobName, Namespace: e.Namespace, ReleaseName: e.ReleaseName, TargetVersion: u.Version, ManifestDigest: u.ManifestDigest, ManifestBytes: append([]byte(nil), u.ManifestBytes...)}
	body, err := json.Marshal(request)
	if err != nil {
		return Result{}, err
	}
	cmd := exec.CommandContext(ctx, e.Path)
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr limitedBuffer
	stdout.limit = 64 << 10
	stderr.limit = 16 << 10
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Result{RunnerRef: jobName, Pending: true, Details: map[string]any{"code": "RunnerInterrupted", "detail": ctx.Err().Error()}}, nil
		}
		return Result{RunnerRef: jobName}, fmt.Errorf("upgrade runner failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	var result Result
	if err = decoder.Decode(&result); err != nil {
		return Result{RunnerRef: jobName}, fmt.Errorf("decode upgrade runner result: %w", err)
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		return Result{RunnerRef: jobName}, errors.New("upgrade runner returned multiple JSON values")
	}
	if result.RunnerRef != jobName {
		return Result{RunnerRef: jobName}, fmt.Errorf("upgrade runner returned %q, expected deterministic Job %q", result.RunnerRef, jobName)
	}
	if result.Details == nil {
		result.Details = map[string]any{}
	}
	return result, nil
}
func JobName(operationID string, generation int64) string {
	compact := strings.ReplaceAll(operationID, "-", "")
	if len(compact) > 12 {
		compact = compact[:12]
	}
	return "kuberploy-upgrade-" + compact + "-g" + strconv.FormatInt(generation, 10)
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("upgrade runner output exceeded limit")
	}
	return b.Buffer.Write(p)
}
