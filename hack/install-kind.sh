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
kind_url="https://kind.sigs.k8s.io/dl/${KIND_VERSION}/${kind_filename}"

case "${KIND_VERSION}:${kind_arch}" in
  v0.32.0:amd64) expected_sha256=50030de23cf40a18505f20426f6a8506bedf13c6e509244bd1fa9463721b0f54 ;;
  v0.32.0:arm64) expected_sha256=b92cd615e97585de8ddade28ed5cd7feb4248d717c233eea5b03c37298900f5d ;;
  *)
    echo "No pinned kind checksum for ${KIND_VERSION} (${kind_arch})" >&2
    exit 1
    ;;
esac

mkdir -p "$cache_dir" "$bin_dir"

if [[ ! -x "$kind_path" ]]; then
  curl --fail --location --silent --show-error \
    --output "$kind_path" \
    "$kind_url"
  chmod +x "$kind_path"
fi

actual_sha256="$(sha256sum "$kind_path" | awk '{print $1}')"
if [[ "$actual_sha256" != "$expected_sha256" ]]; then
  echo "Unexpected SHA-256 for $kind_path: $actual_sha256" >&2
  exit 1
fi

ln -sfn "$kind_path" "$bin_dir/kind"
echo "$bin_dir" >> "$GITHUB_PATH"
