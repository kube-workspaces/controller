# AGENTS.md

## Repository: kube-workspaces/controller

Kubernetes controller for the kube-workspaces platform. Built with kubebuilder and controller-runtime.

## Structure

| Directory | Purpose |
|-----------|---------|
| `api/v1alpha1/` | CRD type definitions (Workspace, Image, User, AuthConfig, PlatformConfig, PodDefault) |
| `cmd/` | Controller entrypoint |
| `config/` | Kustomize configs: CRDs, RBAC, manager, prometheus, network-policy, samples |
| `internal/controller/` | Reconcilers: workspace, user, authconfig |
| `hack/` | Code generation boilerplate |
| `test/` | E2E test directory |

## Commands

```
make build       # build binary (runs manifests, generate, fmt, vet first)
make test        # unit tests (requires envtest binaries, downloaded automatically)
make lint        # golangci-lint v2.1.6
make test-e2e    # e2e tests (creates/destroys a kind cluster)
make manifests   # regenerate CRD YAML
make generate    # regenerate deepcopy
make run         # run controller locally
```

## CRD Group

- Group: `kubeworkspaces.io`, Version: `v1alpha1`
- Kinds: `Workspace` (namespaced), `Image` (cluster-scoped), `User` (cluster-scoped), `AuthConfig` (cluster-scoped), `PlatformConfig` (cluster-scoped), `PodDefault` (namespaced)

## Key Notes

- Go version: 1.24 (see `go.mod`)
- CRD is too large for client-side apply. Always use `kubectl apply --server-side`.
- Workspace start/stop is annotation-driven: `kubeworkspaces.io/stopped: "true"` sets replicas to 0.
- StatefulSet pod naming: `{workspace-name}-0`.
- Multi-port Service generation: exposes ALL container ports (first port maps to Service port 80).
- User controller reconciles: personal namespace creation, RoleBindings, ResourceQuotas.
- Image defaults (`defaultEnv`, `defaultArgs`, `defaultUID`, `defaultInitContainers`, `additionalPorts`) are applied at workspace creation time only.

## CI

- `.github/workflows/lint.yml` — golangci-lint v2.1.6
- `.github/workflows/test.yml` — unit tests via `make test`
- `.github/workflows/test-e2e.yml` — e2e tests via kind cluster
- `.github/workflows/docker.yml` — build & push `kubeworkspaces/controller` image

## Docker Image

Published to: `kubeworkspaces/controller`
