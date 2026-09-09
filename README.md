# Kube Workspaces Controller

![License](https://img.shields.io/github/license/kube-workspaces/controller)
![Go Version](https://img.shields.io/github/go-mod/go-version/kube-workspaces/controller)
![Release](https://img.shields.io/github/v/release/kube-workspaces/controller)
![Tests](https://img.shields.io/github/actions/workflow/status/kube-workspaces/controller/test.yml?label=tests)
![E2E Tests](https://img.shields.io/github/actions/workflow/status/kube-workspaces/controller/test-e2e.yml?label=e2e)
![Lint](https://img.shields.io/github/actions/workflow/status/kube-workspaces/controller/lint.yml?label=lint)
![Docker Image](https://img.shields.io/github/actions/workflow/status/kube-workspaces/controller/docker.yml?label=docker)

Kubernetes controller that manages the lifecycle of `Workspace` custom resources,
built with kubebuilder and controller-runtime.

## Overview

The controller watches for `Workspace` CRs and reconciles them into a workload
based on the workspace's `spec.type`:

- **`container`** (default) — a **StatefulSet** (replicas 0 or 1) running the
  workspace container.
- **`vm`** — a KubeVirt **`VirtualMachine`** (requires KubeVirt installed; the
  controller detects the KubeVirt CRDs and reports a `KubeVirtNotInstalled`
  status condition if they are missing). The main image is a containerDisk
  containing a bootable guest OS; `generateVirtualMachine` also attaches
  cloud-init user-data, injects the owner's SSH keys, pins the guest MAC, and
  emits `domain.devices.gpus[]` for GPU passthrough when requested.
- **`scratch`** — a plain **`Deployment`** (replicas 0 or 1).

Every type gets a **ClusterIP Service** exposing the workspace on port 80.

## CRDs

The controller owns the `kubeworkspaces.io` API group (`v1alpha1`):

| Kind | Scope |
|------|-------|
| `Workspace` | Namespaced |
| `Image` | Cluster |
| `User` | Cluster |
| `AuthConfig` | Cluster |
| `PlatformConfig` | Cluster |
| `PodDefault` | Namespaced |
| `SshKey` | Namespaced |

The `Workspace` spec wraps a full `corev1.PodSpec` under `spec.template.spec`,
mirroring the Kubeflow Notebook CRD pattern. This gives full flexibility for
container configuration including env vars, args, volume mounts, resource
limits, etc.

## Status

The controller reports:
- `readyReplicas` - Number of ready pods (0 or 1); for `vm` workspaces this only
  reports 1 when the KubeVirt `VirtualMachineInstance` is actually in the
  Running phase
- `containerState` - Running/waiting/terminated state of the first container
- `conditions` - Pod conditions mirrored to the workspace (plus
  `KubeVirtNotInstalled` when KubeVirt CRDs are absent)

## Start/Stop

- **Stop:** Add annotation `kubeworkspaces.io/stopped: "true"` (container/scratch
  scale replicas to 0; `vm` sets `spec.running: false`, tearing the VMI down)
- **Start:** Remove the annotation (restores replicas / `spec.running: true`)

The controller watches for annotation changes and reconciles the workload accordingly.

## Reconciliation Logic

The reconciler switches on `spec.type`:

1. `container` — ensure a StatefulSet exists (create or update); `scratch` —
   ensure a Deployment exists
2. `vm` — ensure a KubeVirt `VirtualMachine` exists (`generateVirtualMachine`):
   containerDisk (or DataVolume root when the Image declares
   `persistentRootDisk`), resources→`domain.resources`, ports→masquerade
   interfaces, pinned MAC, cloud-init user-data with owner SSH-key injection,
   GPU `domain.devices.gpus[]` from GPU resource limits, and
   `spec.running: !stopped`
3. Ensure the Service exists (type-aware selector)
4. Check for `kubeworkspaces.io/stopped` annotation -> set replicas / `running`
5. Update workspace status from pod/VMI state
6. Ensure default container port if not specified (default: 8080)

## Development

```bash
# Generate CRD manifests
make manifests

# Generate deepcopy code
make generate

# Install CRD to cluster
make install

# Run controller locally
make run
```

## Docker

```bash
docker build -t kube-workspaces-controller:latest -f Dockerfile .
```

## Sample Resources

```bash
kubectl apply --server-side -f config/samples/v1alpha1_workspace.yaml
kubectl apply --server-side -f config/samples/v1alpha1_workspace_desktop.yaml
```

Note: `--server-side` is required because the CRD is too large for client-side apply.

## RBAC

The controller requires a ClusterRole with permissions for:
- `workspaces.kubeworkspaces.io` - all verbs + status subresource
- `images`, `users`, `sshkeys`, `platformconfigs`, `authconfigs`, `poddefaults`
  - read/write as applicable
- `apps/statefulsets` + `apps/deployments` - create, get, list, watch, update, delete
- `core/services` - create, get, list, watch, update, delete
- `core/pods` - get, list, watch
- `core/pods/log` - get
- `core/events` - get, list, watch
- `kubevirt.io` `virtualmachines` / `virtualmachineinstances` - read/write (VM workspaces)
- `cdi.kubevirt.io` `datavolumes` - read/write (persistent VM root disks)
- `subresources.kubevirt.io` `virtualmachineinstances/console` - get (serial console bridge)

RBAC is declared with kubebuilder markers in `workspace_controller.go` and
regenerated with `make manifests` into `config/rbac/role.yaml`; the same rules
must be mirrored into the `deploy` repo's Helm + Kustomize RBAC and validated
with `compare-rbac.py`.

## Related Repositories

| Repository | Description |
|------------|-------------|
| [kube-workspaces/api](https://github.com/kube-workspaces/api) | REST API service |
| [kube-workspaces/proxy](https://github.com/kube-workspaces/proxy) | Workspace reverse proxy |
| [kube-workspaces/frontend](https://github.com/kube-workspaces/frontend) | Next.js web UI |
| [kube-workspaces/deploy](https://github.com/kube-workspaces/deploy) | Deployment manifests and documentation |

## License

Apache License 2.0
