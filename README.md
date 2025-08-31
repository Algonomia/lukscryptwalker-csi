# LUKSCryptWalker - LUKS CSI Driver

A Kubernetes CSI driver that provides LUKS encryption on top of local storage, similar to rancher.io/local-path but with built-in encryption capabilities.

## Features

- **LUKS Encryption**: Automatically encrypts volumes using LUKS (Linux Unified Key Setup)
- **Local Storage**: Uses local filesystem for storage (similar to local-path provisioner)
- **Secret Management**: Integrates with Kubernetes secrets for passphrase management
- **CSI Compliance**: Full CSI specification compliance with staging/unstaging support
- **Filesystem Support**: Supports ext4, ext3, and xfs filesystems

## Architecture

The driver consists of:

1. **Controller Service**: Handles volume provisioning and deprovisioning
2. **Node Service**: Handles LUKS operations, formatting, mounting/unmounting
3. **Identity Service**: Provides driver metadata and capabilities

## StorageClass Parameters

The driver supports the following StorageClass parameters:

```yaml
parameters:
  # Local path where volumes will be stored (optional)
  local-path: "/opt/local-path-provisioner"
  
  # LUKS encryption secret reference (required)
  csi.storage.k8s.io/node-stage-secret-name: "luks-secret"
  csi.storage.k8s.io/node-stage-secret-namespace: "kube-system"
  
  # Key name within the secret that contains the passphrase (optional, default: "passphrase")
  passphraseKey: "passphrase"
  
  # Filesystem type (optional, default: ext4)
  fsType: "ext4"
  
  # Manual fsGroup override (optional)
  # If set, this fsGroup will be used for this StorageClass instead of auto-detecting from pods
  fsGroup: "26"  # Example: Use PostgreSQL group ID
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

## Security Considerations

- **Passphrase Management**: Store passphrases in Kubernetes secrets with appropriate RBAC
- **Node Access**: The driver requires privileged access on nodes for LUKS operations
- **Data at Rest**: All data is encrypted using LUKS with AES-256 encryption by default
- **Key Rotation**: Consider implementing key rotation policies for production use

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
