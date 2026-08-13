// Package upgrade retains the pre-stable isolated runner model for compatibility
// tests and decoding historical operation rows. Production HTTP routes do not
// create these operations.
//
// The Kubernetes adapter is intentionally isolated from the worker process's
// dependency graph. It must implement the Executable JSON protocol, reconcile
// exactly one namespaced Job named by jobName, use the persisted manifest and
// immutable chart digest, preserve existing Helm values, render/validate, and
// run either a normal rollback-on-failure Helm upgrade or an explicit bounded
// Helm revision rollback copied from a succeeded durable upgrade. Reinvocation with the same
// operation ID, generation, and jobName must reconcile the existing Job rather
// than create another. It must never accept a URL or chart supplied outside the
// verified manifest and must return only after the Job has a durable result.
//
// Argo owns the control-plane child and does not create Helm release storage for
// it, so invoking this runner against an installer-managed deployment would be
// incorrect and may be reverted by self-heal. Operators upgrade or roll back the
// installer Helm release. Installer lifecycle hooks reconcile an immutable
// enabled-Application inventory and require exact revision, Synced, and Healthy
// observations before Helm succeeds.
package upgrade
