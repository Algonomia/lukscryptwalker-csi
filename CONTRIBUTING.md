# Contributing to LUKSCryptWalker CSI

Thank you for your interest in contributing to LUKSCryptWalker CSI! This document provides guidelines and instructions for contributing to the project.

## Code of Conduct

Please read and follow our [Code of Conduct](CODE_OF_CONDUCT.md) to ensure a welcoming environment for all contributors.

## How to Contribute

### Reporting Issues

- Check if the issue already exists in the [GitHub Issues](https://github.com/Algonomia/lukscryptwalker-csi/lukscrypt-csi/issues)
- Use the issue templates when creating new issues
- Provide detailed information including:
  - Kubernetes version
  - CSI driver version
  - Steps to reproduce
  - Expected vs actual behavior
  - Relevant logs and configuration

### Suggesting Features

- Open a [discussion](https://github.com/Algonomia/lukscryptwalker-csi/discussions) first
- Describe the use case and benefits
- Consider implementation complexity
- Be open to feedback and alternatives

### Contributing Code

#### Development Setup

1. **Fork and clone the repository:**
   ```bash
   git clone https://github.com/Algonomia/lukscryptwalker-csi.git
   cd lukscrypt-csi
   ```

2. **Install dependencies:**
   ```bash
   # Install Go 1.21+
   wget https://go.dev/dl/go1.21.linux-amd64.tar.gz
   sudo tar -C /usr/local -xzf go1.21.linux-amd64.tar.gz
   export PATH=$PATH:/usr/local/go/bin

   # Install development tools
   make deps
   go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
   ```

3. **Set up pre-commit hooks:**
   ```bash
   cat > .git/hooks/pre-commit << 'EOF'
   #!/bin/sh
   make fmt
   make lint
   make test
   EOF
   chmod +x .git/hooks/pre-commit
   ```

#### Development Workflow

1. **Create a feature branch:**
   ```bash
   git checkout -b feature/your-feature-name
   ```

2. **Make your changes:**
   - Follow the coding standards (see below)
   - Add/update tests
   - Update documentation
   - Add examples if applicable

3. **Test your changes:**
   ```bash
   # Run unit tests
   make test

   # Run linting
   make lint

   # Test in Kind cluster
   make kind-deploy
   ```

4. **Commit your changes:**
   ```bash
   # Use conventional commits
   git commit -m "feat: add S3 encryption support"
   git commit -m "fix: resolve LUKS mounting issue"
   git commit -m "docs: update installation guide"
   ```

5. **Push and create a Pull Request:**
   ```bash
   git push origin feature/your-feature-name
   ```

### Coding Standards

#### Go Code Style

- Follow [Effective Go](https://golang.org/doc/effective_go)
- Use `gofmt` and `goimports`
- Pass `golangci-lint` checks
- Maintain 80% test coverage for new code

#### Code Organization

```
pkg/
├── driver/           # CSI driver implementation
│   ├── controller.go # Controller service
│   ├── node.go       # Node service
│   └── identity.go  # Identity service
├── rclone/          # rclone integration
├── secrets/         # Secret management
└── utils/           # Utility functions
```

#### Error Handling

```go
// Good
if err != nil {
    return fmt.Errorf("failed to mount volume %s: %w", volumeID, err)
}

// Bad
if err != nil {
    return err
}
```

#### Logging

```go
// Use structured logging
klog.V(2).Infof("Mounting volume %s at %s", volumeID, targetPath)
klog.Errorf("Failed to mount volume %s: %v", volumeID, err)
```

### Testing

#### Unit Tests

- Place tests in `*_test.go` files
- Use table-driven tests
- Mock external dependencies
- Aim for 80% coverage

```go
func TestVolumeMount(t *testing.T) {
    tests := []struct {
        name    string
        input   MountRequest
        wantErr bool
    }{
        {
            name:    "valid mount request",
            input:   MountRequest{...},
            wantErr: false,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            err := Mount(tt.input)
            if (err != nil) != tt.wantErr {
                t.Errorf("Mount() error = %v, wantErr %v", err, tt.wantErr)
            }
        })
    }
}
```

#### Integration Tests

- Test against real Kubernetes clusters
- Use Kind or Minikube for local testing
- Verify CSI operations end-to-end

### Commit Messages

Follow [Conventional Commits](https://www.conventionalcommits.org/):

- `feat:` New feature
- `fix:` Bug fix
- `docs:` Documentation changes
- `style:` Code style changes (formatting, etc.)
- `refactor:` Code refactoring
- `test:` Test additions or modifications
- `chore:` Maintenance tasks
- `perf:` Performance improvements
- `ci:` CI/CD changes

Examples:
```
feat: add support for XFS filesystem
fix: resolve race condition in volume provisioning
docs: update S3 configuration examples
chore: bump Go version to 1.21
```

### Pull Request Process

1. **PR Title:** Use conventional commit format
2. **Description:**
   - Explain what changes were made
   - Why the changes are necessary
   - Any breaking changes
   - Testing performed
3. **Checklist:**
   - [ ] Tests pass (`make test`)
   - [ ] Linting passes (`make lint`)
   - [ ] Documentation updated
   - [ ] Changelog entry added (if applicable)
4. **Review:** Address reviewer feedback promptly
5. **Merge:** Squash and merge after approval

### Release Process

Releases are automated via GitHub Actions:

1. **Version Bump:**
   ```bash
   # For patch release (1.0.0 -> 1.0.1)
   git tag v1.0.1

   # For minor release (1.0.0 -> 1.1.0)
   git tag v1.1.0

   # For major release (1.0.0 -> 2.0.0)
   git tag v2.0.0
   ```

2. **Push Tag:**
   ```bash
   git push origin v1.0.1
   ```

3. **Automated Process:**
   - CI builds and tests
   - Multi-arch Docker images pushed to ghcr.io
   - Helm chart published to GitHub Pages
   - GitHub release created with changelog

### Documentation

- Keep README.md up to date
- Document new features with examples
- Update API documentation
- Add inline code comments for complex logic
- Include architecture diagrams when helpful

### Security

- Never commit secrets or credentials
- Report security issues privately (see [SECURITY.md](SECURITY.md))
- Run security scanners before submitting PRs
- Keep dependencies updated

## Getting Help

- **Discussions:** [GitHub Discussions](https://github.com/Algonomia/lukscryptwalker-csi/discussions)
- **Email:** brice.guillaume@algonomia.com

## Recognition

Contributors are recognized in:
- Release notes
- CONTRIBUTORS.md file
- GitHub contributors page

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.

Thank you for contributing to LUKSCryptWalker CSI!