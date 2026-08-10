package gitprojection

import "errors"

// reconcilePendingError marks a projection result as durable but not yet
// terminal. The worker must leave the queue delivery unacknowledged and
// requeue the central operation so CommitOperation can recover an uncertain
// push or finish the database receipt on a later lease.
//
// Detail is deliberately a fixed, operator-safe string. The wrapped provider,
// Git, or database error remains available through Unwrap for logs without
// being copied into the user-visible operation record.
type reconcilePendingError struct {
	code   string
	detail string
	cause  error
}

func (e *reconcilePendingError) Error() string {
	return e.code + ": " + e.cause.Error()
}

func (e *reconcilePendingError) Unwrap() error { return e.cause }

func (e *reconcilePendingError) ReconcilePending() (string, string) {
	return e.code, e.detail
}

func pendingReconciliation(code, detail string, cause error) error {
	if cause == nil {
		return nil
	}
	var pending interface {
		ReconcilePending() (string, string)
	}
	if errors.As(cause, &pending) {
		return cause
	}
	return &reconcilePendingError{code: code, detail: detail, cause: cause}
}
