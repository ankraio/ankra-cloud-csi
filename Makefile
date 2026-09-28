# ankra-cloud-csi. `make check` runs every gate the pipeline (.ankra/pipeline.yaml) runs, except the image build.

CHART := charts/ankra-cloud-csi
DEPLOY := deploy
RENDER := helm template ankra-cloud-csi $(CHART) --namespace kube-system --set api.existingSecret=ankra-cloud-csi-api
IMAGE ?= share.ankra.cloud/library/ankra-cloud-csi
BASE_IMAGE ?= share.ankra.cloud/base/ankra-cloud-csi-base
TAGS ?= sha-$(shell git rev-parse --short=7 HEAD 2>/dev/null || echo dev)
GOLANGCI_LINT ?= golangci-lint

# Where `make sync-client` reads the Ankra Cloud OpenAPI document from. OPENAPI_SPEC, a local file (for example a
# checkout of the Ankra Cloud monorepo's docs/openapi.yaml), wins over OPENAPI_URL.
OPENAPI_URL ?= https://cloud.ankra.app/docs/openapi.yaml
OPENAPI_SPEC ?=

.PHONY: build test sanity vet lint helm-lint manifests client client-check sync-client image image-push check

build:
	CGO_ENABLED=0 go build -trimpath -o bin/ankra-cloud-csi ./cmd/ankra-cloud-csi

# Unit tests and the csi-sanity suite (in-process, against a fake API and a fake mounter), plus the check that the
# generated client matches api/openapi.yaml.
test:
	go test ./... -count=1
	cd tools/openapigen && go test ./... -count=1

sanity:
	go test ./internal/driver -run 'TestSanity' -count=1 -v

vet:
	go vet ./...
	cd tools/openapigen && go vet ./...

lint:
	$(GOLANGCI_LINT) run ./...

# helm lint, a render with and without the snapshot API, and a check that deploy/ matches the chart.
helm-lint:
	helm lint --strict $(CHART) --set api.token=lint-placeholder
	helm lint --strict $(CHART) --set api.existingSecret=ankra-cloud-csi-api
	$(RENDER) --api-versions snapshot.storage.k8s.io/v1/VolumeSnapshotClass >/dev/null
	@rendered="$$(mktemp -d)"; \
	$(RENDER) > "$$rendered/ankra-cloud-csi.yaml"; \
	$(RENDER) --api-versions snapshot.storage.k8s.io/v1/VolumeSnapshotClass --show-only templates/volumesnapshotclass.yaml > "$$rendered/volumesnapshotclass.yaml"; \
	if ! diff -ru $(DEPLOY) "$$rendered" >/dev/null; then \
		echo "helm-lint: $(DEPLOY) is stale; run make manifests" >&2; diff -ru $(DEPLOY) "$$rendered" >&2; rm -rf "$$rendered"; exit 1; \
	fi; \
	rm -rf "$$rendered"

manifests:
	mkdir -p $(DEPLOY)
	$(RENDER) > $(DEPLOY)/ankra-cloud-csi.yaml
	$(RENDER) --api-versions snapshot.storage.k8s.io/v1/VolumeSnapshotClass --show-only templates/volumesnapshotclass.yaml > $(DEPLOY)/volumesnapshotclass.yaml

# Regenerates internal/ankraapi/operations_gen.go from api/openapi.yaml.
client:
	cd tools/openapigen && go run . -specification ../../api/openapi.yaml -package ankraapi -output ../../internal/ankraapi/operations_gen.go

client-check: client
	@git diff --exit-code -- internal/ankraapi || { echo "client-check: run make client and commit the result" >&2; exit 1; }

# Refreshes api/openapi.yaml from the published Ankra Cloud OpenAPI document and regenerates the client.
sync-client:
	@if [ -n "$(OPENAPI_SPEC)" ]; then cp "$(OPENAPI_SPEC)" api/openapi.yaml; \
	else curl -fsSL "$(OPENAPI_URL)" -o api/openapi.yaml.download && mv api/openapi.yaml.download api/openapi.yaml; fi
	$(MAKE) client

# Multi-arch image without a container daemon (needs apko and ko; see hack/image.sh). image builds and discards,
# image-push publishes with the credentials in your Docker config.
image:
	IMAGE=$(IMAGE) BASE_IMAGE=$(BASE_IMAGE) TAGS="$(TAGS)" PUSH=false ./hack/image.sh

image-push:
	IMAGE=$(IMAGE) BASE_IMAGE=$(BASE_IMAGE) TAGS="$(TAGS)" PUSH=true ./hack/image.sh

check: vet test lint helm-lint
