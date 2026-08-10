// Package runtimeview provides the scoped Kubernetes log and event core used by
// Kuberploy's production HTTP runtime-view handlers.
//
// The package deliberately accepts only opaque Kuberploy object references at
// its public request boundary. An injected Resolver turns that reference into
// an already-authorized namespace and immutable Deployment identities. Raw
// namespaces, Kubernetes selectors, kubeconfig data, bearer tokens, and TLS
// options are never caller-controlled.
//
// Production wiring maps Kubernetes objects into the narrow KubernetesClient
// interface through a TLS-verified in-cluster adapter, translates typed errors,
// writes access metadata (never log content) to the audit store, and serializes
// Stream events as bounded NDJSON with no-store/no-transform headers.
package runtimeview
