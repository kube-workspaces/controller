package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const managementStoppedCase = "stopped"

func managementFixture(t *testing.T) (*WorkspaceReconciler, *workspacev1.Workspace, *unstructured.Unstructured) {
	t.Helper()
	scheme := resetTestScheme(t)
	ws := testWorkspace(WorkspaceTypeVM, nil)
	ws.SetNamespace("team")
	ws.SetName("vm")
	ws.SetUID("workspace-uid")
	vm := testVirtualMachine()
	vm.SetNamespace(ws.Namespace)
	vm.SetName(ws.Name)
	vm.SetUID("vm-uid")
	ws.Status.ManagementExecution = &workspacev1.ManagementExecutionStatus{VMUID: string(vm.GetUID()), RevocationEpoch: 0}
	yes := true
	vm.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubeworkspaces.io/v1alpha1", Kind: "Workspace", Name: ws.Name, UID: ws.UID, Controller: &yes}})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(ws).WithObjects(ws, vm).Build()
	return &WorkspaceReconciler{Client: cl, APIReader: cl, Scheme: scheme, ManagementBootstrapNamespace: "control-system"}, ws, vm
}

func managementSecret(t *testing.T, r *WorkspaceReconciler, ws *workspacev1.Workspace, vm *unstructured.Unstructured) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: r.ManagementBootstrapNamespace, Name: managementBootstrapSecretName(string(ws.UID), string(vm.GetUID()))}, secret); err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestManagementBootstrapProtectedAndIdempotent(t *testing.T) {
	r, ws, vm := managementFixture(t)
	ctx := context.Background()
	if err := r.reconcileManagementBootstrap(ctx, ws, vm); err != nil {
		t.Fatal(err)
	}
	secret := managementSecret(t, r, ws, vm)
	if secret.Namespace == ws.Namespace || !managementBootstrapMatches(secret, ws, vm, time.Now()) {
		t.Fatal("unsafe management bootstrap storage")
	}
	if err := r.reconcileManagementBootstrap(ctx, ws, vm); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secret.Data["bootstrap.json"], managementSecret(t, r, ws, vm).Data["bootstrap.json"]) {
		t.Fatal("reconcile rotated a live bootstrap credential")
	}
	var tenants corev1.SecretList
	if err := r.List(ctx, &tenants, client.InNamespace(ws.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(tenants.Items) != 0 {
		t.Fatal("management credential exposed in workspace namespace")
	}
}

func TestManagementBootstrapOptInAndLifecycleFencing(t *testing.T) {
	if err := (&WorkspaceReconciler{}).reconcileManagementBootstrap(context.Background(), nil, nil); err != nil {
		t.Fatal("disabled provisioning touched Kubernetes", err)
	}
	for _, test := range []string{"tenant-namespace", "invalid-namespace", "reset", managementStoppedCase, "reboot", "foreign-vm-owner", "replaced-vm"} {
		t.Run(test, func(t *testing.T) {
			r, ws, vm := managementFixture(t)
			ctx := context.Background()
			switch test {
			case "tenant-namespace":
				r.ManagementBootstrapNamespace = ws.Namespace
			case "invalid-namespace":
				r.ManagementBootstrapNamespace = "invalid/namespace"
			case "reset", managementStoppedCase, "reboot":
				fresh := &workspacev1.Workspace{}
				if err := r.Get(ctx, client.ObjectKeyFromObject(ws), fresh); err != nil {
					t.Fatal(err)
				}
				fresh.Annotations = map[string]string{"kubeworkspaces.io/" + test: ""}
				if err := r.Update(ctx, fresh); err != nil {
					t.Fatal(err)
				}
			case "foreign-vm-owner":
				fresh := vm.DeepCopy()
				fresh.SetOwnerReferences(nil)
				if err := r.Update(ctx, fresh); err != nil {
					t.Fatal(err)
				}
			case "replaced-vm":
				if err := r.Delete(ctx, vm); err != nil {
					t.Fatal(err)
				}
				fresh := vm.DeepCopy()
				fresh.SetResourceVersion("")
				fresh.SetUID("other-vm-uid")
				if err := r.Create(ctx, fresh); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.reconcileManagementBootstrap(ctx, ws, vm); err == nil {
				t.Fatal("unsafe management bootstrap created")
			}
		})
	}
}

func TestManagementBootstrapRefusesSecretAdoptionAndAutoRenewal(t *testing.T) {
	r, ws, vm := managementFixture(t)
	ctx := context.Background()
	if err := r.reconcileManagementBootstrap(ctx, ws, vm); err != nil {
		t.Fatal(err)
	}
	secret := managementSecret(t, r, ws, vm)
	var doc managementBootstrapDocument
	if err := json.Unmarshal(secret.Data["bootstrap.json"], &doc); err != nil {
		t.Fatal(err)
	}
	doc.ExpiresAt = time.Now().Add(-time.Hour)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	secret.Data["bootstrap.json"] = raw
	// The fake client permits immutable Secret mutation here to construct an
	// expired fixture. Real provisioning never updates immutable Secret data.
	if err := r.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileManagementBootstrap(ctx, ws, vm); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, managementSecret(t, r, ws, vm).Data["bootstrap.json"]) {
		t.Fatal("expired token silently renewed")
	}
	secret = managementSecret(t, r, ws, vm)
	secret.Labels["kubeworkspaces.io/workspace-uid"] = "foreign"
	if err := r.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileManagementBootstrap(ctx, ws, vm); err == nil {
		t.Fatal("foreign Secret adopted")
	}
}

func TestManagementBootstrapContractFixture(t *testing.T) {
	r, ws, vm := managementFixture(t)
	if managementBootstrapSecretName(string(ws.UID), string(vm.GetUID())) != "kw-mgmt-f1bbc6ce0ef901c6028526a778156f7f" {
		t.Fatal("management Secret naming contract drift")
	}
	if err := r.reconcileManagementBootstrap(context.Background(), ws, vm); err != nil {
		t.Fatal(err)
	}
	secret := managementSecret(t, r, ws, vm)
	raw, err := os.ReadFile("testdata/management-bootstrap-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	secret.Data["bootstrap.json"] = raw
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if !managementBootstrapMatches(secret, ws, vm, now) {
		t.Fatal("management-v1 fixture drift")
	}
}
