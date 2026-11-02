# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/).

## Unreleased

## v0.3.0

### Added

- Storage capacity tracking. The controller implements `GetCapacity` and advertises `GET_CAPACITY`; the chart sets the
  CSIDriver's `storageCapacity: true` and runs external-provisioner with `--enable-capacity`, so a `CSIStorageCapacity`
  is published for every node and StorageClass, and the scheduler binds a `WaitForFirstConsumer` claim only to a node
  whose capacity fits it. The capacity is the Ankra Cloud API's own answer (`GET /v1/servers/{id}/storage-capacity`):
  for `local-nvme`, and for a class without a tier in a zone whose default is `local-nvme`, the room left in the Ankra
  Local pool of the node's compute node under the pool's used thresholds and overcommit ratio, computed by the rule
  that refuses `create_storage` with `409 the node's Ankra Local pool takes no new volumes`. Before, a claim could be
  bound to a node whose pool was full; `CreateVolume` then failed until other volumes were deleted, which held CI
  workspaces `Pending` for 15 to 19 minutes on ankra-ci-sto1 (ankra-q573dh.79.5).
- Tiers the API does not limit per node (the Ankra Storage tiers, a pool whose agent reports nothing yet, a tier the
  zone refuses everywhere) and a server the API does not know report the largest volume the API takes (4096 GiB),
  which is what the scheduler assumed before: capacity only steers the choice of node, and a claim no node can take
  still gets the API's reason from `CreateVolume`. Other API failures leave the last published capacity in place.
- One read of a server's capacity answers its StorageClasses for `--capacity-cache-ttl` (10s), so a poll costs one
  API call per node. Values `capacity.enabled` (default `true`), `capacity.pollInterval` (`30s`) and
  `capacity.cacheTTL` (`10s`); the controller gets a namespaced Role for `csistoragecapacities` and for reading its
  pod and ReplicaSet (the objects are owned by the controller Deployment).

## v0.2.2

### Fixed

- Release builds now use the digest-pinned Go 1.26.10 toolchain, including the upstream crypto/x509
  certificate verification fix in GO-2026-6809. Both Go modules require that patched minimum version.
  The driver also updates golang.org/x/net to 0.62.0, which fixes GO-2026-6860 and GO-2026-6866.
  This release retains the HTTP/2 connection health checks from 0.2.1 and changes no storage classes,
  credentials or volume configuration (ankra-t9c7w.36.16).

## v0.2.1

### Fixed

- The API client health-checks its HTTP/2 connection: a connection that stops answering is pinged after 15 seconds
  without a frame and closed when the ping goes unanswered for 10 seconds, so the next call dials a fresh one. Before,
  every call kept going onto the dead connection and waited out the 60-second timeout until the kernel reset it, minutes
  later, and workspace volumes stayed `Pending` the whole time (ankra-t9c7w.36.24).

## v0.2.0

### Fixed

- A `local-nvme` volume is created on the compute node that runs the scheduled node's server (`placement.server_id` on
  `create_storage`). Before, the API placed it on the least-loaded node and attaching it failed with `409 Conflict:
  local-nvme volumes live on one node`.

### Changed

- The default StorageClass is `ankra-default`, which names no tier: each volume gets its zone's default storage tier,
  so claims without a class work in zones without Ankra Storage (where only `local-nvme` is offered). `ankra-standard`
  stays and is no longer the default.
- A StorageClass without a `tier` parameter now means the zone's default tier, not `standard`.
- The API client is regenerated from the current Ankra Cloud OpenAPI document.

## v0.1.0

First public release.

### Added

- The CSI driver `csi.ankra.cloud`: dynamic provisioning for the `standard`, `maxiops`, `hdd` and `local-nvme` tiers,
  hot-plug attach and detach, online expansion, snapshots, restore from a snapshot and clones, in `Filesystem` and
  `Block` volume mode (ReadWriteOnce).
- Topology by zone, and by server for `local-nvme` volumes; every StorageClass binds on first consumer.
- The Helm chart `ankra-cloud-csi`, published to https://ankraio.github.io/ankra-charts and
  `oci://share.ankra.cloud/charts`, and plain manifests in `deploy/`.
- Multi-arch images (linux/amd64, linux/arm64) at `share.ankra.cloud/library/ankra-cloud-csi`, built without a daemon
  with apko and ko, with an SPDX SBOM.
