# LUKSCryptWalker — Encrypted CSI Driver

A Kubernetes CSI driver for **encrypted persistent storage**, with two backends:

- **LUKS local** — local disk encrypted with LUKS (AES-256-XTS). Fast, direct.
- **S3** — any S3-compatible bucket, encrypted client-side with rclone crypt (NaCl SecretBox) and exposed as a normal filesystem via a FUSE mount with a local, LUKS-encrypted cache.

Both backends support dynamic provisioning, online volume expansion, automatic `fsGroup` permissions, and Kubernetes-secret-based passphrases.

Driver name: `lukscryptwalker.csi.k8s.io`

---

## How it works

The driver runs a **controller** (provisioning/deprovisioning) and a **node** DaemonSet (encryption, formatting, mount/unmount). On the node:

- **LUKS local**: a backing file is `losetup` + `cryptsetup`-opened, formatted (ext4/ext3/xfs), and mounted directly.
- **S3**: rclone mounts the bucket as FUSE; a crypt layer encrypts contents and filenames before upload; a local LUKS-encrypted VFS cache buffers reads/writes. Nothing is stored in S3 in plaintext.

---

## Prerequisites

- Kubernetes 1.20+
- `cryptsetup` available on every node
- Privileged containers allowed (LUKS needs it)

---

## Install (Helm)

```bash
helm repo add lukscryptwalker-csi https://algonomia.github.io/lukscryptwalker-csi/
helm repo update

helm install lukscryptwalker lukscryptwalker-csi/lukscryptwalker-csi \
  --namespace kube-system --create-namespace \
  --values my-values.yaml
```

For production, create secrets yourself and set `create: false` (see [Security](#security)).

---

## Quick start

```bash
# 1. Passphrase secret (used for LUKS / rclone crypt)
kubectl create secret generic luks-secret \
  --from-literal=passphrase=your-secure-passphrase -n kube-system
```

```yaml
# 2. StorageClass (LUKS local)
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: lukscryptwalker-local
provisioner: lukscryptwalker.csi.k8s.io
parameters:
  csi.storage.k8s.io/node-stage-secret-name: luks-secret
  csi.storage.k8s.io/node-stage-secret-namespace: kube-system
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
---
# 3. PVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: encrypted-pvc
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: lukscryptwalker-local
  resources:
    requests:
      storage: 1Gi
```

Mount `encrypted-pvc` in a Pod as usual. Expand by patching the PVC (`spec.resources.requests.storage`); the controller grows the backing file and the node resizes the LUKS device and filesystem automatically.

---

## LUKS local backend

StorageClass `parameters`:

| Parameter | Description | Default |
|---|---|---|
| `local-path` | Host directory for backing files | `/opt/local-path-provisioner` |
| `fsType` | `ext4`, `ext3`, or `xfs` | `ext4` |
| `csi.storage.k8s.io/node-stage-secret-name` / `-namespace` | Passphrase secret (required) | — |
| `passphraseKey` | Key within the secret | `passphrase` |
| `fsGroup` | Force a group owner (see [Permissions](#permissions)) | auto-detect |

---

## S3 backend

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: lukscryptwalker-s3
provisioner: lukscryptwalker.csi.k8s.io
parameters:
  storage-backend: "s3"

  # S3 credentials + bucket/region (from secret)
  s3-secret-name: s3-credentials
  s3-secret-namespace: kube-system

  # Needed for DeleteVolume when reclaimPolicy: Delete
  csi.storage.k8s.io/provisioner-secret-name: s3-credentials
  csi.storage.k8s.io/provisioner-secret-namespace: kube-system

  # Encryption passphrase (rclone crypt)
  csi.storage.k8s.io/node-stage-secret-name: luks-secret
  csi.storage.k8s.io/node-stage-secret-namespace: kube-system

  # Optional
  s3-path-prefix: "my-app/data"          # default: volumes/{volumeID}/files
  rclone-vfs-cache-mode: "full"
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

### Credentials secret

```bash
kubectl create secret generic s3-credentials \
  --from-literal=s3-access-key-id=YOUR_ACCESS_KEY \
  --from-literal=s3-secret-access-key=YOUR_SECRET_KEY \
  --from-literal=s3-bucket=my-encrypted-volumes \
  --from-literal=s3-region=us-east-1 \
  -n kube-system
# S3-compatible (MinIO/OVH): also add s3-endpoint and, for MinIO, s3-force-path-style=true
```

### Tuning parameters (optional)

| Parameter | Description | Default |
|---|---|---|
| `rclone-vfs-cache-mode` | `off` \| `minimal` \| `writes` \| `full` | `full` |
| `rclone-vfs-cache-max-age` | Max time to keep a file cached | `1h` |
| `rclone-vfs-cache-max-size` | Max total cache size | `2G` |
| `rclone-vfs-cache-poll-interval` | Stale-entry scan interval | `1m` |
| `rclone-vfs-write-back` | Delay before uploading changes | `5s` |
| `rclone-dir-cache-time` | How long directory listings are cached | `1h` |
| `rclone-attr-timeout` | How long file attributes (stat) are cached | `1h` |

The cache lives in a LUKS-encrypted volume on the node; a background process enforces the size limit and prunes empty directories.

`rclone-dir-cache-time` / `rclone-attr-timeout` control directory **metadata** caching. With filename encryption on, each uncached `stat`/`readdir` costs an S3 `ListObjects` + decrypt, so metadata-heavy workloads on large directories (e.g. pgbackrest WAL archives) can stall. The `1h` default avoids that; these are RWO/single-writer volumes, so a writer always sees its own changes immediately regardless. Lower it per StorageClass if a volume is modified from outside the cluster and needs fresher metadata. The default applies to existing volumes too — they pick it up on the next mount (pod restart), with no re-provisioning.

### Layout in S3

```
s3://bucket/volumes/{volumeID}/files/{encrypted-names}   # default
s3://bucket/{s3-path-prefix}/{encrypted-names}           # with s3-path-prefix
```

### External access

Data is readable outside Kubernetes with the same passphrase:

```bash
rclone config create myremote crypt \
  remote=':s3,provider=Other,access_key_id=XXX,secret_access_key=YYY,region=us-east-1,endpoint=https://s3.example.com:my-bucket/my-app/data' \
  password=$(rclone obscure "your-passphrase") \
  password2=$(rclone obscure "your-passphrase-my-app/data") \
  filename_encryption=standard directory_name_encryption=true

rclone ls myremote:
```

- `password` = the LUKS passphrase
- `password2` = `passphrase-{s3-path-prefix}` if set, else `passphrase-{volumeID}`

### Resilience (S3 mounts)

The node driver continuously reconciles S3 mounts. If it restarts (upgrade, OOM, crash), it re-mounts active S3 volumes in place from the persistent encrypted cache — no data is lost.

To let a workload recover **without being restarted**, mount the volume with `mountPropagation: HostToContainer`, so the re-mount propagates into the running container:

```yaml
volumeMounts:
- name: data
  mountPath: /data
  mountPropagation: HostToContainer
```

Consumers without propagation are restarted automatically so kubelet re-publishes them.

---

## Permissions

By default the driver detects `fsGroup` from the requesting pod, chowns the volume to that group, and applies mode `0750`:

```yaml
spec:
  securityContext:
    fsGroup: 26   # e.g. PostgreSQL
```

To pin a group regardless of the pod, set `fsGroup` in the StorageClass `parameters` (it takes precedence over pod detection).

---

## Security

⚠️ The Helm chart can auto-create secrets (`create: true`) for convenience — **do not use this in production** (credentials end up in plaintext in your values).

For production:

- Set `create: false` and manage secrets with Vault, External Secrets Operator, Sealed Secrets, a cloud secret manager, etc.
- Restrict the passphrase/credential secrets with RBAC.
- Use TLS endpoints and proper IAM/bucket policies for S3.

The node driver requires privileged access for LUKS. All data is encrypted at rest (LUKS for local; rclone crypt for S3, including filenames), and the S3 VFS cache is itself LUKS-encrypted.

---

## Development

```bash
make dev-setup     # set up the dev environment
make test          # run tests
make docker-build  # build the image
make kind-deploy   # deploy to a kind cluster
make kind-clean    # tear down
```

---

## Troubleshooting

```bash
# Driver logs
kubectl logs -n kube-system -l app=lukscryptwalker-csi-node
kubectl logs -n kube-system -l app=lukscryptwalker-csi-controller

# LUKS state on a node
sudo cryptsetup status && ls -la /dev/mapper/
```

| Symptom | Likely cause |
|---|---|
| `cryptsetup: not found` | Install `cryptsetup` on all nodes |
| Permission denied | Driver needs privileged access |
| Secret not found | Wrong secret name/namespace in the StorageClass |
| Mount failures | Check `fsType` support and `/dev` access |
| S3 pod sees I/O errors after a driver restart | Add `mountPropagation: HostToContainer` (see [Resilience](#resilience-s3-mounts)) |
| Slow `stat`/`ls` or app timeouts on a large S3 dir (e.g. pgbackrest WAL archive) | Metadata listing cost — raise `rclone-dir-cache-time` (default `1h`) and keep the directory pruned |
| PostgreSQL `FATAL: data directory ... has invalid permissions` | Volume mode too permissive — uses default `0750` now; ensure the StorageClass isn't overriding `fs-mode` to `0770`/`0775` |

---

## License

Apache License 2.0.
