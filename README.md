# LUKSCryptWalker CSI Helm Repository

This repository contains Helm charts for the LUKSCryptWalker CSI driver.

## Usage

Add this Helm repository:

```bash
helm repo add lukscryptwalker-csi https://algonomia.github.io/lukscryptwalker-csi/
helm repo update
```

Install the chart:

```bash
helm install lukscryptwalker-csi lukscryptwalker-csi/lukscryptwalker-csi \
  --namespace kube-system \
  --create-namespace
```

## Charts

- **lukscryptwalker-csi**: LUKS CSI driver with encryption capabilities for Kubernetes

## Repository

Source code: https://github.com/Algonomia/lukscryptwalker-csi