#!/usr/bin/env bash

set -Eeuo pipefail
kp_script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

kp_run_id="${1:?usage: first-slice-up.sh <run-id>}"
"${kp_script_dir}/preflight.sh"
"${kp_script_dir}/create-run.sh" "${kp_run_id}"
"${kp_script_dir}/install-argo.sh" "${kp_run_id}"
"${kp_script_dir}/install-traefik.sh" "${kp_run_id}"
"${kp_script_dir}/install-local-git.sh" "${kp_run_id}"
"${kp_script_dir}/bootstrap-local-repo.sh" "${kp_run_id}"
"${kp_script_dir}/apply-argo-root.sh" "${kp_run_id}"
"${kp_script_dir}/verify-first-slice.sh" "${kp_run_id}"

printf 'First slice is healthy. Inspect with status.sh %s; route: http://hello.e2e.k8s.orb.local\n' "${kp_run_id}"
