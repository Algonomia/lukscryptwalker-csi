# LUKSCryptWalker - LUKS CSI Driver Helm Chart

This Helm chart deploys the LUKSCryptWalker, a LUKS CSI Driver on a Kubernetes cluster using the Helm package manager.

## Prerequisites

- Kubernetes 1.20+
- Helm 3.0+
- `cryptsetup` utility available on all nodes
- Privileged containers support (for LUKS operations)

## Installation

```bash
# Install from local chart
helm install lukscryptwalker-csi ./charts/lukscryptwalker-csi \
  --namespace kube-system \
  --create-namespace

# Install with custom values
helm install lukscryptwalker-csi ./charts/lukscryptwalker-csi \
  --namespace kube-system \
  --create-namespace \
  --values my-values.yaml
```

## Uninstallation

```bash
helm uninstall lukscryptwalker-csi --namespace kube-system
```

## Configuration

The following table lists the configurable parameters and their default values:

### General Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Container image repository | `lukscryptwalker-csi` |
| `image.tag` | Container image tag | `latest` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `csiDriver.name` | CSI driver name | `lukscryptwalker.csi.k8s.io` |
| `controller.replicas` | Controller replicas | `1` |
| `storage.defaultPath` | Local storage path | `/opt/local-path-provisioner` |
| `rbac.create` | Create RBAC resources | `true` |
| `logging.level` | Logging verbosity level | `4` |

### Local StorageClass Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `storageClasses[].name` | StorageClass name | `lukscryptwalker-local` |
| `storageClasses[].isDefault` | Set as default StorageClass | `false` |
| `storageClasses[].reclaimPolicy` | Reclaim policy | `Delete` |
| `storageClasses[].allowVolumeExpansion` | Allow volume expansion | `true` |
| `storageClasses[].fsGroup` | Manual fsGroup override | (auto-detect) |
| `storageClasses[].secret.name` | LUKS secret name | `luks-secret` |
| `storageClasses[].secret.namespace` | LUKS secret namespace | `kube-system` |

### S3 StorageClass Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `s3StorageClasses[].name` | StorageClass name | - |
| `s3StorageClasses[].s3.pathPrefix` | Custom S3 path (StorageClass parameter) | - |
| `s3StorageClasses[].s3.secret.name` | S3 credentials secret name | - |
| `s3StorageClasses[].s3.secret.namespace` | S3 credentials secret namespace | `kube-system` |
| `s3StorageClasses[].s3.secret.create` | Auto-create S3 credentials secret | `false` |
| `s3StorageClasses[].s3.secret.bucket` | S3 bucket name (in secret) | - |
| `s3StorageClasses[].s3.secret.region` | S3 region (in secret) | - |
| `s3StorageClasses[].s3.secret.endpoint` | S3 endpoint URL (in secret) | (AWS default) |
| `s3StorageClasses[].s3.secret.forcePathStyle` | Force path-style URLs (in secret) | `false` |

### VFS Cache Parameters (rclone mount)

| Parameter | Description | Default |
|-----------|-------------|---------|
| `s3StorageClasses[].vfsCache.mode` | Cache mode: off, minimal, writes, full | `full` |
| `s3StorageClasses[].vfsCache.maxAge` | Max time to keep files in cache | `1h` |
| `s3StorageClasses[].vfsCache.maxSize` | Max total size of cache | `1G` |
| `s3StorageClasses[].vfsCache.writeBack` | Time to wait before uploading modified files | `5s` |

## Example Values

### Local LUKS Storage

```yaml
# Custom image
image:
  repository: my-registry/lukscryptwalker-csi
  tag: "v1.0.0"
  pullPolicy: IfNotPresent

# Custom storage location
storage:
  defaultPath: "/mnt/encrypted-volumes"

# Local StorageClasses
storageClasses:
  - name: encrypted-local
    isDefault: true
    reclaimPolicy: Delete
    allowVolumeExpansion: true
    secret:
      name: luks-secret
      namespace: kube-system
      passphraseKey: "passphrase"
```

### S3 Storage with rclone

```yaml
# S3 StorageClasses (uses rclone mount with crypt encryption)
s3StorageClasses:
  - name: lukscryptwalker-s3
    reclaimPolicy: Delete
    allowVolumeExpansion: true
    volumeBindingMode: WaitForFirstConsumer

    # Encryption passphrase
    secret:
      name: luks-secret
      namespace: kube-system
      passphraseKey: "passphrase"

    # S3 Configuration
    s3:
      # pathPrefix: "my-app/data"  # Custom path (StorageClass parameter)

      secret:
        bucket: "my-encrypted-volumes"
        region: "us-east-1"
        # endpoint: "https://s3.example.com"  # For S3-compatible services
        name: s3-credentials
        namespace: kube-system
        create: false  # Create secret manually for production

    # VFS Cache Configuration
    vfsCache:
      mode: "full"      # off, minimal, writes, full
      maxAge: "1h"      # Max time to keep files in cache
      maxSize: "1G"     # Max total cache size
      writeBack: "5s"   # Time before uploading modified files

# Resource limits
controller:
  resources:
    limits:
      cpu: 500m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi

node:
  resources:
    limits:
      cpu: 500m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi
```

## Volume Expansion

The chart enables volume expansion by default (`allowVolumeExpansion: true`). To expand a volume:

```bash
# Patch the PVC to increase size
kubectl patch pvc my-pvc -p '{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}'
```

## Security Considerations

1. **LUKS Secrets**: The chart can auto-generate a random passphrase or you can provide your own
2. **S3 Encryption**: S3 data is encrypted using rclone crypt (NaCl SecretBox - XSalsa20 + Poly1305)
3. **Encrypted Cache**: VFS cache for S3 volumes is stored in a LUKS-encrypted container
4. **Privileged Access**: Node pods run with privileged access for LUKS operations
5. **RBAC**: Proper RBAC rules are created for controller and node components

## External Access to S3 Data

Data stored in S3 can be accessed externally using rclone CLI:

```bash
rclone config create myremote crypt \
  remote=':s3,provider=Other,access_key_id=XXX,secret_access_key=YYY,region=REGION,endpoint=ENDPOINT:BUCKET/PATH' \
  password=$(rclone obscure "YOUR_PASSPHRASE") \
  password2=$(rclone obscure "YOUR_PASSPHRASE-PATH") \
  filename_encryption=standard \
  directory_name_encryption=true

rclone ls myremote:
```

Note: `password2` uses `passphrase-{pathPrefix}` if pathPrefix is set in the StorageClass, otherwise `passphrase-{volumeID}`.

## Troubleshooting

### Check deployment status:
```bash
helm status lukscryptwalker-csi -n kube-system
```

### View logs:
```bash
kubectl logs -n kube-system -l app.kubernetes.io/name=lukscryptwalker-csi -c lukscryptwalker-csi
```

### Test with a PVC:
```bash
kubectl create -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: test-pvc
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
  storageClassName: lukscrypt-local
EOF
```

### Common Issues

1. **Image Pull Errors**: Ensure the image is available and image pull secrets are configured
2. **LUKS Commands Fail**: Verify `cryptsetup` is installed on all nodes
3. **Permission Denied**: Ensure the CSI driver has privileged access on nodes
4. **Secret Not Found**: Check that the LUKS secret exists and is in the correct namespace

## Development

### Lint the chart:
```bash
helm lint ./charts/lukscryptwalker-csi
```

### Template generation:
```bash
helm template lukscryptwalker-csi ./charts/lukscryptwalker-csi --namespace kube-system
```

### Package the chart:
```bash
helm package ./charts/lukscryptwalker-csi
```
