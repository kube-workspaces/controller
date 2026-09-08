/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

func resetTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kubeworkspacesiov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func testVirtualMachine() *unstructured.Unstructured {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(kubeVirtVirtualMachineGVK)
	vm.SetName(testWorkspaceName)
	vm.SetNamespace("workspaces")
	_ = unstructured.SetNestedField(vm.Object, true, "spec", "running")
	return vm
}

func TestHandleResetNoAnnotation(t *testing.T) {
	scheme := resetTestScheme(t)
	ws := testWorkspace(WorkspaceTypeVM, nil)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws).Build()
	r := &WorkspaceReconciler{Client: cl, Scheme: scheme}

	res, handled, err := r.handleReset(context.Background(), ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("expected not handled without the reset annotation")
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %+v", res)
	}
}

func TestHandleResetDeletesVirtualMachine(t *testing.T) {
	scheme := resetTestScheme(t)
	ws := testWorkspace(WorkspaceTypeVM, map[string]string{
		AnnotationReset: "2026-09-08T04:00:00.000000000Z",
	})
	vm := testVirtualMachine()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws, vm).Build()
	r := &WorkspaceReconciler{Client: cl, Scheme: scheme}

	res, handled, err := r.handleReset(context.Background(), ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected reset to be handled while the VirtualMachine exists")
	}
	if res.RequeueAfter == 0 {
		t.Error("expected the reset to requeue while the VirtualMachine is deleted")
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(kubeVirtVirtualMachineGVK)
	err = cl.Get(context.Background(), types.NamespacedName{Name: testWorkspaceName, Namespace: "workspaces"}, got)
	if !errors.IsNotFound(err) {
		t.Errorf("expected VirtualMachine to be deleted, got err=%v obj=%v", err, got)
	}
}

func TestHandleResetClearsAnnotationWhenVirtualMachineGone(t *testing.T) {
	scheme := resetTestScheme(t)
	ws := testWorkspace(WorkspaceTypeVM, map[string]string{
		AnnotationReset:                         "2026-09-08T04:00:00.000000000Z",
		"kubeworkspaces.io/ready-time-recorded": "true",
	})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws).Build()
	r := &WorkspaceReconciler{Client: cl, Scheme: scheme}

	res, handled, err := r.handleReset(context.Background(), ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected reset to be handled when the VirtualMachine is gone")
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a short requeue before the fresh re-provision")
	}

	got := &kubeworkspacesiov1alpha1.Workspace{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: testWorkspaceName, Namespace: "workspaces"}, got); err != nil {
		t.Fatalf("unable to fetch workspace: %v", err)
	}
	if _, ok := got.Annotations[AnnotationReset]; ok {
		t.Error("expected the reset annotation to be cleared")
	}
	if _, ok := got.Annotations["kubeworkspaces.io/ready-time-recorded"]; ok {
		t.Error("expected the ready-time-recorded annotation to be cleared after a reset")
	}
}
