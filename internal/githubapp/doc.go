// Package githubapp implements the security-sensitive, provider-facing
// primitives used by Kuberploy's GitHub App integration.
//
// The package deliberately has no database, HTTP-handler, or Kubernetes
// dependencies. Callers inject secret access, time, randomness, HTTP transport,
// and atomic replay claims. Installation and repository authorization remains a
// caller responsibility and must be repeated immediately before a durable build
// operation mints a token.
package githubapp
