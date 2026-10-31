# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/).

## Unreleased

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
