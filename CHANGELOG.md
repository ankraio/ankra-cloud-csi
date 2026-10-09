# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/).

## Unreleased

## Chart 0.2.0

A chart release; the driver stays at v0.1.0.

### Security

- The pinned Kubernetes CSI sidecars move to current releases, which are built with newer Go and gRPC:
  csi-provisioner v6.3.0, csi-attacher v4.13.0, csi-resizer v2.2.1, csi-snapshotter v8.6.0,
  csi-node-driver-registrar v2.18.0 and livenessprobe v2.20.0.

### Changed

- The chart needs Kubernetes 1.34 or later (was 1.27), the minimum of csi-provisioner v6 and csi-resizer v2.
- The controller's ClusterRole may update VolumeSnapshots, so csi-provisioner v6 can keep a snapshot from being
  deleted while a volume is restored from it.

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
