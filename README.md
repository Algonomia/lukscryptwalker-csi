# LUKS CSI Driver

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
  
  # Filesystem type (optional, default: ext4)
  fsType: "ext4"
```

## Prerequisites

- Kubernetes 1.20+
- `cryptsetup` utility available on all nodes
- Privileged containers support (for LUKS operations)

## Installation

### Option 1: Helm Installation (Recommended)

1. **Add the Helm repository:**
   ```bash
   # If you have a Helm repository, add it here
   # helm repo add lukscryptwalker-csi https://your-repo-url
   ```

2. **Install with Helm:**
   ```bash
   # Install from local chart
   helm install lukscryptwalker-csi ./charts/lukscryptwalker-csi \
     --namespace kube-system \
     --create-namespace

   # Or with custom values
   helm install lukscryptwalker-csi ./charts/lukscryptwalker-csi \
     --namespace kube-system \
     --create-namespace \
     --values my-values.yaml
   ```

3. **Customize installation:**
   ```yaml
   # my-values.yaml
   image:
     repository: my-registry/lukscryptwalker-csi
     tag: "v1.0.0"
   
   storageClass:
     name: my-encrypted-storage
     isDefault: true
   
   storage:
     localPath: "/mnt/encrypted-volumes"
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
     name: lukscrypt-local
   provisioner: lukscrypt.csi.k8s.io
   parameters:
     local-path: "/opt/local-path-provisioner"
     csi.storage.k8s.io/node-stage-secret-name: "luks-secret"
     csi.storage.k8s.io/node-stage-secret-namespace: "kube-system"
   reclaimPolicy: Delete
   volumeBindingMode: WaitForFirstConsumer
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
     storageClassName: lukscrypt-local
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