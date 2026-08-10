# Protected Git publication

`gitpublication` owns the provider-neutral state machine for publishing one
accepted protected-environment Git command through a GitHub pull request.

The caller persists immutable intent before Git or provider I/O. Candidate refs
are derived only from the operation UUID. Pull-request creation is recovered by
an exact repository/base/head/SHA lookup, so a lost provider response cannot
create a second review. Provider observations must repeat the exact repository,
target ref, candidate ref, candidate SHA, pull-request number and canonical URL.

An open or closed-unmerged pull request is workflow state only. Even a provider
`merged` observation remains `merge-pending` until the provider proves the exact
merge revision is contained by the current authoritative target head. The
package then exposes that verified target revision to the ordinary target-ref
indexer; it never writes desired state, arbitrary Git content, refs, checks,
comments, or Kubernetes resources.
