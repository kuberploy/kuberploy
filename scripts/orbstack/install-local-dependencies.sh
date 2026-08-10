#!/usr/bin/env bash

set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

kp_assert_orbstack_context
kp_require_tools helm kubectl
kp_run_id="${1:?usage: install-local-dependencies.sh <run-id>}"
kp_namespace="$(kp_require_run_namespace "${kp_run_id}")"
kp_root="$(kp_repo_root)"
kp_release="$(kp_release_name local-deps "${kp_run_id}")"

# These credentials are synthetic and scoped to a disposable local namespace.
# They are created directly as Secrets and are never written to Git or output.
kp_postgres_password="kp-pg-${kp_run_id}-test-only"
kp_valkey_password="kp-valkey-${kp_run_id}-test-only"
kp_postgres_url="postgres://kuberploy:${kp_postgres_password}@kuberploy-postgresql.${kp_namespace}.svc.cluster.local:5432/kuberploy?sslmode=disable"

kubectl create secret generic kuberploy-postgresql -n "${kp_namespace}" \
  --from-literal=password="${kp_postgres_password}" \
  --from-literal=url="${kp_postgres_url}" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl label secret kuberploy-postgresql -n "${kp_namespace}" --overwrite \
  "kuberploy.io/test-run=${kp_run_id}"

kubectl create secret generic kuberploy-valkey -n "${kp_namespace}" \
  --from-literal="addresses=kuberploy-valkey.${kp_namespace}.svc.cluster.local:6379" \
  --from-literal=username=default \
  --from-literal=password="${kp_valkey_password}" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl label secret kuberploy-valkey -n "${kp_namespace}" --overwrite \
  "kuberploy.io/test-run=${kp_run_id}"

helm upgrade --install "${kp_release}" "${kp_root}/deploy/orbstack/local-dependencies" \
  --namespace "${kp_namespace}" \
  --set-string "testRun=${kp_run_id}" \
  --rollback-on-failure --cleanup-on-fail --wait --timeout 10m

kubectl rollout status statefulset/kuberploy-postgresql -n "${kp_namespace}" --timeout=5m
kubectl rollout status statefulset/kuberploy-valkey -n "${kp_namespace}" --timeout=5m
printf 'Installed run-scoped PostgreSQL and Valkey in %s.\n' "${kp_namespace}"
