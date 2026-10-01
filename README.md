<div align="center"><img src="assets/kangal-patch-logo.png" alt="KangalPatch Logo" width="250"/></div>

## Purpose

KangalPatch automates rolling upgrades of Talos Linux nodes in Kubernetes clusters. The operator handles node draining, OS updates, reboots, and readiness checks while respecting PodDisruptionBudgets and failure thresholds.

Key features:
- Controlled concurrent node upgrades
- Automatic workload draining with PDB enforcement
- Configurable failure budgets with automatic halt on threshold breach
- Maintenance window support with date exclusions
- Pause and resume support for manual intervention
- Real-time upgrade status tracking

## How it works

The operator watches PatchPlan resources. When you create one, it:
1. Selects the nodes named by `nodeSelector` and assigns each to the first matching group in `strategy.order`
2. For each node: drain → upgrade → reboot → verify
3. Respects the per-group concurrency, timing, and failure settings

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
  --version 0.1.2 \
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
  name: simple-upgrade
spec:
  target:
    talosVersion: v1.11.6
    source: ghcr
  
  # Control plane first, then workers (this is the default order)
  strategy:
    order: [controlPlane, workers]

  # Batch configuration: how many nodes of a group are patched at once
  groups:
    controlPlane:
      concurrency: 1
    workers:
      concurrency: 1
  
  # Timing
  delayBetweenNodes: 300s
  
  # Safety
  respectPDBs: true
  drainTimeout: 5m
  rebootTimeout: 10m
  failurePolicy:
    type: Halt
    maxFailures: 1
  
  # Talos API
  talosConfig:
    endpoints:
      - 10.0.0.10:50000
    secretRef:
      name: talos-credentials
      namespace: kangal-patch
```

**Example using Talos Factory images:**

The `target` specification uses individual fields to construct the factory image URL. The operator builds the full URL in the format:
```
factory.talos.dev/{installer}-installer[-secureboot]/{schematicID}:{talosVersion}
```

Field breakdown:
```yaml
target:
  talosVersion: v1.11.6               # The Talos version tag
  source: factory                     # Use factory.talos.dev (vs ghcr)
  installer: nocloud                  # The installer type (aws, azure, nocloud, etc.)
  schematicID: 95d432d6bb...          # Optional: omit to keep each node's current schematic
  secureBoot: true                    # Adds -secureboot suffix to installer
```

Full example:

```yaml
apiVersion: kangalpatch.ozalp.dk/v1alpha1
kind: PatchPlan
metadata:
  name: talos-upgrade-factory
spec:
  target:
    talosVersion: v1.12.1
    source: factory
    installer: aws
    schematicID: 376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba
    secureBoot: true
  
  groups:
    workers:
      concurrency: 2
  
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

### 3. Optional: Configure Maintenance Windows

Restrict patching to specific time windows and exclude certain dates:

```yaml
apiVersion: kangalpatch.ozalp.dk/v1alpha1
kind: PatchPlan
metadata:
  name: talos-upgrade-maintenance
spec:
  target:
    talosVersion: v1.11.6
    source: ghcr
  
  # ... other configuration ...
  
  # Maintenance windows
  maintenance:
    # Exclude specific dates (holidays, blackout periods)
    excludeDates:
      - "2026-12-24"
      - "2026-12-25"
      - "2026-12-26"
      - "2026-12-31"
      - "2027-01-01"
    
    # Define when patching is allowed (UTC)
    windows:
      # Monday and Friday early morning
      - days: ["Monday", "Friday"]  # Supports: "Monday", "Mon", "monday"
        startTime: "01:00"
        endTime: "05:00"
      
      # Wednesday night window
      - days: ["Wed"]
        startTime: "22:00"
        endTime: "02:00"  # Spans midnight
      
      # Every day window (omit days field or use ["Any"])
      - startTime: "03:00"
        endTime: "04:00"
```

**Notes on maintenance windows:**
- All times are in UTC
- Day names support full names ("Monday"), 3-letter abbreviations ("Mon"), case-insensitive
- Omit `days` field or use `["Any"]` to match all days
- Windows can span midnight (e.g., 22:00 to 02:00)
- Patching will be paused outside maintenance windows
- Exclude dates use YYYY-MM-DD format


### 4. Monitor Progress

Watch the upgrade progress:

```console
# Watch status in real-time
$ kubectl get patchplan -w

NAME             PHASE       TALOSTARGET   K8STARGET   TOTAL   COMPLETED   FAILED   AGE
simple-upgrade   Completed   v1.11.6                   6       6           0        79m

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
  "targetTalosVersion": "v1.11.6",
  "totalNodes": 6
}
```

### 5. Pause/Resume

Pause an ongoing upgrade:

```bash
kubectl patch patchplan simple-upgrade --type merge -p '{"spec":{"paused":true}}'
```

Resume:

```bash
kubectl patch patchplan simple-upgrade --type merge -p '{"spec":{"paused":false}}'
```

### 6. Cancel

Unlike pause, cancelling is permanent: the PatchPlan moves to the terminal `Cancelled` phase and
the controller stops scheduling new nodes. PatchJobs already in progress are not affected and
run to completion.

```bash
kubectl patch patchplan simple-upgrade --type merge -p '{"spec":{"cancelled":true}}'
```

Setting `cancelled` back to `false` resumes scheduling, same as pause/resume.

### 7. Upgrading Across Multiple Talos Versions

Talos doesn't reliably support jumping several minor versions in a single upgrade. A large
version skip (e.g. `v1.11.x` → `v1.14.x`) can silently fail: the upgrade succeeds and the node
reboots, but it boots back into the *old* partition with no error reported anywhere. The
`PatchJob` will just look stuck, waiting for a target version that never arrives.

If you're multiple minor versions behind, upgrade in stages rather than jumping straight to the
latest version. For example, going from `v1.11.x` to `v1.14.0`:

```bash
# Stage 1: v1.11.x -> v1.13.10
kubectl apply -f - <<EOF
apiVersion: kangalpatch.ozalp.dk/v1alpha1
kind: PatchPlan
metadata:
  name: upgrade-stage-1
spec:
  target:
    talosVersion: v1.13.10
    source: ghcr
  groups:
    workers: {concurrency: 1}
  talosConfig:
    endpoints: ["10.0.0.10:50000"]
    secretRef: {name: talos-credentials, namespace: kangal-patch}
EOF

# Wait for upgrade-stage-1 to reach phase: Completed, then:

# Stage 2: v1.13.10 -> v1.14.0
kubectl apply -f - <<EOF
apiVersion: kangalpatch.ozalp.dk/v1alpha1
kind: PatchPlan
metadata:
  name: upgrade-stage-2
spec:
  target:
    talosVersion: v1.14.0
    source: ghcr
  groups:
    workers: {concurrency: 1}
  talosConfig:
    endpoints: ["10.0.0.10:50000"]
    secretRef: {name: talos-credentials, namespace: kangal-patch}
EOF
```

**Spotting a stuck upgrade:** `kubectl get patchjob` shows a job stuck in the `Rebooting` phase
with `currentTalosVersion` unchanged from before the upgrade, even though `kubectl get nodes` reports
the node as `Ready` again. Confirm the actual installed version via the node's `OS-IMAGE` column
(`kubectl get nodes -o wide`) or `talosctl -n <node-ip> version`, then retry with an intermediate
version as shown above.

### 8. Kubernetes Version Compatibility

Not every Kubernetes version runs on every Talos version. Before setting `target.kubernetesVersion`,
check the official Talos support matrix for the Talos version your nodes are running:
**https://docs.siderolabs.com/talos/\<talos-version\>/getting-started/support-matrix**

As of this writing, the supported combinations are:

| Talos version | Supported Kubernetes versions |
|---|---|
| [1.14](https://docs.siderolabs.com/talos/v1.14/getting-started/support-matrix) | 1.37, 1.36, 1.35, 1.34, 1.33 |
| [1.13](https://docs.siderolabs.com/talos/v1.13/getting-started/support-matrix) | 1.36, 1.35, 1.34, 1.33, 1.32, 1.31 |
| [1.12](https://docs.siderolabs.com/talos/v1.12/getting-started/support-matrix) | 1.35, 1.34, 1.33, 1.32, 1.31, 1.30 |
| [1.11](https://docs.siderolabs.com/talos/v1.11/getting-started/support-matrix) | 1.34, 1.33, 1.32, 1.31, 1.30, 1.29 |
| [1.10](https://docs.siderolabs.com/talos/v1.10/getting-started/support-matrix) | 1.33, 1.32, 1.31, 1.30, 1.29, 1.28 |

This table goes stale with every new Talos/Kubernetes release, so always check the link above for
the current matrix rather than relying on the snapshot here. Before scheduling any node, the
preflight checks (see [Preflight Checks](#preflight-checks)) reject a `target.kubernetesVersion`
that's outside the range in a built-in copy of this table. Talos versions newer than the built-in
table are not checked.

### Auto-Update

Instead of naming a Talos version, a PatchPlan can follow the upstream
[siderolabs/talos releases](https://github.com/siderolabs/talos/releases). With
`target.autoUpdate.enabled: true` the PatchPlan is a template (phase `Watching`) that never patches
nodes itself. Every `checkInterval` it creates a child PatchPlan named `<template>-<version>` with
`target.talosVersion` set and the rest of the spec copied, and that child runs the normal flow
(preflight, `PatchJobs`). Each release therefore has its own PatchPlan and history.

```yaml
spec:
  target:
    autoUpdate:
      enabled: true
      allow: patch          # patch (default) | minor
      checkInterval: 30m    # default 1h, minimum 1m
      minReleaseAge: 72h    # default 0s
```

- The current version is the lowest Talos version across the selected nodes. `allow: patch` only
  moves within the current minor; `allow: minor` also steps to the next minor, never skipping one.
- Only stable GitHub releases are considered, and a release must be at least `minReleaseAge` old.
- No new child is created while an earlier child is not `Completed` (including `Failed`); delete or
  fix it to resume. Children are garbage-collected with the template.
- `talosVersion` and `kubernetesVersion` must be omitted on a template. Releases are read
  anonymously from the GitHub API.
- `status.autoUpdate` shows the last check, the current, latest and pending (too young) versions,
  and the last created plan.

```bash
kubectl get patchplan
```

### Preflight Checks

Before the first `PatchJob` of a `PatchPlan` is created, the controller runs these checks once
(phase `Preflighting`) and schedules nothing until all pass:

- the node selection resolves to at least one node, and `strategy.order` is valid and claims at least one of them
- when `target.kubernetesVersion` is set, `strategy.order` schedules every control plane node before any worker node (the order is never changed for you)
- no other `InProgress` or `Paused` PatchPlan targets any of the same nodes (auto-update templates are ignored)
- the Kubernetes API reports ready (`/readyz`)
- the Talos credentials work against the configured endpoints, and every selected node answers on the Talos API
- the Talos installer image exists in the registry for nodes that still need the upgrade
- the Talos/Kubernetes support matrix accepts the combination every node ends up with (the target
  versions, or the version a node keeps if only the other one changes); the current combination is reported

The resulting rollout plan (nodes and concurrency per group) is part of the `PreflightPassed` condition message.

On failure the plan moves to `Failed`, the `PreflightPassed` condition carries the reason and
message, and the checks are retried every minute, so fixing the cause (e.g. a Secret) recovers the
plan without recreating it. Checks are not repeated once `PatchJobs` exist.

```bash
kubectl get patchplan simple-upgrade -o jsonpath='{.status.conditions}' | jq
```

## Configuration Reference

### Groups, Strategy and Concurrency

`nodeSelector` defines the complete population of a plan. `groups` are label filters over that
population and never add nodes; they may overlap. `strategy.order` lists the groups from first to
last and resolves overlaps: **the first listed group a node matches schedules it, and a node is
patched at most once per plan**. `workers` (non-control-plane nodes) and `controlPlane` are built in
and need not be declared.

```yaml
spec:
  nodeSelector:
    matchLabels:
      environment: production
  groups:
    database:
      selector:
        matchLabels: {workload: database}
      concurrency: 1
    kafka:
      selector:
        matchLabels: {workload: kafka}
      concurrency: 1
    gpu:
      selector:
        matchLabels: {accelerator: nvidia}
      concurrency: 2
    workers:        # built in, declared only to set its concurrency
      concurrency: 3
  strategy:
    order: [database, kafka, gpu, workers, controlPlane]
  failurePolicy:
    type: Halt
    maxFailures: 1
  retention:
    history: 168h
```

- Groups run strictly in `strategy.order`: a group starts once every earlier group has finished.
  The order is used exactly as written; with `strategy.order` omitted it is `[controlPlane, workers]`.
- Selected nodes that no listed group claims are not patched. Omit `workers` (or `controlPlane`) to
  leave those nodes alone.
- `concurrency` is per group and is enforced with Leases. Every active `PatchJob` holds one Lease in
  the operator namespace, labelled `kangalpatch.ozalp.dk/patchplan` and `kangalpatch.ozalp.dk/group`,
  from creation until the job is `Completed` or `Failed`, whatever phase it is in. The live concurrency
  of a group is the number of its Leases:
  `kubectl -n kangal-patch get lease -l kangalpatch.ozalp.dk/patchplan=<plan>,kangalpatch.ozalp.dk/group=<group>`.
- `delayBetweenNodes` is the minimum time between two scheduling rounds of the plan; a round fills
  all free slots of the current group.
- `failurePolicy` is independent of concurrency and triggers once `maxFailures` (default `1`) nodes
  failed. `Halt` (default) marks the plan `Failed`. `Pause` sets `spec.paused: true` so you can
  inspect the failed `PatchJobs`, delete one to have its node retried, then set `paused: false` to
  resume; resuming accepts the failures seen so far (`status.failuresAcknowledged`) and the plan
  pauses again after `maxFailures` further failures. Running nodes are never interrupted.
- `retention.history` (default `168h`): after the plan completed, failed or was cancelled, its
  finished `PatchJobs` are removed once this time has passed (only when none is still running). The
  plan is frozen afterwards (`status.historyPurged`); create a new `PatchPlan` to roll out again.
- Status shows `totalNodes` (unique nodes in the rollout), `pendingNodes`, `runningNodes`,
  `completedNodes`, `failedNodes`, and a `status.groups` breakdown in schedule order.

### PatchPlan Spec

| Field | Type | Description | Default |
|-------|------|-------------|---------|
| `target` | object | Target Talos and/or Kubernetes version specification | Required |
| `nodeSelector` | LabelSelector | Complete population of nodes the plan may patch | all nodes |
| `groups` | map | Custom node groups: `selector` (label selector) and `concurrency` (min 1, default 1). `workers`/`controlPlane` are built in | `{}` |
| `strategy.order` | []string | Group evaluation and rollout order | `[controlPlane, workers]` |
| `failurePolicy.type` | string | `Halt` or `Pause` | `Halt` |
| `failurePolicy.maxFailures` | int | Failed nodes the policy reacts to | `1` |
| `retention.history` | duration | How long finished PatchJobs are kept | `168h` |
| `delayBetweenNodes` | duration | Minimum time between scheduling rounds | `5m` |
| `respectPDBs` | bool | Respect PodDisruptionBudgets | `true` |
| `drainTimeout` | duration | Max time for node drain | `5m` |
| `rebootTimeout` | duration | Max time for reboot | `10m` |
| `paused` | bool | Pause operation | `false` |
| `cancelled` | bool | Permanently cancel operation | `false` |
| `maintenance` | object | Maintenance window configuration | `nil` |

#### Target Spec

At least one of `talosVersion`/`kubernetesVersion` (or an enabled `autoUpdate`) must be set; both
can be set to upgrade both
in the same PatchPlan. Setting `kubernetesVersion` requires `strategy.order` to schedule every control
plane node before any worker node (checked in preflight), since a kubelet must never run newer than
the control plane it connects to.

| Field | Type | Description | Default |
|-------|------|-------------|---------|
| `talosVersion` | string | Talos OS version (e.g., v1.12.1) | - |
| `kubernetesVersion` | string | Kubernetes version (e.g., v1.32.4). Patches the kubelet and, on control plane nodes, the kube-apiserver/controller-manager/scheduler static pods and the cluster-wide kube-proxy DaemonSet. No drain/reboot required | - |
| `source` | string | Image source: "ghcr" or "factory" | `ghcr` |
| `installer` | string | Installer type (e.g., "aws", "nocloud"). Required when source=factory | - |
| `schematicID` | string | Talos factory schematic ID. If omitted with source=factory, each node's currently running schematic is used | - |
| `secureBoot` | bool | Enable secure boot. Only applicable when source=factory | `false` |
| `autoUpdate.enabled` | bool | Make the plan an auto-update template, see [Auto-Update](#auto-update) | - |
| `autoUpdate.allow` | string | Largest automatic change: `patch` or `minor` | `patch` |
| `autoUpdate.checkInterval` | duration | How often to check for releases (min 1m) | `1h` |
| `autoUpdate.minReleaseAge` | duration | Minimum age of a release before it is used | `0s` |

#### Maintenance Spec

| Field | Type | Description |
|-------|------|-------------|
| `excludeDates` | []string | List of dates (YYYY-MM-DD) to exclude from patching |
| `windows` | []MaintenanceWindow | List of time windows when patching is allowed |

#### MaintenanceWindow

| Field | Type | Description |
|-------|------|-------------|
| `days` | []string | Days of week (e.g., "Monday", "Mon"). Empty = all days |
| `startTime` | string | Start time in HH:MM format (UTC) |
| `endTime` | string | End time in HH:MM format (UTC) |
| `disabled` | bool | Temporarily disable this window |

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

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.