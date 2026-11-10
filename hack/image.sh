#!/bin/sh
# Builds a multi-arch (linux/amd64, linux/arm64) driver image without a container daemon: apko assembles the base
# from BASE_CONFIG and ko cross-compiles COMMAND onto it, attaching an SPDX SBOM.
#
#   IMAGE       the image repository, for example share.ankra.cloud/library/ankra-cloud-csi
#   BASE_IMAGE  the base image repository, for example share.ankra.cloud/base/ankra-cloud-csi-base
#   COMMAND     the Go main package (default ./cmd/ankra-cloud-csi; ./cmd/ankra-cloud-cosi for the COSI driver)
#   BASE_CONFIG the apko configuration of the base (default build/base.apko.yaml; build/cosi-base.apko.yaml for COSI)
#   TAGS        space-separated immutable tags, for example "v0.1.0" or "sha-1a2b3c4"
#   VERSION     the version the binary reports (default: the first tag)
#   PUSH        true to publish; anything else builds both images and discards them
#
# Registry credentials come from the Docker config ($DOCKER_CONFIG/config.json or ~/.docker/config.json).
set -eu

: "${IMAGE:?set IMAGE}"
: "${BASE_IMAGE:?set BASE_IMAGE}"
: "${TAGS:?set TAGS}"
PUSH="${PUSH:-false}"
COMMAND="${COMMAND:-./cmd/ankra-cloud-csi}"
BASE_CONFIG="${BASE_CONFIG:-build/base.apko.yaml}"
base_name="$(basename "${BASE_IMAGE}")"
first_tag="${TAGS%% *}"
VERSION="${VERSION:-${first_tag}}"
export VERSION

for tag in ${TAGS}; do
	case "${tag}" in
	latest) echo "image.sh: refusing the moving tag latest" >&2; exit 1 ;;
	esac
done

work_directory="$(mktemp -d)"
trap 'rm -rf "${work_directory}"' EXIT
platforms="linux/amd64,linux/arm64"
ko_tags="$(printf '%s' "${TAGS}" | tr ' ' ',')"

if [ "${PUSH}" = "true" ]; then
	if command -v crane >/dev/null 2>&1; then
		for tag in ${TAGS}; do
			if crane manifest "${IMAGE}:${tag}" >/dev/null 2>&1; then
				echo "image.sh: ${IMAGE}:${tag} is already published and tags are immutable" >&2
				exit 1
			fi
		done
	fi
	base_tags=""
	for tag in ${TAGS}; do
		base_tags="${base_tags} ${BASE_IMAGE}:${tag}"
	done
	# shellcheck disable=SC2086
	apko publish "${BASE_CONFIG}" ${base_tags} --image-refs "${work_directory}/base-refs" \
		--sbom-path "${work_directory}"
	# apko lists each architecture's image and then the index that ties them together.
	base_reference="$(tail -n 1 "${work_directory}/base-refs")"
	echo "base image: ${base_reference}"
	KO_DEFAULTBASEIMAGE="${base_reference}" KO_DOCKER_REPO="${IMAGE}" \
		ko build "${COMMAND}" --bare --platform="${platforms}" --tags="${ko_tags}" --sbom=spdx \
		--image-refs "${work_directory}/image-refs"
	echo "image: $(tail -n 1 "${work_directory}/image-refs")"
else
	apko build "${BASE_CONFIG}" "${base_name}:${first_tag}" "${work_directory}/base.tar" \
		--sbom-path "${work_directory}"
	KO_DEFAULTBASEIMAGE="cgr.dev/chainguard/static" KO_DOCKER_REPO="${IMAGE}" \
		ko build "${COMMAND}" --bare --platform="${platforms}" --tags="${ko_tags}" --push=false
	echo "built ${IMAGE}:${first_tag} and its base without pushing"
fi
