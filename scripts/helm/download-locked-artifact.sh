#!/usr/bin/env bash

# Download one reviewed chart artifact without accepting caller-selected
# commands, hosts, paths, or filenames. GitHub release assets prefer the
# authenticated gh CLI; other locked HTTPS hosts and gh failures use curl.
kp_download_locked_artifact() {
  local kp_url="$1"
  local kp_filename="$2"
  local kp_destination="$3"
  local kp_release_path kp_repository kp_release_tag kp_asset kp_partial

  [[ "${kp_url}" == https://* && "${kp_filename}" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,199}\.tgz$ &&
     "$(basename "${kp_destination}")" == "${kp_filename}" ]] || {
    printf 'invalid locked artifact destination\n' >&2
    return 1
  }
  mkdir -p "$(dirname "${kp_destination}")"

  kp_release_path="${kp_url#https://github.com/}"
  if [[ "${kp_release_path}" != "${kp_url}" && "${kp_release_path}" == */releases/download/* ]]; then
    kp_repository="${kp_release_path%%/releases/download/*}"
    kp_release_path="${kp_release_path#*/releases/download/}"
    kp_release_tag="${kp_release_path%%/*}"
    kp_asset="${kp_release_path#*/}"
    if [[ "${kp_repository}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ &&
          "${kp_release_tag}" =~ ^[A-Za-z0-9._+-]+$ && "${kp_asset}" == "${kp_filename}" &&
          "${kp_asset}" != */* ]] && command -v gh >/dev/null 2>&1; then
      if gh release download "${kp_release_tag}" --repo "${kp_repository}" --pattern "${kp_filename}" \
          --dir "$(dirname "${kp_destination}")" --clobber >/dev/null; then
        [[ -f "${kp_destination}" ]] && return 0
      fi
      printf 'gh release download failed; retrying locked HTTPS asset\n' >&2
    fi
  fi

  command -v curl >/dev/null 2>&1 || {
    printf 'curl is required when the locked artifact is not available through gh\n' >&2
    return 1
  }
  kp_partial="${kp_destination}.partial"
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
    "${kp_url}" -o "${kp_partial}"
  mv "${kp_partial}" "${kp_destination}"
}
