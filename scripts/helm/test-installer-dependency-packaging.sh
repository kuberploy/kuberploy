#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-installer-dependency-test.XXXXXX")"
trap 'rm -rf -- "${kp_tmp}"' EXIT
kp_packager="${kp_root}/scripts/helm/package-installer-dependencies.py"
kp_replacer="${kp_root}/scripts/helm/replace-installer-dependencies.py"
kp_lock="${kp_root}/charts/kuberploy-installer/dependencies.lock"
kp_epoch="${kp_root}/charts/kuberploy-installer/dependencies.source-date-epoch"

python3 "${kp_packager}" --root "${kp_root}" --destination "${kp_tmp}/first" \
  --lock "${kp_lock}" --source-date-epoch-file "${kp_epoch}"
sleep 2
python3 "${kp_packager}" --root "${kp_root}" --destination "${kp_tmp}/second" \
  --lock "${kp_lock}" --source-date-epoch-file "${kp_epoch}"

diff -qr "${kp_tmp}/first" "${kp_tmp}/second" >/dev/null
(
  cd "${kp_tmp}"
  mkdir locked
  cp -R first locked/charts
  cd locked
  shasum -a 256 -c "${kp_lock}" >/dev/null
)

python3 - "${kp_lock}" "${kp_tmp}/mutated.lock" <<'PY'
import sys
from pathlib import Path

source = Path(sys.argv[1]).read_text(encoding="utf-8")
replacement = ("0" if source[0] != "0" else "1") + source[1:]
Path(sys.argv[2]).write_text(replacement, encoding="utf-8")
PY
if python3 "${kp_packager}" --root "${kp_root}" --destination "${kp_tmp}/mutated" \
  --lock "${kp_tmp}/mutated.lock" --source-date-epoch-file "${kp_epoch}" >/dev/null 2>&1; then
  printf 'installer dependency packager accepted a mutated checksum lock\n' >&2
  exit 1
fi

for kp_failure_mode in after-backup signal-after-backup; do
  kp_replacement="${kp_tmp}/replacement-${kp_failure_mode}"
  mkdir -p "${kp_replacement}/charts/nested"
  printf 'previous archive bytes\n' >"${kp_replacement}/charts/previous.tgz"
  printf 'previous nested bytes\n' >"${kp_replacement}/charts/nested/state"
  cp -R "${kp_replacement}/charts" "${kp_replacement}/expected"
  kp_transaction="${kp_replacement}/.installer-dependencies.injected"
  mkdir -p "${kp_transaction}/new"
  printf 'new archive bytes\n' >"${kp_transaction}/new/new.tgz"
  if KUBERPLOY_TEST_INSTALLER_REPLACE_FAILURE="${kp_failure_mode}" \
    python3 "${kp_replacer}" --installer-root "${kp_replacement}" \
      --staged-charts "${kp_transaction}/new" >/dev/null 2>&1; then
    printf 'installer dependency replacement %s injection unexpectedly succeeded\n' \
      "${kp_failure_mode}" >&2
    exit 1
  fi
  diff -qr "${kp_replacement}/expected" "${kp_replacement}/charts" >/dev/null
  [[ ! -e "${kp_replacement}/.charts.previous" ]]
  [[ -f "${kp_transaction}/new/new.tgz" ]]
  [[ ! -e "${kp_replacement}/charts/new.tgz" ]]
done

printf 'installer dependency packaging reproducibility checks passed\n'
