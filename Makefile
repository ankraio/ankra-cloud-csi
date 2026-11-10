# ankra-cloud-csi. `make check` runs every gate the pipeline (.ankra/pipeline.yaml) runs, except the image build.

CHART := charts/ankra-cloud-csi
COSI_CHART := charts/ankra-cloud-cosi
DEPLOY := deploy
RENDER := helm template ankra-cloud-csi $(CHART) --namespace kube-system --set api.existingSecret=ankra-cloud-csi-api
COSI_RENDER := helm template ankra-cloud-cosi $(COSI_CHART) --namespace kube-system --set api.existingSecret=ankra-cloud-csi-api
COSI_API := objectstorage.k8s.io/v1alpha1/BucketClass
IMAGE ?= share.ankra.cloud/library/ankra-cloud-csi
BASE_IMAGE ?= share.ankra.cloud/base/ankra-cloud-csi-base
COSI_IMAGE ?= share.ankra.cloud/library/ankra-cloud-cosi
COSI_BASE_IMAGE ?= share.ankra.cloud/base/ankra-cloud-cosi-base
TAGS ?= sha-$(shell git rev-parse --short=7 HEAD 2>/dev/null || echo dev)
GOLANGCI_LINT ?= golangci-lint

# Where `make sync-client` reads the Ankra Cloud OpenAPI document from. OPENAPI_SPEC, a local file (for example a
# checkout of the Ankra Cloud monorepo's docs/openapi.yaml), wins over OPENAPI_URL.
OPENAPI_URL ?= https://cloud.ankra.app/docs/openapi.yaml
OPENAPI_SPEC ?=

.PHONY: build test sanity vet lint helm-lint manifests client client-check sync-client image image-push check

build:
	CGO_ENABLED=0 go build -trimpath -o bin/ankra-cloud-csi ./cmd/ankra-cloud-csi
	CGO_ENABLED=0 go build -trimpath -o bin/ankra-cloud-cosi ./cmd/ankra-cloud-cosi

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

# helm lint of both charts, renders with and without the snapshot and COSI APIs, and a check that deploy/ matches the
# charts. deploy/cosi-controller is the pinned install of the upstream COSI controller, not a render.
helm-lint:
	helm lint --strict $(CHART) --set api.token=lint-placeholder
	helm lint --strict $(CHART) --set api.existingSecret=ankra-cloud-csi-api
	$(RENDER) --api-versions snapshot.storage.k8s.io/v1/VolumeSnapshotClass >/dev/null
	helm lint --strict $(COSI_CHART) --set api.token=lint-placeholder
	helm lint --strict $(COSI_CHART) --set api.existingSecret=ankra-cloud-csi-api --set bucketClass.zone=fsn1 \
		--set bucketClass.bucketNamePrefix=team-a --api-versions $(COSI_API)
	$(COSI_RENDER) --api-versions $(COSI_API) --set bucketClass.zone=fsn1 --show-only templates/bucketclass.yaml >/dev/null
	$(COSI_RENDER) --api-versions $(COSI_API) --show-only templates/bucketaccessclass.yaml >/dev/null
	@rendered="$$(mktemp -d)"; \
	$(RENDER) > "$$rendered/ankra-cloud-csi.yaml"; \
	$(RENDER) --api-versions snapshot.storage.k8s.io/v1/VolumeSnapshotClass --show-only templates/volumesnapshotclass.yaml > "$$rendered/volumesnapshotclass.yaml"; \
	$(COSI_RENDER) > "$$rendered/ankra-cloud-cosi.yaml"; \
	if ! diff -ruB -x cosi-controller $(DEPLOY) "$$rendered" >/dev/null; then \
		echo "helm-lint: $(DEPLOY) is stale; run make manifests" >&2; diff -ruB -x cosi-controller $(DEPLOY) "$$rendered" >&2; rm -rf "$$rendered"; exit 1; \
	fi; \
	rm -rf "$$rendered"

manifests:
	mkdir -p $(DEPLOY)
	$(RENDER) > $(DEPLOY)/ankra-cloud-csi.yaml
	$(RENDER) --api-versions snapshot.storage.k8s.io/v1/VolumeSnapshotClass --show-only templates/volumesnapshotclass.yaml > $(DEPLOY)/volumesnapshotclass.yaml
	$(COSI_RENDER) > $(DEPLOY)/ankra-cloud-cosi.yaml

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

# Multi-arch images without a container daemon (needs apko and ko; see hack/image.sh): the CSI driver and the COSI
# driver. image builds and discards, image-push publishes with the credentials in your Docker config.
image:
	IMAGE=$(IMAGE) BASE_IMAGE=$(BASE_IMAGE) TAGS="$(TAGS)" PUSH=false ./hack/image.sh
	IMAGE=$(COSI_IMAGE) BASE_IMAGE=$(COSI_BASE_IMAGE) COMMAND=./cmd/ankra-cloud-cosi BASE_CONFIG=build/cosi-base.apko.yaml \
		TAGS="$(TAGS)" PUSH=false ./hack/image.sh

image-push:
	IMAGE=$(IMAGE) BASE_IMAGE=$(BASE_IMAGE) TAGS="$(TAGS)" PUSH=true ./hack/image.sh
	IMAGE=$(COSI_IMAGE) BASE_IMAGE=$(COSI_BASE_IMAGE) COMMAND=./cmd/ankra-cloud-cosi BASE_CONFIG=build/cosi-base.apko.yaml \
		TAGS="$(TAGS)" PUSH=true ./hack/image.sh

check: vet test lint helm-lint
