package controller

import (
	"context"
	"encoding/json"
	"testing"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestExecutionEpochRevocationIsMonotonicAndIdempotent(t *testing.T) {
	r, ws, vm := managementFixture(t)
	ctx := context.Background()
	if err := r.reconcileManagementIdentity(ctx, ws, vm); err != nil {
		t.Fatal(err)
	}
	old := managementSecret(t, r, ws, vm)
	// The fake client does not enforce CEL. Also exercise defense-in-depth
	// against a lower/missing request after the status floor was acknowledged.
	for _, request := range []int64{3, 3, 1, 0} {
		fresh := &workspacev1.Workspace{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(ws), fresh); err != nil {
			t.Fatal(err)
		}
		fresh.Spec.ExecutionRevocationEpoch = request
		if err := r.Update(ctx, fresh); err != nil {
			t.Fatal(err)
		}
		if err := r.reconcileManagementIdentity(ctx, ws, vm); err != nil {
			t.Fatal(err)
		}
		if ws.Status.ManagementExecution.RevocationEpoch != 3 {
			t.Fatalf("request %d rolled back epoch: %+v", request, ws.Status.ManagementExecution)
		}
		// Ordinary readiness updates must preserve the execution authority.
		if err := r.updateWorkspaceStatus(ctx, ws, 1, nil); err != nil {
			t.Fatal(err)
		}
	}
	fresh := &workspacev1.Workspace{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(ws), fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Status.ManagementExecution.RevocationEpoch != 3 {
		t.Fatal("readiness overwrote epoch")
	}
	newSecret := old.DeepCopy()
	if err := r.Get(ctx, client.ObjectKey{Namespace: old.Namespace, Name: managementBootstrapSecretName(string(ws.UID), string(vm.GetUID()), 3)}, newSecret); err != nil {
		t.Fatal(err)
	}
	var doc managementBootstrapDocument
	if err := json.Unmarshal(newSecret.Data["bootstrap.json"], &doc); err != nil {
		t.Fatal(err)
	}
	if newSecret.Name == old.Name || doc.RevocationEpoch != 3 || string(newSecret.Data["bootstrap.json"]) == string(old.Data["bootstrap.json"]) {
		t.Fatal("revocation reused old enrollment material")
	}
}

func TestExecutionEpochInitializesOnceWithoutBootstrapOptIn(t *testing.T) {
	r, ws, vm := managementFixture(t)
	r.ManagementBootstrapNamespace = ""
	ctx := context.Background()
	ws.Status.ManagementExecution = nil
	if err := r.Status().Update(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileManagementIdentity(ctx, ws, vm); err != nil {
		t.Fatal(err)
	}
	if state := ws.Status.ManagementExecution; state == nil || state.VMUID != string(vm.GetUID()) || state.RevocationEpoch != 0 {
		t.Fatalf("initial epoch not published: %+v", state)
	}
	rv := ws.ResourceVersion
	if err := r.reconcileManagementIdentity(ctx, ws, vm); err != nil {
		t.Fatal(err)
	}
	if ws.ResourceVersion != rv {
		t.Fatal("unchanged epoch rewrote status")
	}
}

func TestExecutionEpochRejectsNegativeRequest(t *testing.T) {
	r, ws, vm := managementFixture(t)
	ws.Spec.ExecutionRevocationEpoch = -1
	if err := r.Update(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileExecutionEpoch(context.Background(), ws, vm); err == nil {
		t.Fatal("negative epoch admitted")
	}
}

func TestExecutionEpochBindsNewVMWithoutLoweringEpoch(t *testing.T) {
	r, ws, vm := managementFixture(t)
	ctx := context.Background()
	ws.Status.ManagementExecution.RevocationEpoch = 5
	if err := r.Status().Update(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, vm); err != nil {
		t.Fatal(err)
	}
	vm.SetUID("new-vm-uid")
	vm.SetResourceVersion("")
	if err := r.Create(ctx, vm); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileExecutionEpoch(ctx, ws, vm); err != nil {
		t.Fatal(err)
	}
	if state := ws.Status.ManagementExecution; state.VMUID != "new-vm-uid" || state.RevocationEpoch != 5 {
		t.Fatalf("reset lost the epoch floor: %+v", state)
	}
}
