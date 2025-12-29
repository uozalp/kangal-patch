<div align="center"><img src="assets/kangal-patch-logo.png" alt="KangalPatch Logo" width="250"/></div>

## Purpose

KangalPatch automates rolling upgrades of Talos Linux nodes in Kubernetes clusters. The operator handles node draining, OS updates, reboots, and readiness checks while respecting PodDisruptionBudgets and failure thresholds.

Key features:
- Controlled concurrent node upgrades
- Automatic workload draining with PDB enforcement
- Configurable failure budgets with automatic halt on threshold breach
- Pause and resume support for manual intervention
- Real-time upgrade status tracking

## How it works

The operator watches PatchPlan resources. When you create one, it:
1. Selects nodes based on labels and role
2. For each node: drain → upgrade → reboot → verify
3. Respects your concurrency, timing, and failure settings

If failures exceed your threshold, it stops automatically.

## Quick Start

### Prerequisites

- Kubernetes cluster running Talos Linux
- `kubectl` configured to access your cluster
- Helm 3.x (for Helm installation)

### Installation via Helm

```bash
# Install KangalPatch
helm install kangal-patch oci://ghcr.io/uozalp/helm/kangal-patch \
  --version 0.1.0 \
  --namespace kangal-patch \
  --create-namespace
```

### Installation via kubectl

```bash
# Install CRDs
kubectl apply -k config/crd

# Install RBAC and operator
kubectl apply -k config/manager
```

## Usage

### 1. Create Talos Credentials Secret

First, create a secret containing your Talos API credentials:

```bash
# Extract credentials from your talosconfig (typically ~/.talos/config)

# Encode credentials to base64
CA_CERT=$(base64 -w0 < /path/to/ca.crt)
CLIENT_CERT=$(base64 -w0 < /path/to/client.crt)
CLIENT_KEY=$(base64 -w0 < /path/to/client.key)

# Create the secret with base64-encoded values
kubectl create secret generic talos-credentials \
  --namespace kangal-patch \
  --from-literal=ca.crt="$CA_CERT" \
  --from-literal=tls.crt="$CLIENT_CERT" \
  --from-literal=tls.key="$CLIENT_KEY"
```

### 2. Create a PatchPlan

Create a `PatchPlan` custom resource to define your upgrade:

```yaml
apiVersion: kangalpatch.ozalp.dk/v1alpha1
kind: PatchPlan
metadata:
  name: talos-upgrade-v1.11.6
spec:
  targetVersion: "ghcr.io/siderolabs/installer:v1.11.6"
  
  # Patch workers first, then control plane
  patchWorkers: true
  patchControlPlane: true
  controlPlaneFirst: false
  
  # Batch configuration
  maxConcurrency: 1
  
  # Timing
  delayBetweenNodes: 300s
  
  # Safety
  respectPDBs: true
  drainTimeout: 5m
  rebootTimeout: 10m
  maxFailures: 1
  
  # Talos API
  talosConfig:
    endpoints:
      - 10.0.0.10:50000
    secretRef:
      name: talos-credentials
      namespace: kangal-patch
```

Apply the PatchPlan:

```bash
kubectl apply -f patchplan.yaml
```

### 3. Monitor Progress

Watch the upgrade progress:

```console
# Watch status in real-time
$ kubectl get patchplan -w

NAME             PHASE       TARGET    TOTAL   COMPLETED   FAILED   AGE
simple-upgrade   Completed   v1.11.6   6       6           0        79m

# Check individual node status
$ kubectl get patchplan simple-upgrade -o jsonpath='{.status}' | jq
{
  "completedNodes": 6,
  "completionTime": "2025-12-28T20:54:03Z",
  "failedNodes": 0,
  "lastNodeScheduledAt": "2025-12-28T20:54:03Z",
  "message": "all nodes processed",
  "phase": "Completed",
  "startTime": "2025-12-28T19:32:29Z",
  "targetVersion": "v1.11.6",
  "totalNodes": 6
}
```

### 4. Pause/Resume

Pause an ongoing upgrade:

```bash
kubectl patch patchplan simple-upgrade --type merge -p '{"spec":{"paused":true}}'
```

Resume:

```bash
kubectl patch patchplan simple-upgrade --type merge -p '{"spec":{"paused":false}}'
```

## Configuration Reference

### PatchPlan Spec

| Field | Type | Description | Default |
|-------|------|-------------|---------|
| `targetVersion` | string | Target Talos version | Required |
| `nodeSelector` | map | Label selector for nodes | `{}` |
| `maxConcurrency` | int | Max nodes to patch concurrently | `1` |
| `maxFailures` | int | Max allowed failures before stopping | `0` |
| `delayBetweenNodes` | duration | Delay between nodes | `5m` |
| `respectPDBs` | bool | Respect PodDisruptionBudgets | `true` |
| `drainTimeout` | duration | Max time for node drain | `5m` |
| `rebootTimeout` | duration | Max time for reboot | `10m` |
| `patchControlPlane` | bool | Patch control plane nodes | `true` |
| `patchWorkers` | bool | Patch worker nodes | `true` |
| `controlPlaneFirst` | bool | Patch control plane first | `false` |
| `paused` | bool | Pause operation | `false` |

## Development

### Building from Source

```bash
# Build the binary
make build

# Run tests
make test

# Build Docker image
make docker-build IMG=ghcr.io/uozalp/kangal-patch:dev

# Generate manifests
make manifests

# Generate code
make generate
```

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

## Status

This is an early version of KangalPatch. Use with caution in production environments and always test upgrades in a staging environment first.
