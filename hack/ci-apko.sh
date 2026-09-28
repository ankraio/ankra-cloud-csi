#!/bin/sh
# Installs a pinned, checksum-verified apko into .ci-tools/apko for pipeline steps (apko needs a newer Go than the
# pipeline's toolchain to build from source). Prints the directory to put on PATH.
set -eu

version="1.4.5"
case "$(uname -m)" in
x86_64)
	architecture="amd64"
	checksum="d3393e89c7d5230a3f93eac13f3e9462ef41f5ae4673023f297583bc9c913762"
	;;
aarch64 | arm64)
	architecture="arm64"
	checksum="82be1104509fc80aef44a498a828195799aec2a03bfd9d5b4375c94b7be99140"
	;;
*) echo "ci-apko.sh: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
directory="$(pwd)/.ci-tools/apko"
if [ ! -x "${directory}/apko" ]; then
	archive="apko_${version}_linux_${architecture}.tar.gz"
	download_directory="$(mktemp -d)"
	curl -fsSL -o "${download_directory}/${archive}" \
		"https://github.com/chainguard-dev/apko/releases/download/v${version}/${archive}"
	echo "${checksum}  ${download_directory}/${archive}" | sha256sum -c - >&2
	mkdir -p "${directory}"
	tar -xzf "${download_directory}/${archive}" -C "${directory}" --strip-components=1 "apko_${version}_linux_${architecture}/apko"
	rm -rf "${download_directory}"
fi
echo "${directory}"
