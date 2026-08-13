#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(git rev-parse --show-toplevel)"
kp_stage="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-secret-scan.XXXXXX")"
kp_index="${kp_stage}/index"
kp_worktree="${kp_stage}/worktree"
trap 'rm -rf -- "${kp_stage}"' EXIT

cd "${kp_root}"
mkdir -p "${kp_index}" "${kp_worktree}"

while IFS= read -r -d '' kp_entry; do
  kp_metadata="${kp_entry%%$'\t'*}"
  kp_path="${kp_entry#*$'\t'}"
  kp_mode="${kp_metadata%% *}"
  [[ -n "${kp_path}" ]] || continue
  case "${kp_path}" in
    .secrets|.secrets/*)
      printf 'error: tracked local secret path: %s\n' "${kp_path}" >&2
      exit 1
      ;;
  esac
  case "${kp_mode}" in
    100644|100755) ;;
    120000)
      printf 'error: tracked symlink is outside the secret-scan copy boundary: %s\n' "${kp_path}" >&2
      exit 1
      ;;
    *)
      printf 'error: unsupported tracked Git object mode %s: %s\n' "${kp_mode}" "${kp_path}" >&2
      exit 1
      ;;
  esac
done < <(git ls-files --stage -z)

# The index is the exact commit-ready authority. Materialize it first so a
# staged secret cannot be hidden by a different or deleted working-tree file.
git checkout-index --all --prefix="${kp_index}/"

# Scan existing tracked working-tree bytes too. An unstaged deletion is safe to
# skip here because its still-commit-ready index bytes were copied above.
while IFS= read -r -d '' kp_path; do
  [[ -n "${kp_path}" ]] || continue
  [[ -e "${kp_path}" ]] || continue
  if [[ -L "${kp_path}" ]]; then
    printf 'error: tracked symlink is outside the secret-scan copy boundary: %s\n' "${kp_path}" >&2
    exit 1
  fi
  mkdir -p "${kp_worktree}/$(dirname "${kp_path}")"
  cp -p -- "${kp_path}" "${kp_worktree}/${kp_path}"
done < <(git ls-files -z)

if command -v gitleaks >/dev/null 2>&1; then
  exec gitleaks dir --no-banner --redact --exit-code 1 --config "${kp_root}/.gitleaks.toml" "${kp_stage}"
fi

command -v docker >/dev/null 2>&1 || {
  printf 'error: secret scan requires gitleaks or Docker\n' >&2
  exit 1
}
exec docker run --rm --network none --read-only \
  --volume "${kp_stage}:/scan:ro" \
  --volume "${kp_root}/.gitleaks.toml:/gitleaks.toml:ro" \
  ghcr.io/gitleaks/gitleaks:v8.28.0 dir --no-banner --redact --exit-code 1 --config /gitleaks.toml /scan
