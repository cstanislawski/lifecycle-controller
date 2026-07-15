#!/usr/bin/env bash

set -euo pipefail

: "${KIND_VERSION:?KIND_VERSION must be set}"
: "${GITHUB_PATH:?GITHUB_PATH must be set}"

bin_dir="$HOME/.local/bin"

case "${RUNNER_ARCH:-X64}" in
  X64) kind_arch=amd64 ;;
  ARM64) kind_arch=arm64 ;;
  *)
    echo "Unsupported runner architecture: ${RUNNER_ARCH:-unknown}" >&2
    exit 1
    ;;
esac

cache_dir="${XDG_CACHE_HOME:-$HOME/.cache}/kind/${KIND_VERSION}-${kind_arch}"
kind_filename="kind-linux-${kind_arch}"
kind_path="$cache_dir/$kind_filename"
checksum_path="$cache_dir/$kind_filename.sha256sum"
kind_url="https://kind.sigs.k8s.io/dl/${KIND_VERSION}/${kind_filename}"

mkdir -p "$cache_dir" "$bin_dir"

if [[ ! -x "$kind_path" || ! -f "$checksum_path" ]]; then
  curl --fail --location --silent --show-error \
    --output "$kind_path" \
    "$kind_url"
  curl --fail --location --silent --show-error \
    --output "$checksum_path" \
    "${kind_url}.sha256sum"
  chmod +x "$kind_path"
fi

(cd "$cache_dir" && sha256sum --check "$(basename "$checksum_path")")

ln -sfn "$kind_path" "$bin_dir/kind"
echo "$bin_dir" >> "$GITHUB_PATH"
