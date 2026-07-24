# kube-workspaces-controller

Kubernetes controller that manages the lifecycle of `Workspace` custom resources.

## Overview

The controller watches for `Workspace` CRs and reconciles them into:
- A **StatefulSet** (replicas 0 or 1) running the workspace container
- A **ClusterIP Service** exposing the workspace on port 80

## CRD

- **Group:** `kubeworkspaces.io`
- **Version:** `v1alpha1`
- **Kind:** `Workspace`
- **Scope:** Namespaced

The spec wraps a full `corev1.PodSpec` under `spec.template.spec`, mirroring the Kubeflow Notebook CRD pattern. This gives full flexibility for container configuration including env vars, args, volume mounts, resource limits, etc.

## Status

The controller reports:
- `readyReplicas` - Number of ready pods (0 or 1)
- `containerState` - Running/waiting/terminated state of the first container
- `conditions` - Pod conditions mirrored to the workspace

## Start/Stop

- **Stop:** Add annotation `kubeworkspaces.io/stopped: "true"` (sets replicas to 0)
- **Start:** Remove the annotation (sets replicas to 1)

The controller watches for annotation changes and updates the StatefulSet replicas accordingly.

## Reconciliation Logic

1. Ensure StatefulSet exists (create or update)
2. Ensure Service exists (create if missing)
3. Check for `kubeworkspaces.io/stopped` annotation -> set replicas to 0 or 1
4. Update workspace status from pod state (container state, conditions, ready replicas)
5. Ensure default container port if not specified (default: 8080)

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
- `apps/statefulsets` - create, get, list, watch, update, delete
- `core/services` - create, get, list, watch, update, delete
- `core/pods` - get, list, watch
- `core/pods/log` - get
- `core/events` - get, list, watch

## License

Apache License 2.0
