#!/usr/bin/env bash

set -Eeuo pipefail

readonly KP_REGISTRY_IMAGE="docker.io/library/registry:3.1.1"
readonly KP_BUILDKIT_IMAGE="docker.io/moby/buildkit:v0.32.2"
readonly KP_BASE_IMAGE="docker.io/library/alpine:3.24.1"

kp_die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

kp_require_tool() {
  command -v "$1" >/dev/null 2>&1 || kp_die "required tool not found: $1"
}

kp_docker() {
  docker --context "${KUBERPLOY_TEST_DOCKER_CONTEXT}" "$@"
}

kp_require_tool awk
kp_require_tool curl
kp_require_tool docker
kp_require_tool grep
kp_require_tool sed
kp_require_tool tr

: "${KUBERPLOY_TEST_DOCKER_CONTEXT:?KUBERPLOY_TEST_DOCKER_CONTEXT is required}"
: "${KUBERPLOY_DOCKER_RUN_ID:?KUBERPLOY_DOCKER_RUN_ID is required}"

[[ "${KUBERPLOY_TEST_DOCKER_CONTEXT}" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$ ]] || \
  kp_die "KUBERPLOY_TEST_DOCKER_CONTEXT contains unsupported characters"
[[ "${KUBERPLOY_DOCKER_RUN_ID}" =~ ^[a-z0-9][a-z0-9-]{0,19}$ ]] || \
  kp_die "KUBERPLOY_DOCKER_RUN_ID must match ^[a-z0-9][a-z0-9-]{0,19}$"

kp_context_name="$(docker context inspect "${KUBERPLOY_TEST_DOCKER_CONTEXT}" \
  --format '{{.Name}}' 2>/dev/null || true)"
[[ "${kp_context_name}" == "${KUBERPLOY_TEST_DOCKER_CONTEXT}" ]] || \
  kp_die "the requested Docker context does not exist exactly"

kp_platform="$(kp_docker info --format '{{.OSType}}/{{.Architecture}}')"
[[ "${kp_platform}" == "linux/arm64" || "${kp_platform}" == "linux/aarch64" || \
   "${kp_platform}" == "linux/amd64" || "${kp_platform}" == "linux/x86_64" ]] || \
  kp_die "the Docker engine must be a supported Linux amd64 or arm64 engine"

readonly kp_registry_name="kuberploy-registry-${KUBERPLOY_DOCKER_RUN_ID}"
readonly kp_builder_name="kuberploy-buildkit-${KUBERPLOY_DOCKER_RUN_ID}"
kp_registry_created=false
kp_builder_created=false

kp_cleanup() {
  local kp_status=$?
  trap - EXIT INT TERM
  set +e
  if [[ "${kp_builder_created}" == "true" ]]; then
    kp_docker buildx rm "${kp_builder_name}" >/dev/null 2>&1
  fi
  if [[ "${kp_registry_created}" == "true" ]]; then
    kp_docker rm --force "${kp_registry_name}" >/dev/null 2>&1
  fi
  exit "${kp_status}"
}
trap kp_cleanup EXIT INT TERM

kp_existing_container="$(kp_docker ps --all \
  --filter "name=^/${kp_registry_name}$" --format '{{.Names}}')"
[[ -z "${kp_existing_container}" ]] || \
  kp_die "refusing to reuse existing container ${kp_registry_name}"

kp_existing_builder="$(kp_docker buildx ls --format '{{.Name}}' | \
  sed 's/[[:space:]*].*$//' | grep -Fx "${kp_builder_name}" || true)"
[[ -z "${kp_existing_builder}" ]] || \
  kp_die "refusing to reuse existing builder ${kp_builder_name}"

kp_docker run --detach --rm \
  --name "${kp_registry_name}" \
  --publish 127.0.0.1::5000 \
  --env REGISTRY_STORAGE_DELETE_ENABLED=true \
  "${KP_REGISTRY_IMAGE}" >/dev/null
kp_registry_created=true

kp_registry_port="$(kp_docker port "${kp_registry_name}" 5000/tcp | \
  sed -n 's/.*://p')"
[[ "${kp_registry_port}" =~ ^[0-9]{2,5}$ ]] || \
  kp_die "could not resolve the loopback registry port"
readonly kp_registry="127.0.0.1:${kp_registry_port}"

kp_ready=false
for _ in {1..20}; do
  if curl --fail --silent "http://${kp_registry}/v2/" >/dev/null; then
    kp_ready=true
    break
  fi
  sleep 1
done
[[ "${kp_ready}" == "true" ]] || kp_die "the temporary registry did not become ready"

kp_docker buildx create \
  --name "${kp_builder_name}" \
  --driver docker-container \
  --driver-opt network=host \
  --driver-opt "image=${KP_BUILDKIT_IMAGE}" >/dev/null
kp_builder_created=true
kp_builder_inspect="$(kp_docker buildx inspect \
  --builder "${kp_builder_name}" --bootstrap)"
grep -F 'BuildKit version:      v0.32.2' <<<"${kp_builder_inspect}" >/dev/null || \
  kp_die "the temporary builder did not start the pinned BuildKit version"

readonly kp_image_ref="${kp_registry}/kuberploy/cache-smoke:image-${KUBERPLOY_DOCKER_RUN_ID}"
readonly kp_second_ref="${kp_registry}/kuberploy/cache-smoke:image-2-${KUBERPLOY_DOCKER_RUN_ID}"
readonly kp_cache_ref="${kp_registry}/kuberploy/cache-smoke:buildcache-${KUBERPLOY_DOCKER_RUN_ID}"

kp_docker buildx build \
  --builder "${kp_builder_name}" \
  --progress plain \
  --file - \
  --push \
  --provenance=false \
  --sbom=false \
  --tag "${kp_image_ref}" \
  --cache-to "type=registry,ref=${kp_cache_ref},mode=max,image-manifest=true,oci-mediatypes=true" \
  . <<EOF
FROM ${KP_BASE_IMAGE}
RUN printf 'kuberploy-registry-cache-smoke\\n' >/marker
EOF

kp_second_build="$(kp_docker buildx build \
  --builder "${kp_builder_name}" \
  --progress plain \
  --file - \
  --push \
  --provenance=false \
  --sbom=false \
  --tag "${kp_second_ref}" \
  --cache-from "type=registry,ref=${kp_cache_ref}" \
  . 2>&1 <<EOF
FROM ${KP_BASE_IMAGE}
RUN printf 'kuberploy-registry-cache-smoke\\n' >/marker
EOF
)"
printf '%s\n' "${kp_second_build}"
grep -E '^#[0-9]+ CACHED$' <<<"${kp_second_build}" >/dev/null || \
  kp_die "the second build did not reuse the exported registry cache"

kp_digest="$(curl --fail --silent --show-error --head \
  -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
  "http://${kp_registry}/v2/kuberploy/cache-smoke/manifests/image-2-${KUBERPLOY_DOCKER_RUN_ID}" | \
  tr -d '\r' | awk 'tolower($1)=="docker-content-digest:" {print $2}')"
[[ "${kp_digest}" =~ ^sha256:[a-f0-9]{64}$ ]] || \
  kp_die "the pushed image did not return an immutable digest"

curl --fail --silent --show-error --request DELETE \
  "http://${kp_registry}/v2/kuberploy/cache-smoke/manifests/${kp_digest}" >/dev/null
kp_deleted_status="$(curl --silent --output /dev/null --write-out '%{http_code}' \
  --head \
  -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
  "http://${kp_registry}/v2/kuberploy/cache-smoke/manifests/image-2-${KUBERPLOY_DOCKER_RUN_ID}")"
[[ "${kp_deleted_status}" == "404" ]] || \
  kp_die "digest deletion did not make the selected manifest unavailable"

printf 'Registry image push, mode=max cache export/import, cache hit, and digest deletion passed on %s.\n' \
  "${kp_platform}"
