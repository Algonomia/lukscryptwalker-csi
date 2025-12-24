# LUKSCryptWalker - Encrypted CSI Driver

A Kubernetes CSI driver that provides encrypted storage with two backend options: **LUKS local storage** or **S3-compatible encrypted storage**.

## Features

### Core Features
- **Dual Storage Backends**: Choose between local LUKS-encrypted storage or S3-compatible encrypted storage
- **Strong Encryption**: LUKS (AES-256-XTS) for local storage, rclone crypt (NaCl SecretBox) for S3
- **Secret Management**: Integrates with Kubernetes secrets for passphrase management
- **CSI Compliance**: Full CSI specification compliance with staging/unstaging support
- **Volume Expansion**: Online volume expansion for both backends

### LUKS Local Storage Features
- **Local Filesystem**: Uses local filesystem for storage (similar to local-path provisioner)
- **LUKS Encryption**: Automatically encrypts volumes using LUKS (Linux Unified Key Setup)
- **Filesystem Support**: Supports ext4, ext3, and xfs filesystems
- **Direct Mount**: Fast, direct access to encrypted volumes

### S3 Backend Features
- **S3-Compatible Storage**: Works with AWS S3, MinIO, OVH, and other S3-compatible storage
- **rclone Integration**: Uses rclone for transparent S3 access via FUSE mount
- **Client-Side Encryption**: Data encrypted before upload using rclone crypt
- **Encrypted VFS Caching**: Configurable local encrypted caching (LUKS) for performance optimization

## Architecture

The driver consists of:

1. **Controller Service**: Handles volume provisioning and deprovisioning
2. **Node Service**: Handles encryption operations, formatting, mounting/unmounting
3. **Identity Service**: Provides driver metadata and capabilities

## Storage Backend Options

### Option 1: LUKS Local Storage (Default)

Direct local storage with LUKS encryption:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: lukscryptwalker-local
provisioner: lukscryptwalker.csi.k8s.io
parameters:
  # Local storage configuration
  local-path: "/opt/local-path-provisioner"  # optional
  fsType: "ext4"  # optional, default: ext4

  # Encryption secret reference (required)
  csi.storage.k8s.io/node-stage-secret-name: "luks-secret"
  csi.storage.k8s.io/node-stage-secret-namespace: "kube-system"
  passphraseKey: "passphrase"  # optional, default: "passphrase"

  # Permission management (optional)
  fsGroup: "26"  # manual override for specific applications
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
```

### Option 2: S3 Encrypted Storage

S3-compatible storage with client-side encryption:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: lukscryptwalker-s3
provisioner: lukscryptwalker.csi.k8s.io
parameters:
  # Storage backend selection
  storage-backend: "s3"

  # S3 configuration
  s3-bucket: "my-encrypted-volumes"
  s3-region: "us-east-1"
  s3-endpoint: "https://s3.amazonaws.com"  # optional
  s3-force-path-style: "false"  # optional, set to true for MinIO
  s3-path-prefix: "my-app/data"  # optional custom path

  # S3 credentials secret reference
  s3-access-key-id-secret-name: "s3-credentials"
  s3-access-key-id-secret-namespace: "kube-system"

  # Encryption secret reference (required)
  csi.storage.k8s.io/node-stage-secret-name: "luks-secret"
  csi.storage.k8s.io/node-stage-secret-namespace: "kube-system"
  passphraseKey: "passphrase"

  # rclone VFS cache configuration (optional)
  rclone-vfs-cache-mode: "full"  # off, minimal, writes, full
  rclone-vfs-cache-max-age: "1h"
  rclone-vfs-cache-max-size: "10G"
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

## Prerequisites

- Kubernetes 1.20+
- `cryptsetup` utility available on all nodes
- Privileged containers support (for LUKS operations)

## Installation

### Option 1: Helm Installation (Recommended)

1. **Add the Helm repository:**
   ```bash
   helm repo add lukscryptwalker-csi https://algonomia.github.io/lukscryptwalker-csi/
   helm repo update
   ```

2. **Install with Helm:**
   ```bash
   # Install with default configuration
   helm install my-lukscryptwalker lukscryptwalker-csi/lukscryptwalker-csi \
     --namespace kube-system \
     --create-namespace

   # Or with custom values
   helm install my-lukscryptwalker lukscryptwalker-csi/lukscryptwalker-csi \
     --namespace kube-system \
     --create-namespace \
     --values my-values.yaml
   ```

3. **Customize installation:**
   ```yaml
   # my-values.yaml
   image:
     repository: my-registry/lukscryptwalker-csi
     tag: "v1.0.3"
   
   storage:
     localPath: "/mnt/encrypted-volumes"
   
   # Create multiple StorageClasses for different use cases
   storageClasses:
     - name: my-encrypted-storage
       isDefault: true
       fsGroup: 26  # PostgreSQL
       secret:
         name: luks-secret
         namespace: kube-system
         passphraseKey: "passphrase"
     
     - name: mysql-encrypted
       fsGroup: 999  # MySQL
       localPath: "/mnt/mysql-volumes"
       secret:
         name: luks-secret
         namespace: kube-system
         passphraseKey: "passphrase"
   ```

### Option 2: Direct Kubernetes Manifests

1. **Build the driver:**
   ```bash
   make docker-build
   ```

2. **Deploy to Kubernetes:**
   ```bash
   kubectl apply -f deploy/
   ```

3. **Create a secret with your passphrase:**
   ```bash
   kubectl create secret generic luks-secret \
     --from-literal=passphrase=your-secure-passphrase \
     -n kube-system
   ```

## Usage

1. **Create a StorageClass:**
   ```yaml
   apiVersion: storage.k8s.io/v1
   kind: StorageClass
   metadata:
     name: lukscryptwalker-local
   provisioner: lukscryptwalker.csi.k8s.io
   parameters:
     local-path: "/opt/local-path-provisioner"
     csi.storage.k8s.io/node-stage-secret-name: "luks-secret"
     csi.storage.k8s.io/node-stage-secret-namespace: "kube-system"
     passphraseKey: "passphrase"
   reclaimPolicy: Delete
   volumeBindingMode: WaitForFirstConsumer
   allowVolumeExpansion: true
   ```

2. **Create a PVC:**
   ```yaml
   apiVersion: v1
   kind: PersistentVolumeClaim
   metadata:
     name: encrypted-pvc
   spec:
     accessModes:
       - ReadWriteOnce
     resources:
       requests:
         storage: 1Gi
     storageClassName: lukscryptwalker-local
   ```

3. **Use in a Pod:**
   ```yaml
   apiVersion: v1
   kind: Pod
   metadata:
     name: test-pod
   spec:
     containers:
     - name: app
       image: nginx
       volumeMounts:
       - name: encrypted-storage
         mountPath: /data
     volumes:
     - name: encrypted-storage
       persistentVolumeClaim:
         claimName: encrypted-pvc
   ```

4. **Expand a volume:**
   ```bash
   # Edit the PVC to increase the size
   kubectl patch pvc encrypted-pvc -p '{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}'
   
   # Or edit directly
   kubectl edit pvc encrypted-pvc
   ```

   The expansion process includes:
   - Controller expands the backing file
   - Node service resizes the LUKS device
   - Node service resizes the filesystem (ext4/xfs)
   - Changes are reflected in the pod automatically

5. **Using custom passphrase key names:**
   ```bash
   # Create secret with custom key name
   kubectl create secret generic my-luks-secret \
     --from-literal=encryption-key=my-secure-passphrase \
     -n kube-system
   ```

   ```yaml
   # StorageClass with custom passphrase key
   apiVersion: storage.k8s.io/v1
   kind: StorageClass
   metadata:
     name: custom-lukscryptwalker
   provisioner: lukscryptwalker.csi.k8s.io
   parameters:
     csi.storage.k8s.io/node-stage-secret-name: "my-luks-secret"
     csi.storage.k8s.io/node-stage-secret-namespace: "kube-system"
     passphraseKey: "encryption-key"  # Custom key name
   reclaimPolicy: Delete
   volumeBindingMode: WaitForFirstConsumer
   allowVolumeExpansion: true
   ```

## File System Permissions

The driver supports flexible file system permission management:

### Automatic fsGroup Detection (Recommended)
By default, the driver automatically detects the `fsGroup` from requesting pods and sets appropriate permissions:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: postgres-pod
spec:
  securityContext:
    fsGroup: 26  # PostgreSQL group
  containers:
  - name: postgres
    image: postgres:15
    volumeMounts:
    - name: data
      mountPath: /var/lib/postgresql/data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: postgres-pvc
```

The driver will automatically:
- Create directories with `0775` permissions
- Set group ownership to `26` (PostgreSQL group)
- Allow both the container user and group to write

### Manual fsGroup Override
For scenarios where specific StorageClasses should always use a particular group, configure it directly in the StorageClass:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: postgres-encrypted
provisioner: lukscryptwalker.csi.k8s.io
parameters:
  fsGroup: "26"  # Manual override for PostgreSQL
  csi.storage.k8s.io/node-stage-secret-name: "luks-secret"
  csi.storage.k8s.io/node-stage-secret-namespace: "kube-system"
```

### Permission Modes
- **No fsGroup**: `0755` permissions (owner only) - uses auto-detection from pod
- **With fsGroup**: `0775` permissions + group ownership - uses specified fsGroup from StorageClass
- **StorageClass fsGroup takes precedence** over automatic pod detection

## S3 Backend with rclone Mount

The driver supports S3 and S3-compatible storage backends using rclone's FUSE mount with built-in encryption. Data is encrypted client-side using rclone's crypt backend before being stored in S3.

### Key Features

- **rclone Crypt Encryption**: NaCl SecretBox (XSalsa20 + Poly1305) encryption for all data
- **FUSE Mount**: Transparent S3 access via rclone mount - files appear as local filesystem
- **Encrypted VFS Cache**: Local cache is LUKS-encrypted for additional security
- **S3-Compatible**: Works with AWS S3, MinIO, OVH, and other S3-compatible storage
- **Custom Path Prefix**: Configure custom S3 paths via `s3-path-prefix` parameter
- **External Access**: Data can be accessed externally using rclone CLI with the same passphrase

### S3 StorageClass Configuration

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: lukscryptwalker-s3
provisioner: lukscryptwalker.csi.k8s.io
parameters:
  # Storage backend type
  storage-backend: "s3"

  # S3 Configuration
  s3-bucket: "my-encrypted-volumes"
  s3-region: "us-east-1"
  # s3-endpoint: "https://s3.example.com"  # Optional: for S3-compatible services
  # s3-force-path-style: "false"  # Set to true for MinIO
  # s3-path-prefix: "my-app/data"  # Optional: custom path in S3 (default: volumes/{volumeID}/files)

  # S3 Credentials (from secret)
  s3-access-key-id-secret-name: "s3-credentials"
  s3-access-key-id-secret-namespace: "kube-system"

  # Provisioner secrets for DeleteVolume (ReclaimPolicy: Delete)
  csi.storage.k8s.io/provisioner-secret-name: "s3-credentials"
  csi.storage.k8s.io/provisioner-secret-namespace: "kube-system"

  # LUKS/Encryption passphrase (used for rclone crypt)
  csi.storage.k8s.io/node-stage-secret-name: "luks-secret"
  csi.storage.k8s.io/node-stage-secret-namespace: "kube-system"
  passphraseKey: "passphrase"

  # VFS Cache Configuration (optional)
  # rclone-vfs-cache-mode: "full"  # off, minimal, writes, full
  # rclone-vfs-cache-max-age: "1h"
  # rclone-vfs-cache-max-size: "10G"
  # rclone-vfs-write-back: "5s"

reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

### S3 Credentials Secret

```bash
# Create S3 credentials secret (includes bucket/region for DeleteVolume)
kubectl create secret generic s3-credentials \
  --from-literal=s3-access-key-id=YOUR_ACCESS_KEY \
  --from-literal=s3-secret-access-key=YOUR_SECRET_KEY \
  --from-literal=s3-bucket=my-encrypted-volumes \
  --from-literal=s3-region=us-east-1 \
  -n kube-system
```

### Helm Configuration for S3

```yaml
# values.yaml
s3StorageClasses:
  - name: lukscryptwalker-s3
    isDefault: false
    reclaimPolicy: Delete
    allowVolumeExpansion: true
    volumeBindingMode: WaitForFirstConsumer

    # Encryption passphrase (shared with LUKS secret)
    secret:
      name: luks-secret
      namespace: kube-system
      passphraseKey: "passphrase"

    # S3 Configuration
    s3:
      bucket: "my-encrypted-volumes"
      region: "us-east-1"
      # endpoint: "https://s3.example.com"  # For S3-compatible services
      # forcePathStyle: false  # Set to true for MinIO
      # pathPrefix: "my-app/data"  # Custom S3 path (also used as encryption salt)

      # S3 Credentials Secret
      # WARNING: Setting credentials in values.yaml is not secure for production!
      credentialsSecret:
        name: s3-credentials
        namespace: kube-system
        create: true  # Set to false for production
        accessKeyId: ""  # WARNING: Not secure - use external secret management
        secretAccessKey: ""  # WARNING: Not secure - use external secret management
```

### OVH S3 Configuration Example

```yaml
s3StorageClasses:
  - name: lukscryptwalker-ovh
    isDefault: false

    secret:
      name: luks-secret
      namespace: kube-system
      passphraseKey: "passphrase"

    s3:
      bucket: "encrypted-volumes"
      region: "gra"  # GRA, SBG, DE, UK, etc.
      endpoint: "https://s3.gra.cloud.ovh.net"
      forcePathStyle: false
      pathPrefix: "my-app/data"  # Custom path - also used as encryption salt

      credentialsSecret:
        name: ovh-s3-credentials
        namespace: kube-system
```

### How rclone Mount Works

1. **Volume Mount**: rclone mounts S3 as a FUSE filesystem at the staging path
2. **Encryption Layer**: rclone crypt wraps S3 with NaCl SecretBox encryption
3. **VFS Cache**: Local cache (LUKS-encrypted) provides read/write buffering
4. **Transparent Access**: Applications see a normal filesystem, all I/O encrypted automatically

### S3 Directory Structure

```
s3://bucket/
├── volumes/
│   └── {volumeID}/
│       └── files/
│           └── {encrypted-filenames}    # Encrypted content + filenames
```

Or with custom `pathPrefix`:
```
s3://bucket/
└── {pathPrefix}/
    └── {encrypted-filenames}            # Encrypted content + filenames
```

### VFS Cache Modes

| Mode | Description |
|------|-------------|
| `off` | No caching, all I/O goes directly to S3 |
| `minimal` | Minimal caching for open files only |
| `writes` | Cache writes, direct reads |
| `full` | Full read/write caching (recommended) |

### External Access with rclone CLI

Data stored in S3 can be accessed externally using rclone:

```bash
# Create rclone config for external access
rclone config create myremote crypt \
  remote=':s3,provider=Other,access_key_id=XXX,secret_access_key=YYY,region=us-east-1,endpoint=https://s3.example.com:my-bucket/my-app/data' \
  password=$(rclone obscure "your-passphrase") \
  password2=$(rclone obscure "your-passphrase-my-app/data") \
  filename_encryption=standard \
  directory_name_encryption=true

# List files
rclone ls myremote:

# Copy files
rclone copy myremote:path/to/file ./local/
```

**Password derivation:**
- `password` = LUKS passphrase
- `password2` = `passphrase-{pathPrefix}` (if pathPrefix is set) or `passphrase-{volumeID}` (default)

### Security Features

- **rclone Crypt**: NaCl SecretBox encryption (XSalsa20 + Poly1305)
- **Filename Encryption**: File and directory names are encrypted
- **Encrypted Local Cache**: VFS cache is stored in a LUKS-encrypted container
- **Key Derivation**: Passwords are obscured using rclone's standard method
- **No Plaintext in S3**: Only encrypted content and filenames stored
- **Access Control**: Standard S3 IAM and bucket policies apply

## Security Considerations

### Secret Management

⚠️ **IMPORTANT**: The Helm chart provides convenience options to auto-create secrets, but this is **NOT SECURE for production environments**.

#### Development/Testing
- Use `create: true` in values.yaml for quick setup and testing
- Credentials are stored in plain text in your values.yaml file
- Suitable for development and proof-of-concept deployments

#### Production Deployment
- **Always set `create: false`** for both LUKS and S3 credentials
- Create secrets manually using external secret management tools:
  - HashiCorp Vault
  - AWS Secrets Manager
  - Azure Key Vault
  - External Secrets Operator
  - Sealed Secrets

```bash
# Production: Create secrets manually
kubectl create secret generic luks-secret \
  --from-literal=passphrase=your-secure-passphrase \
  -n kube-system

kubectl create secret generic s3-credentials \
  --from-literal=s3-access-key-id=YOUR_ACCESS_KEY \
  --from-literal=s3-secret-access-key=YOUR_SECRET_KEY \
  -n kube-system
```

### Additional Security Best Practices

- **Passphrase Management**: Store passphrases in Kubernetes secrets with appropriate RBAC
- **Node Access**: The driver requires privileged access on nodes for LUKS operations
- **Data at Rest**: All data is encrypted using LUKS with AES-256 encryption by default
- **Network Security**: Use TLS/SSL endpoints for S3-compatible storage services
- **Access Control**: Apply proper IAM policies and bucket policies for S3 access

## Planned Features

### Security Enhancements
- **Key Rotation**: Automated key rotation policies for production environments (planned)

## Development

1. **Setup development environment:**
   ```bash
   make dev-setup
   ```

2. **Run tests:**
   ```bash
   make test
   ```

3. **Deploy to kind cluster:**
   ```bash
   make kind-deploy
   ```

4. **Clean up:**
   ```bash
   make kind-clean
   ```

## Troubleshooting

### Check driver logs:
```bash
kubectl logs -n kube-system -l app=lukscryptwalker-csi-node
kubectl logs -n kube-system -l app=lukscryptwalker-csi-controller
```

### Check LUKS devices:
```bash
# On the node
sudo cryptsetup status
ls -la /dev/mapper/
```

### Common Issues:

1. **Permission denied errors**: Ensure the driver has privileged access
2. **cryptsetup command not found**: Install cryptsetup on all nodes
3. **Secret not found**: Verify the secret exists in the correct namespace
4. **Mount failures**: Check filesystem support and device permissions

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Run tests and linting
5. Submit a pull request

## License

This project is licensed under the Apache License 2.0.
