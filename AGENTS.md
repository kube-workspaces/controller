# AGENTS.md

## Repository: kube-workspaces/controller

Kubernetes controller for the kube-workspaces platform. Built with kubebuilder and controller-runtime.

## Structure

| Directory | Purpose |
|-----------|---------|
| `api/v1alpha1/` | CRD type definitions (Workspace, Image, User, AuthConfig, PlatformConfig, PodDefault, SshKey) |
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
- Kinds: `Workspace` (namespaced), `Image` (cluster-scoped), `User` (cluster-scoped), `AuthConfig` (cluster-scoped), `PlatformConfig` (cluster-scoped), `PodDefault` (namespaced), `SshKey` (namespaced)

## Key Notes

- Go version: 1.24 (see `go.mod`)
- CRD is too large for client-side apply. Always use `kubectl apply --server-side`.
- Workspace start/stop is annotation-driven: `kubeworkspaces.io/stopped: "true"` sets replicas to 0 (or `spec.running: false` for `vm`).
- Workspace `spec.type` selects the workload: `container` → StatefulSet, `vm` → KubeVirt VirtualMachine, `scratch` → Deployment. `generateVirtualMachine` handles containerDisk/DataVolume root, masquerade ports, pinned MAC, cloud-init user-data + SSH-key injection, and GPU passthrough.
- StatefulSet pod naming: `{workspace-name}-0`.
- Multi-port Service generation: exposes ALL container ports (first port maps to Service port 80).
- User controller reconciles: personal namespace creation, RoleBindings, ResourceQuotas.
- Image defaults (`defaultEnv`, `defaultArgs`, `defaultUID`, `defaultInitContainers`, `additionalPorts`) are applied at workspace creation time only.
- Image fields relevant to `vm` workspaces: `workspaceTypes`, `defaultUser`/`defaultPassword`/`defaultCloudInit`/`defaultUserData`, `persistentRootDisk`/`persistentRootDiskSize`, `memoryLimit`/`memoryRequest`, and `videoDevice` (e.g. `virtio` for the virtio-gpu paravirtual display).
- Persistent VM DataVolumes/PVCs use `{workspace-name}-rootdisk` (long names are shortened with a hash). For migrations only, an existing **VirtualMachine** can carry `kubeworkspaces.io/legacy-root-disk: "true"` to retain its existing `rootdisk` reference. Ensure that disk has a single VM consumer and its DataVolume owner reference points to that VM before migrating the other consumers. This annotation belongs on the VM, not the Workspace, so clones receive isolated disks.
- VM user `volumeMounts` reference independently managed blank CDI DataVolumes labelled `kubeworkspaces.io/volume-type: vm-disk`. They have no Workspace/VM owner reference and survive reset/deletion. An atomic `kubeworkspaces.io/disk-workspace: name/uid` claim allows one writable workspace; a deleted owner's claim is reusable after its VMI disappears. Stable virtio serials feed per-boot ext4 mounting; only signature-free blank disks are formatted, existing ext4 data is retained.
- SshKey CRs: create/update/delete events reconcile VM workspaces in the same namespace. Images with annotation `kubeworkspaces.io/ssh-key-propagation: qemuGuestAgent` and `defaultUser` use a Workspace-owned `{name}-sshkeys` Secret and KubeVirt `accessCredentials` / `qemuGuestAgent` propagation (requires a running guest agent; replaces that user's `authorized_keys`, including revocation). Existing VMs need one restart to attach the Secret; no restart is forced. Other images retain first-boot cloud-init seeding.

## CI

- `.github/workflows/lint.yml` — golangci-lint v2.1.6
- `.github/workflows/test.yml` — unit tests via `make test`
- `.github/workflows/test-e2e.yml` — e2e tests via kind cluster
- `.github/workflows/docker.yml` — build & push `kubeworkspaces/controller` image

## Docker Image

Published to: `ghcr.io/kube-workspaces/controller`
