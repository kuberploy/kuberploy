// Package registry owns OCI artifact lifecycle policy and the narrowly scoped
// managed-Distribution execution boundary. It turns a transactionally loaded,
// complete observation snapshot into a deterministic preview, then asks the
// store to revalidate and lease each destructive step.
//
// The Distribution client is bound to one managed target, a separately supplied
// platform-owned expected origin, and a repository prefix. It receives
// short-lived credentials from an injected broker, verifies
// an exact manifest digest with HEAD, deletes that digest, and confirms absence.
// It does not follow redirects, accept arbitrary provider URLs, retain provider
// bodies, or expose an online blob-delete method.
//
// External targets intentionally stop before planning: their image and Buildx
// cache lifecycle belongs to the external registry operator. Managed targets
// use release retention, explicit protection references, a safety age, scoped
// cache policies, and registry-wide OCI graph reachability.
//
// A blob item is eligibility evidence for an offline managed-registry sweep,
// not permission to issue a non-standard online per-blob DELETE. The provider
// executor first finishes digest-based manifest deletions, then requires an
// exclusive tested read-only/stopped maintenance proof, a fresh complete
// registry-wide authority/reachability checkpoint, and one durable idempotent
// provider sweep receipt before recording blob items as deleted. Production
// supplies the exact-name Kubernetes maintenance controller: it UID/resource-
// version fences the managed Deployment, verifies the immutable ConfigMap/PVC,
// stops the registry, and adopts only deterministic bounded helper Jobs before
// restoring readiness. External registries never enter this path.
package registry
