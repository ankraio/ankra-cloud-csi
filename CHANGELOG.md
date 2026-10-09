# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/).

## Unreleased

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
