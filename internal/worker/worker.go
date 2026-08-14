package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/queue"
	"github.com/kuberploy/kuberploy/internal/store"
)

type GitWriter interface {
	Write(context.Context, domain.Operation, domain.Project, domain.Environment, domain.Application, domain.Deployment) (domain.GitPublicationResult, error)
}

// VariableGitWriter is intentionally a closed capability of the projection
// writer. The legacy application writer cannot publish arbitrary files.
type VariableGitWriter interface {
	WriteVariable(context.Context, domain.Operation) (domain.GitPublicationResult, error)
}
type Processor struct {
	Store                      store.Store
	Queue                      queue.Consumer
	Writer                     GitWriter
	Name                       string
	Batch                      int
	OperationLeaseDuration     time.Duration
	OperationHeartbeatInterval time.Duration
}

const (
	defaultOperationLeaseDuration     = 2 * time.Minute
	defaultOperationHeartbeatInterval = 30 * time.Second
)

func (p *Processor) RunOnce(ctx context.Context) (int, error) {
	name := p.Name
	if name == "" {
		name = "worker"
	}
	limit := p.Batch
	if limit <= 0 {
		limit = 10
	}
	lease, heartbeat, err := p.operationLeaseSettings()
	if err != nil {
		return 0, err
	}
	var messages []domain.WorkMessage
	var queueErr error
	if p.Queue != nil {
		messages, queueErr = p.Queue.Receive(ctx, name, limit)
	}
	if queueErr != nil || len(messages) == 0 {
		messages, queueErr = p.Store.LeasePendingOperations(ctx, name, limit, lease)
		if queueErr != nil {
			return 0, queueErr
		}
	}
	processed := 0
	for _, message := range messages {
		op, execute, err := p.Store.StartOperation(ctx, message.OperationID, message.Generation, name, lease)
		if err != nil {
			return processed, err
		}
		if !execute {
			if p.Queue != nil && message.DeliveryID != "" {
				if err = p.Queue.Ack(ctx, message); err != nil {
					return processed, err
				}
			}
			continue
		}
		workCtx, stopHeartbeat := p.startOperationHeartbeat(ctx, op, name, lease, heartbeat)
		var workErr error
		reconcilePending := false
		pendingCode, pendingDetail := "", ""
		switch op.Kind {
		case "deployment.git-write":
			d, err := p.Store.GetDeploymentForOperation(workCtx, op.ID)
			if err == nil {
				var a domain.Application
				a, err = p.Store.GetApplication(workCtx, d.ApplicationID)
				if err == nil {
					var e domain.Environment
					e, err = p.Store.GetEnvironment(workCtx, d.EnvironmentID)
					if err == nil {
						var project domain.Project
						project, err = p.Store.GetProject(workCtx, e.ProjectID)
						if err == nil {
							if p.Writer == nil {
								err = fmt.Errorf("Git writer is not configured")
							} else {
								var publication domain.GitPublicationResult
								publication, err = p.Writer.Write(workCtx, op, project, e, a, d)
								if err == nil && workCtx.Err() != nil {
									err = workCtx.Err()
								}
								if err == nil {
									err = p.Store.CompleteGitOperation(workCtx, op.ID, op.Generation, name, publication)
									if err != nil {
										pendingCode = "GitOperationCompletionPending"
										pendingDetail = "The durable Git commit will be attached to the deployment operation again."
									}
								}
							}
						}
					}
				}
			}
			if code, detail, ok := reconcilePendingDetails(err); ok {
				pendingCode, pendingDetail = code, detail
			}
			workErr = err
		case "variable-set.git-write":
			writer, ok := p.Writer.(VariableGitWriter)
			if !ok {
				workErr = fmt.Errorf("Git variable writer is not configured")
				break
			}
			publication, writeErr := writer.WriteVariable(workCtx, op)
			if writeErr == nil && workCtx.Err() != nil {
				writeErr = workCtx.Err()
			}
			if writeErr == nil {
				writeErr = p.Store.CompleteGitOperation(workCtx, op.ID, op.Generation, name, publication)
				if writeErr != nil {
					pendingCode = "GitOperationCompletionPending"
					pendingDetail = "The durable Git commit will be attached to the variable operation again."
				}
			}
			if code, detail, pending := reconcilePendingDetails(writeErr); pending {
				pendingCode, pendingDetail = code, detail
			}
			workErr = writeErr
		default:
			workErr = fmt.Errorf("unsupported operation kind %q", op.Kind)
		}
		heartbeatErr := stopHeartbeat()
		if heartbeatErr != nil {
			return processed, fmt.Errorf("operation %s lease heartbeat failed: %w", op.ID, heartbeatErr)
		}
		if ctx.Err() != nil {
			return processed, ctx.Err()
		}
		if pendingCode != "" {
			if requeueErr := p.Store.RequeueOperation(ctx, op.ID, op.Generation, name, pendingCode, pendingDetail); requeueErr != nil {
				return processed, fmt.Errorf("requeue %s reconciliation: %w", op.Kind, requeueErr)
			}
			reconcilePending = true
			workErr = nil
		}
		if workErr != nil {
			code := "WorkFailed"
			if op.Kind == "deployment.git-write" || op.Kind == "variable-set.git-write" {
				code = "GitWriteFailed"
			}
			if failErr := p.Store.FailOperation(ctx, op.ID, op.Generation, name, code, workErr.Error()); failErr != nil {
				return processed, fmt.Errorf("work failed (%v) and recording failure failed: %w", workErr, failErr)
			}
		}
		if !reconcilePending && p.Queue != nil && message.DeliveryID != "" {
			if ackErr := p.Queue.Ack(ctx, message); ackErr != nil {
				return processed, ackErr
			}
		}
		processed++
	}
	return processed, nil
}

func (p *Processor) operationLeaseSettings() (time.Duration, time.Duration, error) {
	lease := p.OperationLeaseDuration
	if lease == 0 {
		lease = defaultOperationLeaseDuration
	}
	heartbeat := p.OperationHeartbeatInterval
	if heartbeat == 0 {
		heartbeat = defaultOperationHeartbeatInterval
	}
	if lease < 100*time.Millisecond || lease > 15*time.Minute || heartbeat < 10*time.Millisecond || heartbeat >= lease/2 {
		return 0, 0, fmt.Errorf("invalid operation lease or heartbeat interval")
	}
	return lease, heartbeat, nil
}

func (p *Processor) startOperationHeartbeat(parent context.Context, op domain.Operation, worker string, lease, interval time.Duration) (context.Context, func() error) {
	workCtx, cancelWork := context.WithCancel(parent)
	heartbeatCtx, cancelHeartbeat := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				done <- nil
				return
			case <-ticker.C:
				timeout := interval
				if timeout > 10*time.Second {
					timeout = 10 * time.Second
				}
				attemptCtx, cancel := context.WithTimeout(heartbeatCtx, timeout)
				err := p.Store.HeartbeatOperation(attemptCtx, op.ID, op.Generation, worker, lease)
				cancel()
				if err != nil {
					cancelWork()
					done <- err
					return
				}
			}
		}
	}()
	return workCtx, func() error {
		cancelHeartbeat()
		cancelWork()
		return <-done
	}
}

func reconcilePendingDetails(err error) (string, string, bool) {
	if err == nil {
		return "", "", false
	}
	var pending interface {
		ReconcilePending() (string, string)
	}
	if !errors.As(err, &pending) {
		return "", "", false
	}
	code, detail := pending.ReconcilePending()
	if code == "" || detail == "" {
		return "", "", false
	}
	return code, detail, true
}
