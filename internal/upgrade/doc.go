// Package upgrade defines the isolated execution boundary for platform upgrades.
//
// The Kubernetes adapter is intentionally isolated from the worker process's
// dependency graph. It must implement the Executable JSON protocol, reconcile
// exactly one namespaced Job named by jobName, use the persisted manifest and
// immutable chart digest, preserve existing Helm values, render/validate, and
// run a normal rollback-on-failure Helm upgrade. Reinvocation with the same
// operation ID, generation, and jobName must reconcile the existing Job rather
// than create another. It must never accept a URL or chart supplied outside the
// verified manifest and must return only after the Job has a durable result.
package upgrade
