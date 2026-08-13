#!/usr/bin/env bash

set -Eeuo pipefail

kp_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
kp_tmp="$(mktemp -d "${TMPDIR:-/tmp}/kuberploy-secret-scan-test.XXXXXX")"
trap 'rm -rf -- "${kp_tmp}"' EXIT

mkdir -p "${kp_tmp}/bin" "${kp_tmp}/repo"
cat >"${kp_tmp}/bin/gitleaks" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
kp_scan="${!#}"
if grep -R -q 'KUBERPLOY_TEST_INDEX_SECRET' "${kp_scan}"; then
  exit 1
fi
exit 0
EOF
chmod 0755 "${kp_tmp}/bin/gitleaks"

cd "${kp_tmp}/repo"
git init --quiet
git config user.name "Kuberploy test"
git config user.email "test@kuberploy.invalid"
printf '[extend]\nuseDefault = true\n' >.gitleaks.toml
printf 'safe\n' >fixture.txt
git add .gitleaks.toml fixture.txt
git commit --quiet -m baseline

# The worktree is safe, but the exact index contains a secret. The scanner
# must reject the commit-ready index rather than inspecting only the worktree.
printf 'KUBERPLOY_TEST_INDEX_SECRET\n' >fixture.txt
git add fixture.txt
printf 'safe worktree replacement\n' >fixture.txt
if PATH="${kp_tmp}/bin:${PATH}" "${kp_root}/scripts/security/scan-tracked-secrets.sh" >"${kp_tmp}/scan.log" 2>&1; then
  printf 'error: staged secret was hidden by different working-tree bytes\n' >&2
  exit 1
fi

git reset --hard --quiet HEAD
ln -s fixture.txt linked.txt
git add linked.txt
if PATH="${kp_tmp}/bin:${PATH}" "${kp_root}/scripts/security/scan-tracked-secrets.sh" >"${kp_tmp}/symlink.log" 2>&1; then
  printf 'error: tracked symlink was accepted\n' >&2
  exit 1
fi
grep -q 'tracked symlink' "${kp_tmp}/symlink.log"
