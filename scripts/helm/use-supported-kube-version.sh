#!/usr/bin/env bash

# Helm 4 follows the newest Kubernetes release when no cluster is attached.
# Offline chart tests must instead use Kuberploy's supported baseline unless a
# test supplies an explicit version.
helm() {
  local kp_helm_arg
  for kp_helm_arg in "$@"; do
    case "${kp_helm_arg}" in
      --kube-version|--kube-version=*) command helm "$@"; return ;;
    esac
  done
  case "${1:-}" in
    lint|template) command helm "$@" --kube-version 1.34.0 ;;
    *) command helm "$@" ;;
  esac
}
