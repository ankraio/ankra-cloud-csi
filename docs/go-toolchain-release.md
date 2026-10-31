# Release toolchain verification

The driver release must use the patched Go toolchain, not merely pass a scan run
under a different toolchain. This work is tracked by `ankra-t9c7w.36.16`.

On 2026-10-31, release pipeline 28 for `7e9434f` reported the published Go advisory
[GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809). The optional scan used Go
1.26.9. The ARM64 driver extracted without execution from published image index
`sha256:5175923da8d71b73229ac045300e24afa79d667d39afb08ab5edc8ed0dd6746c`
also reports `go1.26.9` in its build information. The image stage inherits the
same pinned pipeline toolchain and uses `ko build`.

The release change pins the official multi-platform Go 1.26.10 image by digest
for every inherited pipeline stage. It retains the HTTP/2 connection health
checks released in 0.2.1. A new chart and driver patch version is required:
published version 0.2.1 must never be overwritten or reused for a rebuild.

Validation includes the repository's vet, unit and CSI sanity tests, lint,
chart rendering checks, and a binary-mode vulnerability scan of the rebuilt
driver. Both image architectures must retain the same source revision and
patched toolchain. Pipeline-definition approval is part of the ordinary audited
release path; no gate is bypassed. Existing unrelated optional findings are not
reported as clean.

The storage incident rollout remains tracked separately by `ankra-t9c7w.36.24`.
Installing the new chart must preserve the existing storage classes, credentials,
volume objects and driver configuration. Recovery is verified from successful
volume provisioning and attachment, not from a healthy controller pod alone.
