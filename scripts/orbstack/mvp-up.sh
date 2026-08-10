#!/usr/bin/env bash

set -Eeuo pipefail

kp_script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
kp_run_id="${1:?usage: mvp-up.sh <run-id>}"

"${kp_script_dir}/first-slice-up.sh" "${kp_run_id}"
"${kp_script_dir}/install-local-dependencies.sh" "${kp_run_id}"
"${kp_script_dir}/platform-up.sh" "${kp_run_id}"

printf 'Walking development slice is ready. This is not the feature-complete MVP. Tenant route: http://hello.e2e.k8s.orb.local; control plane: http://kuberploy.k8s.orb.local\n'
