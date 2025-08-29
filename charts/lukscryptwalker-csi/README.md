# LUKS CSI Driver Helm Chart

This Helm chart deploys the LUKS CSI Driver on a Kubernetes cluster using the Helm package manager.

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

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Container image repository | `lukscryptwalker-csi` |
| `image.tag` | Container image tag | `latest` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `csiDriver.name` | CSI driver name | `lukscrypt.csi.k8s.io` |
| `controller.replicas` | Controller replicas | `1` |
| `storage.localPath` | Local storage path | `/opt/local-path-provisioner` |
| `storageClass.create` | Create StorageClass | `true` |
| `storageClass.name` | StorageClass name | `lukscrypt-local` |
| `storageClass.isDefault` | Set as default StorageClass | `false` |
| `storageClass.allowVolumeExpansion` | Allow volume expansion | `true` |
| `storageClass.secret.create` | Auto-create LUKS secret | `true` |
| `storageClass.secret.name` | LUKS secret name | `luks-secret` |
| `storageClass.secret.namespace` | LUKS secret namespace | `kube-system` |
| `rbac.create` | Create RBAC resources | `true` |
| `logging.level` | Logging verbosity level | `4` |

## Example Values

```yaml
# Custom image
image:
  repository: my-registry/lukscryptwalker-csi
  tag: "v1.0.0"
  pullPolicy: IfNotPresent

# Custom StorageClass
storageClass:
  name: encrypted-ssd
  isDefault: true
  
# Custom storage location
storage:
  localPath: "/mnt/encrypted-volumes"

# Custom secret (provide your own passphrase)
storageClass:
  secret:
    create: false
    name: my-luks-secret
    namespace: default

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
2. **Privileged Access**: Node pods run with privileged access for LUKS operations
3. **RBAC**: Proper RBAC rules are created for controller and node components

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