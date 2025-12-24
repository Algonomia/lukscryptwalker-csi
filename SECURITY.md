# Security Policy

## Reporting Security Vulnerabilities

We take security seriously and appreciate your help in disclosing vulnerabilities responsibly.

### How to Report

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, please report them via one of these methods:

1. **Email:** security@algonomia.com
2. **GitHub Security Advisories:** [Create a private security advisory](https://github.com/Algonomia/lukscryptwalker-csi/security/advisories/new)

### What to Include

Please include the following information:

- Type of vulnerability (e.g., privilege escalation, data exposure, denial of service)
- Affected component (controller, node service, specific function)
- Steps to reproduce the vulnerability
- Proof-of-concept or exploit code (if available)
- Impact assessment
- Suggested fix (if you have one)

### Response Timeline

- **Initial Response:** Within 48 hours
- **Triage and Assessment:** Within 7 days
- **Fix Development:** Based on severity (see below)
- **Public Disclosure:** Coordinated with reporter

### Severity Levels and Response

| Severity | CVSS Score | Response Time | Example |
|----------|------------|---------------|---------|
| Critical | 9.0-10.0 | 24-48 hours | Remote code execution, privilege escalation |
| High | 7.0-8.9 | 7 days | Data exposure, authentication bypass |
| Medium | 4.0-6.9 | 30 days | Limited data exposure, DoS |
| Low | 0.1-3.9 | 90 days | Minor information disclosure |

## Supported Versions

| Version | Supported | EOL Date |
|---------|-----------|----------|
| 1.x.x | ✅ | TBD |
| 0.x.x | ❌ | 2024-01-01 |

## Security Best Practices

### For Users

1. **Secret Management:**
   - Use Kubernetes Secrets with appropriate RBAC
   - Enable encryption at rest for etcd
   - Rotate encryption passphrases regularly
   - Use external secret management solutions in production

2. **Network Security:**
   - Restrict CSI driver pod network access
   - Use NetworkPolicies to limit communication
   - Enable TLS for S3 endpoints

3. **Access Control:**
   - Limit privileged pod access
   - Use Pod Security Policies/Standards
   - Implement least privilege RBAC

4. **Monitoring:**
   - Enable audit logging
   - Monitor for suspicious mount operations
   - Track volume access patterns

### For Operators

1. **Deployment:**
   ```yaml
   # Use specific image versions, not :latest
   image: ghcr.io/yourusername/lukscrypt-csi:v1.0.0

   # Enable security contexts
   securityContext:
     runAsNonRoot: true
     readOnlyRootFilesystem: true
     allowPrivilegeEscalation: false
   ```

2. **Node Security:**
   - Keep cryptsetup updated
   - Restrict access to /dev/mapper devices
   - Monitor LUKS operations

3. **Storage Security:**
   - Use strong passphrases (minimum 32 characters)
   - Enable LUKS2 with Argon2id KDF
   - Regularly verify encryption status

## Security Features

### Built-in Security

- **LUKS Encryption:** AES-256-XTS by default
- **S3 Encryption:** rclone crypt with NaCl SecretBox
- **Secret Protection:** Kubernetes Secrets integration
- **Audit Logging:** Comprehensive operation logging
- **Image Signing:** Container images signed with cosign
- **SBOM:** Software Bill of Materials for supply chain security

### Recommended Hardening

```yaml
# LUKS Local Storage with hardened settings
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: secure-lukscryptwalker-local
provisioner: lukscryptwalker.csi.k8s.io
parameters:
  # Storage location (optional, default: /opt/local-path-provisioner)
  local-path: "/opt/secure-volumes"

  # LUKS encryption secret reference (required)
  csi.storage.k8s.io/node-stage-secret-name: "luks-secure-secret"
  csi.storage.k8s.io/node-stage-secret-namespace: "kube-system"

  # Custom passphrase key (optional, default: "passphrase")
  passphraseKey: "encryption-key"

  # Filesystem type (optional, default: ext4)
  fsType: "ext4"

  # Manual fsGroup override for strict permissions (optional)
  fsGroup: "65534"  # nobody user
---
# S3 Backend with hardened settings (mutually exclusive with LUKS local)
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: secure-lukscryptwalker-s3
provisioner: lukscryptwalker.csi.k8s.io
parameters:
  # S3 backend configuration
  storage-backend: "s3"
  s3-bucket: "secure-encrypted-volumes"
  s3-region: "us-east-1"
  s3-endpoint: "https://s3.example.com"  # Use HTTPS endpoints only
  s3-force-path-style: "true"  # Required for some S3-compatible services
  s3-path-prefix: "secure/volumes"  # Custom path prefix

  # S3 Credentials secret reference
  s3-access-key-id-secret-name: "s3-secure-credentials"
  s3-access-key-id-secret-namespace: "kube-system"

  # LUKS/rclone encryption secret reference (required)
  csi.storage.k8s.io/node-stage-secret-name: "luks-secure-secret"
  csi.storage.k8s.io/node-stage-secret-namespace: "kube-system"
  passphraseKey: "encryption-key"

  # rclone VFS Cache settings for security
  rclone-vfs-cache-mode: "writes"  # Minimize local cache
  rclone-vfs-cache-max-age: "10m"  # Short cache retention
  rclone-vfs-cache-max-size: "1G"  # Limited cache size
  rclone-cache-dir: "/var/lib/lukscrypt-cache/secure"  # Dedicated cache directory
```

## Known Security Considerations

1. **Privileged Operations:** Node service requires privileged access for LUKS operations
2. **Memory Exposure:** Passphrases may briefly exist in memory
3. **Log Sanitization:** Ensure logs don't contain sensitive information
4. **Container Escape:** Privileged containers have elevated risk

## Security Audit

Last security audit: N/A (planned for Q1 2024)

## Compliance

The project aims to comply with:
- CIS Kubernetes Benchmark
- NIST Cybersecurity Framework
- PCI-DSS for encrypted storage
- GDPR for data protection

## Security Tools

We use the following security tools:
- **Static Analysis:** Gosec, CodeQL
- **Dependency Scanning:** Dependabot, Trivy
- **Container Scanning:** Trivy, Cosign
- **License Compliance:** FOSSA

## Contact

- **Security Email:** security@algonomia.com
- **Response Team:** @algonomia-security-team

## Acknowledgments

We thank the following researchers for responsibly disclosing vulnerabilities:
- (List will be populated as vulnerabilities are reported and fixed)

## Resources

- [OWASP Kubernetes Security Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Kubernetes_Security_Cheat_Sheet.html)
- [Kubernetes Security Best Practices](https://kubernetes.io/docs/concepts/security/)
- [CSI Security Recommendations](https://kubernetes-csi.github.io/docs/)