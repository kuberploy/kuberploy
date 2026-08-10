// Package buildlogs provides bounded, authorization-fenced access to source
// build logs. Kubernetes identities are derived only from immutable build
// attempts and are never accepted from an API caller.
package buildlogs
