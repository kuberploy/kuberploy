# Release engineering

Kuberploy publishes semantic release tags (`vMAJOR.MINOR.PATCH` and explicit
release-candidate tags such as `vMAJOR.MINOR.PATCH-rc.N`). A trusted tag
builds each of six images natively on amd64 and arm64 GitHub-hosted runners,
assembles and verifies one two-platform OCI image index per component, records
the index digests, packages an immutable Helm chart, validates the release
manifest twice, and creates a draft GitHub Release before making it public.
Versions with a prerelease suffix publish as GitHub prereleases; only stable
versions may become the repository's latest release.

A stable tag is additionally fail-closed behind a reviewed receipt at
`release/qualifications/<stable-version>.json`. The receipt must name an exact
final RC on the same version line, its protected commit, the checksum and UTC
completion time of the qualification report, and successful qualification and
teardown states. The release gate independently proves that the named RC tag
still resolves to that commit and that its GitHub prerelease is immutable.
Release candidates do not require this receipt. This keeps ordinary RC work
moving while making an accidental stable tag unable to publish.

Repository release immutability is an externally verified prerequisite. Before
a tag is pushed, a repository administrator must enable immutable releases and
verify the setting through the repository settings or an administrator-scoped
API credential. Approval of the protected `release` environment records that
this verification happened. The workflow's `GITHUB_TOKEN` intentionally has no
repository Administration permission, so the workflow does not make the
impossible settings preflight itself. After publication it still requires the
release API to report `immutable: true`; otherwise the job fails closed. GitHub
applies the repository setting only to future releases.

Before the first tag, an administrator must complete and independently verify
all of these repository prerequisites:

- make the repository public, or use an organization plan that supports the
  same protected-tag rulesets and immutable-release controls for private
  repositories;
- create a ruleset that protects `refs/tags/v*`, restricts creation and update
  to the release maintainers, and forbids deletion and non-fast-forward change;
- create the protected `release` Actions environment with required reviewers
  and restrict it to the protected release-tag policy;
- enable immutable releases before any release object is created; and
- allow the release workflow's narrowly scoped `GITHUB_TOKEN` to write package
  and release artifacts, without granting repository Administration access.

The workflow requires both the protected-tag signal and approval of the
`release` environment. A manual `workflow_dispatch` runs validation only and
cannot publish. Do not push a `v*` tag until the checklist is complete: the
workflow deliberately cannot repair repository settings, and a draft left
after publication starts requires explicit administrator review.

The source chart intentionally uses exact development tags. Release packaging
copies it to a temporary directory, enables `global.requireImageDigest`, and
injects the API, worker, web, migration, and builder-agent `image@sha256`
references. The published OCI chart and `.tgz` therefore render the same
immutable images.

Release-manifest schema v1 still carries the historical `upgrader` image field
so already-published signed manifests remain verifiable. The control-plane
chart does not deploy or invoke that artifact; platform changes are performed
only by a cluster administrator upgrading the installer Helm release.

The migration image contains Prisma CLI 7.9.1 and the reviewed native SQL
history. Its mandatory pre-install/pre-upgrade Job is the only production
schema writer. API and worker processes perform read-only history verification;
they never run migrations during startup.

The same release publishes the canonical component-chart set for Argo CD, the
single-invocation installer, builder, cert-manager, Traefik edge, external-dns, External Secrets,
monitoring, PostgreSQL, registry, runtime, Sealed Secrets, and Valkey. Every
chart is versioned with the Kuberploy release, packaged reproducibly, recorded
by package and OCI digest in `release-manifest.json`, and attached to the
GitHub release. Third-party chart dependencies are fetched only from the
checked-in HTTPS artifact locks and must match their SHA-256 entries in
`DEPENDENCIES.md`. The workflow publishes or adopts each OCI version
independently, comparing exact package bytes and predicted OCI digests, so a
retry can safely resume after only part of the set was pushed.

The installer package vendors the exact byte-reproducible Argo CD wrapper
package published by the same release. Its dependency version, Helm lock,
package SHA-256 annotation, and sidecar dependency lock are regenerated from
the release version and tagged commit time; semantic validation rejects any
difference between the nested and standalone Argo CD artifacts.

The published chart embeds `kuberploy-builder` under the `builder` alias. The
release packager rewrites the embedded chart to the release version and pins
its builder-agent image, but `builder.enabled` remains `false`. Enabling that
subchart is a separate cluster-administrator decision that requires a
dedicated labelled and tainted builder node pool. Inclusion of this boundary
does not claim that the build controller or build API is enabled.

Each native build pushes an untagged, content-addressed platform manifest.
After all twelve builds complete, the assembly job verifies each child digest's
reported architecture and creates one uniquely tagged
`candidate-RUN_ID-RUN_ATTEMPT` OCI index per component. It refuses an existing
candidate tag and verifies that every index contains exactly the expected
`linux/amd64` and `linux/arm64` child digests. Only these merged index digests
enter the chart and release manifest; child digests are never published as the
multi-platform release identity.

## GitHub-hosted runners

The native builds use `ubuntu-26.04` for amd64 and `ubuntu-26.04-arm` for
arm64. GitHub currently lists both labels as public preview, so they are
available but carry preview availability and support risk. The lightweight tag
gate and Helm-only lifecycle render use `ubuntu-slim`. GitHub documents that
runner as a one-CPU, unprivileged container with a hard 15-minute timeout; it
has no Docker daemon and cannot run the normal Buildx container driver.

The release contract remains on `ubuntu-26.04` because it compiles and vets Go
code as well as rendering the chart. Image-index assembly also requires the
full VM's Docker daemon. Final publication remains on the full VM because it
installs Go and Helm, performs all local manifest validation, executes registry
and GitHub release operations, and needs more recovery headroom than the slim
runner's hard 15-minute limit. See the
[GitHub-hosted runner reference](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
and the current
[`ubuntu-slim` software list](https://github.com/actions/runner-images/blob/main/images/ubuntu-slim/ubuntu-slim-Readme.md).

Release packaging writes deterministic `.tgz` files using the tagged commit
time and predicts each Helm OCI manifest digest locally. Manifest generation,
semantic validation, and checksums all finish before any canonical OCI chart
tag is published. Before the installer is packaged, the release packager
computes the exact `kuberploy-runtime` OCI digest and embeds its semantic
version, digest, and consistency lock in installer chart metadata. The release
validator requires that lock to equal the runtime entry in
`release-manifest.json`; it is never an operator Helm value. On a rerun, every
existing chart version is pulled and
reused only when both its manifest digest and package bytes match the locally
generated artifact; any mismatch is rejected without pushing over the tag. An existing
GitHub release, including a draft left by an interrupted run, is also rejected
and requires explicit administrator review rather than automated mutation.

Run the complete non-publishing release contract locally:

    ./release/test.sh

This validates source-version alignment, approved action major-version pins, JSON Schema,
cross-field semantics, checksums, and install/upgrade/rollback chart rendering.
It does not log in to GHCR, push an image or chart, create a release, or contact
a Kubernetes cluster.

`release-manifest.json` is the machine-readable release contract used by the
read-only release checker and operator tooling. Its `$schema` URL is pinned to the exact source commit;
the canonical schema is `release/release-manifest.schema.json`.
