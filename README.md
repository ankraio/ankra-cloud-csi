# ankra-cloud-csi

The Kubernetes CSI driver `csi.ankra.cloud` for [Ankra Cloud](https://cloud.ankra.app) block storage. Clusters running
on Ankra Cloud servers get persistent volumes backed by Ankra Cloud storages: dynamic provisioning per storage tier,
hot-plug attach and detach, online expansion, snapshots, restore from a snapshot and clones. The driver talks only to
the public Ankra Cloud API, with a customer API token.

The driver is in preview. Full documentation: <https://cloud.ankra.app/docs/kubernetes-csi>.

## Quick start

```bash
helm repo add ankra https://ankraio.github.io/ankra-charts
helm repo update
helm install ankra-cloud-csi ankra/ankra-cloud-csi -n kube-system --set api.token=<token>
```

`<token>` is an Ankra Cloud API token whose user can operate storages (**Settings → API tokens** in the console). For production,
keep the token out of Helm values and point the chart at a Secret you manage:

```bash
kubectl -n kube-system create secret generic ankra-cloud-csi-api --from-literal=token=<token>
helm install ankra-cloud-csi ankra/ankra-cloud-csi -n kube-system --set api.existingSecret=ankra-cloud-csi-api
```

The chart is also published as an OCI artifact:

```bash
helm install ankra-cloud-csi oci://share.ankra.cloud/charts/ankra-cloud-csi --version 0.2.0 -n kube-system \
  --set api.existingSecret=ankra-cloud-csi-api
```

Or apply the rendered manifests (create the Secret above first):

```bash
kubectl apply -f https://raw.githubusercontent.com/ankraio/ankra-cloud-csi/main/deploy/ankra-cloud-csi.yaml
# Needs the snapshot CRDs and snapshot-controller (github.com/kubernetes-csi/external-snapshotter):
kubectl apply -f https://raw.githubusercontent.com/ankraio/ankra-cloud-csi/main/deploy/volumesnapshotclass.yaml
```

Then claim a volume:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: ankra-default
  resources:
    requests:
      storage: 10Gi
```

## What gets installed

- The `CSIDriver` object `csi.ankra.cloud`.
- A controller Deployment: the driver with the external-provisioner, -attacher, -resizer and -snapshotter sidecars
  and a liveness probe.
- A node DaemonSet: the driver, node-driver-registrar and a liveness probe. It uses the host network, so the metadata
  service sees the server's own address.
- RBAC for both.
- The StorageClasses `ankra-default` (default), `ankra-standard`, `ankra-maxiops`, `ankra-hdd` and `ankra-local-nvme`.
  All bind on first consumer. `ankra-default` names no tier: each volume gets its zone's default storage tier
  (`default_storage_tier` of `GET /v1/zones/{zone}/capabilities`), which is `local-nvme` in a zone without Ankra
  Storage.
- The `ankra-snapshots` VolumeSnapshotClass, when the cluster serves `snapshot.storage.k8s.io/v1`. The chart does not
  install the snapshot CRDs or the snapshot-controller.

Only single-node writer (ReadWriteOnce) is supported, in `Filesystem` and `Block` volume mode.

## Values

| Value | Default | Description |
| --- | --- | --- |
| `api.url` | `https://cloud.ankra.app` | The Ankra Cloud API's base URL. |
| `api.existingSecret` | `""` | A Secret holding the API token. The chart creates one from `api.token` when empty. |
| `api.existingSecretKey` | `token` | The key of the token in the Secret. |
| `api.token` | `""` | An API token. Prefer `api.existingSecret`. |
| `api.caBundle.existingSecret` | `""` | A Secret with a PEM CA bundle for a privately signed API certificate. |
| `api.caBundle.key` | `ca.crt` | The key of the bundle in that Secret. |
| `defaultZone` | `""` | Zone of volumes created without a topology; empty reads the controller's own zone. |
| `image.repository` | `share.ankra.cloud/library/ankra-cloud-csi` | The driver image (linux/amd64, linux/arm64). |
| `image.tag` | the chart's `appVersion` | An immutable tag: `v<semver>` or `sha-<commit>`. |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `sidecars.*.image` | pinned | The Kubernetes CSI sidecars. Upgrade them together with the chart. |
| `controller.replicas` | `1` | |
| `controller.operationTimeout` | `5m` | How long one CSI call waits for an API operation. |
| `controller.sidecarTimeout` | `6m` | The sidecars' per-call timeout; above `operationTimeout` so the driver answers first. |
| `controller.resources`, `.nodeSelector`, `.tolerations`, `.affinity`, `.priorityClassName` | see `values.yaml` | Scheduling of the controller. |
| `node.kubeletDir` | `/var/lib/kubelet` | The kubelet's root directory. |
| `node.maxVolumesPerNode` | `15` | Volumes one server can attach: 16 storage devices less its boot storage. |
| `node.resources`, `.nodeSelector`, `.tolerations`, `.priorityClassName` | see `values.yaml` | Scheduling of the node plugin. |
| `storageClasses` | the zone default and four tiers | `name`, `tier` and `default` per StorageClass; an empty `tier` is the zone's default tier. |
| `storageClassDefaults.reclaimPolicy` | `Delete` | |
| `storageClassDefaults.allowVolumeExpansion` | `true` | |
| `storageClassDefaults.fsType` | `ext4` | `ext4` or `xfs`. |
| `volumeSnapshotClass.enabled` | `true` | Rendered only when the snapshot API is served. |
| `volumeSnapshotClass.name` | `ankra-snapshots` | |
| `volumeSnapshotClass.default` | `true` | |
| `volumeSnapshotClass.deletionPolicy` | `Delete` | |

The binary reads these environment variables, which the chart sets:

| Variable | |
| --- | --- |
| `ANKRA_CLOUD_API_URL` | The API's base URL. Required. |
| `ANKRA_CLOUD_TOKEN` | An API token that may operate storages. Required. |
| `ANKRA_CLOUD_CA_BUNDLE` | A PEM file of extra roots. Optional. |
| `ANKRA_CLOUD_ZONE` | The controller's default zone (`--default-zone`). Optional. |
| `ANKRA_CLOUD_SERVER_ID` | Skips the metadata service on the node (`--server-id`). Optional. |

## How it maps to the API

| CSI | Ankra Cloud |
| --- | --- |
| Volume id | Storage id. The CSI volume name (`pvc-…`) is the storage title, the idempotency key: `CreateVolume` lists storages and reuses the one with that title. Storages have no labels yet; `csi.ankra.cloud/volume-name` is sent once they do. |
| `CreateVolume` | `create_storage`: size rounded up to whole GiB (at least 1, at most 4096), `tier` from the StorageClass (left out when the class names none, so the API uses the zone's `default_storage_tier`), zone from `topology.ankra.cloud/zone`. A new `local-nvme` (or zone-default) volume sends `placement.server_id` = the scheduled node's server, so the storage lands on the compute node that runs it. A snapshot source sends `source_snapshot_id`, a volume source `source_storage_id`. A storage left in `error` is deleted and created again. |
| `DeleteVolume` | `delete_storage`; a missing storage is success, an attached one is `FAILED_PRECONDITION`. |
| `ControllerPublishVolume` | `attach_storage` to the node's server (hot-plug), waits for the operation. Returns the device serial in the publish context. |
| `ControllerUnpublishVolume` | `detach_storage` (hot-unplug); a missing or elsewhere-attached storage is success. |
| `ControllerExpandVolume` | `resize_storage` while attached (online); `node_expansion_required` for file system volumes. |
| `CreateSnapshot` / `DeleteSnapshot` / `ListSnapshots` | `create_snapshot`, `delete_snapshot`, `get_snapshot`, `list_storage_snapshots`. Snapshot names are snapshot titles; listing all snapshots walks every storage. |
| `NodeGetInfo` | Node id = server id from `http://[fd00:ec2::254]/latest/meta-data/instance-id`, then `http://169.254.169.254/…`; the zone from `get_server`. Topology: zone and `topology.ankra.cloud/node` = server id. At most 15 volumes (16 storage devices per server less the boot storage). |
| `NodeStageVolume` | Waits for `/dev/disk/by-id/virtio-<serial>` (serial = the first 20 characters of the storage id), formats it ext4 (default) or xfs when blank, mounts it at the staging path. Block volumes are not staged. |
| `NodePublishVolume` | Bind mount of the staging path (or of the device, for block volumes) at the target path, `ro` when read-only. |
| `NodeExpandVolume` | `resize2fs` or `xfs_growfs`. |
| `NodeGetVolumeStats` | `statfs` bytes and inodes; the device size for block volumes. |

Only single-node writer (ReadWriteOnce) is supported, in `Filesystem` and `Block` volume mode.

API errors become gRPC codes: 400 `INVALID_ARGUMENT`, 401 `UNAUTHENTICATED`, 403 `PERMISSION_DENIED`, 404
`NOT_FOUND`, 409 `FAILED_PRECONDITION`, 422 (quota, no free device slot) `RESOURCE_EXHAUSTED`, 429 and 502-504
`UNAVAILABLE`, a failed operation `INTERNAL`. Two calls for the same volume at once answer `ABORTED`.

### Topology and WaitForFirstConsumer

Every StorageClass binds on first consumer. The external-provisioner then passes the scheduled node's topology first
in `preferred`, so the storage is created in that node's zone. A `local-nvme` (Ankra Local) volume lives on one
compute node's own disks, so the driver creates it with `placement.server_id` = the server id of the scheduled node,
which puts the storage on the compute node that runs that server, and reports `topology.ankra.cloud/node` = that
server id; Kubernetes then only ever schedules the volume's pods onto that node. A clone stays on its source's node. A `local-nvme` request without a node in
its topology (an `Immediate` StorageClass) is refused with `INVALID_ARGUMENT`.

### Operations the API is still gaining

Snapshots (`create_snapshot`, `list_storage_snapshots`, `get_snapshot`, `delete_snapshot`) are called in
`internal/cloud/pending.go`. Each call names its operationId: when the regenerated
client (`make sync-client`) knows the operation, it goes through `ankraapi.Client.Call` with the specification's method and
path, otherwise it is a plain HTTP request to the route the API documents. Everything else uses the typed generated client.
The device serial is derived from the storage id until the generated `Storage` carries `device_serial`.

## Images and releases

Images are multi-arch (linux/amd64, linux/arm64) and published to the public registry `share.ankra.cloud`, pullable
without credentials:

- `share.ankra.cloud/library/ankra-cloud-csi:v<semver>` for each release, `:sha-<commit>` for each commit on main.
  Tags are immutable; there is no `latest`.
- `share.ankra.cloud/base/ankra-cloud-csi-base`: the Wolfi base with the file system tools the node plugin runs
  (`build/base.apko.yaml`).

Each image carries an SPDX SBOM in the registry. The chart is published to the Helm repository
`https://ankraio.github.io/ankra-charts` and to `oci://share.ankra.cloud/charts`.

CI runs on [Ankra Pipelines](.ankra/pipeline.yaml): go vet, the unit tests, the csi-sanity suite, golangci-lint,
govulncheck and the chart gates on every push and pull request; the image build on every pull request; publishing on
main, where a commit that sets a new `appVersion` in the chart is the release. See [CHANGELOG.md](CHANGELOG.md) for
what each release changed.

## Layout

| Path | What |
| --- | --- |
| `cmd/ankra-cloud-csi` | The binary: `-mode controller`, `-mode node` or `-mode all`. |
| `internal/driver` | Identity, controller and node services; `Host` is the node's mounts and disks (`HostOS` uses `k8s.io/mount-utils`). |
| `internal/cloud` | `API`, the narrow interface over the calls the driver makes, and `Client`, its implementation. |
| `internal/cloud/cloudfake` | The in-memory API the unit tests and csi-sanity run against. |
| `internal/ankraapi` | The Ankra Cloud API client, generated from `api/openapi.yaml` by `tools/openapigen`. |
| `internal/metadata` | Reads the server id from the metadata service. |
| `charts/ankra-cloud-csi` | The Helm chart. |
| `deploy/` | Plain manifests rendered from the chart (`make manifests`). |
| `build/`, `hack/image.sh`, `.ko.yaml` | The image build. |

## Develop

```bash
make vet test        # go vet, the unit tests, csi-sanity, and the check that the API client is current
make sanity          # the csi-sanity suite on its own (in-process, fake API, fake mounter)
make lint            # golangci-lint v2
make helm-lint       # helm lint, helm template, and a check that deploy/ matches the chart
make manifests       # re-render deploy/
make sync-client     # refresh api/openapi.yaml from https://cloud.ankra.app/docs/openapi.yaml and regenerate the client
make image           # build the multi-arch image without pushing (needs apko and ko)
```

`make sync-client OPENAPI_SPEC=<path>` regenerates from a local copy of the OpenAPI document instead of the published
one. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
