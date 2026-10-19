package controller

import (
	"context"
	"fmt"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *WorkspaceReconciler) reconcileManagementIdentity(ctx context.Context, ws *workspacev1.Workspace, vm *unstructured.Unstructured) error {
	if err := r.reconcileExecutionEpoch(ctx, ws, vm); err != nil {
		return err
	}
	return r.reconcileManagementBootstrap(ctx, ws, vm)
}

func requestedExecutionEpoch(ws *workspacev1.Workspace) (int64, error) {
	epoch := ws.Spec.ExecutionRevocationEpoch
	if epoch < 0 {
		return 0, fmt.Errorf("execution revocation epoch must be a nonnegative int64")
	}
	return epoch, nil
}

// reconcileExecutionEpoch uses a fresh Workspace and resource-version protected
// status update. It also copies the result into the caller so later readiness
// status writes cannot overwrite the new execution identity.
func (r *WorkspaceReconciler) reconcileExecutionEpoch(ctx context.Context, instance *workspacev1.Workspace, vm *unstructured.Unstructured) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	ws := &workspacev1.Workspace{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(instance), ws); err != nil {
		return err
	}
	currentVM := &unstructured.Unstructured{}
	currentVM.SetGroupVersionKind(kubeVirtVirtualMachineGVK)
	if err := reader.Get(ctx, client.ObjectKeyFromObject(instance), currentVM); err != nil {
		return err
	}
	if ws.UID == "" || ws.UID != instance.UID || !ws.DeletionTimestamp.IsZero() || !managementVMCurrent(ws, currentVM, vm) {
		return fmt.Errorf("execution epoch identity is not current")
	}
	epoch, err := requestedExecutionEpoch(ws)
	if err != nil {
		return err
	}
	previous := ws.Status.ManagementExecution
	if previous != nil && previous.RevocationEpoch > epoch {
		epoch = previous.RevocationEpoch
	}
	if previous == nil || previous.VMUID != string(vm.GetUID()) || previous.RevocationEpoch != epoch {
		ws.Status.ManagementExecution = &workspacev1.ManagementExecutionStatus{VMUID: string(vm.GetUID()), RevocationEpoch: epoch}
		if ws.Status.Conditions == nil {
			ws.Status.Conditions = []workspacev1.WorkspaceCondition{}
		}
		if err := r.Status().Update(ctx, ws); err != nil {
			return err
		}
	}
	instance.Status.ManagementExecution = ws.Status.ManagementExecution.DeepCopy()
	instance.ResourceVersion = ws.ResourceVersion
	return nil
}
