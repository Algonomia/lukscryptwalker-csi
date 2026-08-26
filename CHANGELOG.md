# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- **Consumers left reading EIO forever after a re-bind.** Bind mounts are made in the
  host mount namespace but were unmounted from the driver's own, so the old mount could
  survive and the fresh bind stacked on top of it. A container started before the re-bind
  keeps the buried mount, whose VFS has already been shut down, and every operation that
  reaches the backend comes back `ListObjects, context canceled` → EIO — while `stat()`,
  which resolves only the topmost mount, reported the path healthy. Unmounts now run in
  the host namespace and unwind every layer, verified against the host mount table.
- The host mount table was parsed into `mountpoint → fstype`, which cannot represent two
  mounts at one path, so stacked mounts were invisible to every check built on it. It now
  keeps the full stack; `HostMountDepth` answers whether a path is *fully* unmounted.
- `consumerMountHealthy` compared `stat()` device numbers and so passed a consumer whose
  container still held a stale mount buried under the live one. It now rejects any
  stacked bind.
- **Consumers stranded on a superseded mount are now recovered.** Re-binding on the host
  never moves a running container: its mounts are its own entries in its own namespace.
  So a consumer whose re-bind it could not see kept reading the old superblock — whose
  VFS is shut down — while the bind path itself looked perfectly healthy. Back-to-back
  reconciles made this routine, because the second one hit the consumer-restart cooldown
  and left the container behind with nothing to revisit it. Every 5 minutes the checker
  now compares what each consumer's containers actually hold (`/proc/<pid>/mountinfo`)
  against the devices the host still resolves, clears any stacked layers, re-points the
  bind, re-checks whether propagation carried it into the container, and restarts only
  those it did not — one per sweep, under the existing per-volume cooldown, so a node
  full of stranded consumers recovers in stages rather than all at once.
- **Silent data loss when a pod moved to another node.** `NodeUnstageVolume` reported
  success while the volume's writes were still only in the node's local VFS cache. That
  success is the CO's signal that the volume may be published elsewhere, so the consumer
  restarted on another node against an S3 prefix missing everything still queued — an
  empty or stale volume, with no error anywhere. Unstaging now fails with
  `FailedPrecondition` until the upload is confirmed, so the consumer cannot be
  rescheduled onto a stale view of its own data.
- Every path that could not observe the write-back queue — a dead FUSE mount, a missing
  VFS after a driver restart, a leaked duplicate VFS — answered "queue empty". These now
  fall back to the on-disk cache metadata, which stays truthful without a live VFS.
- A drain that failed cleared its own `drain-pending` marker, destroying the only record
  that the node still held the volume's sole copy. The marker is now kept on failure and
  cleared only on a confirmed upload.
- Unreadable or truncated VFS cache metadata was treated as "uploaded", which let the
  orphan sweeper reclaim a cache directory holding unuploaded writes. It now counts as
  unuploaded.
- The VFS cache directory was deleted whenever the write-back queue reported empty, even
  if the cache itself still held dirty items; both must now agree.
- The stale-mount checker skipped any volume with a pending drain marker, including ones
  with no drain actually running — which blocked the re-mount that would have uploaded
  the cache. It now skips only volumes with a live drain.

### Added
- Stranded-upload recovery: a periodic sweep re-mounts volumes that kubelet no longer
  stages on this node but whose VFS cache still holds unuploaded writes, uploads them,
  and unmounts. Previously such data was only recoverable by hand.
- `UnuploadedData` warning events on the PVC naming the node that holds unuploaded
  writes. The cache is node-local and the PV records nothing about it, so this is the
  only cluster-visible signal that a volume is pinned to one node.
- `lukscryptwalker.io/force-unstage: "true"` annotation (on the PVC or PV) to unstage a
  volume anyway, abandoning the unuploaded writes. For when S3 is unreachable for good
  and the consumer must be allowed to move. The local cache is kept for manual recovery.
- VFS cache directory cleanup mechanism to prevent I/O errors from full LUKS volume
- Background goroutine for periodic empty directory cleanup in VFS cache
- Disk usage monitoring with configurable threshold for aggressive cache cleanup
- `CachePollInterval` configuration for rclone VFS cache (ensures stale file cleanup)
- New Helm chart parameters: `node.vfsCacheCleanupInterval`, `node.vfsCacheDiskThreshold`
- New StorageClass parameter: `rclone-vfs-cache-poll-interval`

### Fixed
- VFS cache directories not being cleaned up after cache expiration (rclone limitation workaround)
- I/O errors when VFS cache fills up the LUKS-encrypted volume

### Added
- Enterprise-grade CI/CD pipeline with GitHub Actions
- Multi-architecture Docker builds (amd64, arm64)
- Container image signing with cosign and SBOM generation
- Comprehensive security scanning (Gosec, Trivy, CodeQL)
- Automated Helm chart publishing to GitHub Pages
- Dependabot configuration for dependency updates
- Integration tests with Kind cluster
- Contributing guidelines and security policy
- Testing infrastructure and examples

### Changed
- Updated to Go 1.24 for latest features and performance
- Optimized Dockerfile for multi-arch builds
- Enhanced CI pipeline with Alpine-based lightweight images

### Security
- Added container image signing with cosign
- Implemented SBOM generation for supply chain security
- Added comprehensive security scanning in CI

## [1.0.0] - TBD

### Added
- Initial release of LUKSCryptWalker CSI driver
- LUKS encryption for local storage
- S3 backend support with rclone
- Kubernetes CSI compliance
- Helm chart for easy deployment

### Security
- AES-256-XTS encryption by default
- Secure passphrase management via Kubernetes Secrets
- Client-side encryption for S3 storage