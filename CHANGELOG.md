# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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