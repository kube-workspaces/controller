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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

func TestWorkspacesForSSHKey(t *testing.T) {
	vm := testWorkspace(WorkspaceTypeVM, nil)
	stopped := testWorkspace(WorkspaceTypeVM, map[string]string{AnnotationStopped: "true"})
	stopped.Name = "stopped"
	container := testWorkspace(WorkspaceTypeContainer, nil)
	container.Name = "container"
	scratch := testWorkspace(WorkspaceTypeScratch, nil)
	scratch.Name = "scratch"
	other := testWorkspace(WorkspaceTypeVM, nil)
	other.Namespace = "other-user"
	cl := fake.NewClientBuilder().WithScheme(resetTestScheme(t)).WithObjects(vm, stopped, container, scratch, other).Build()
	r := &WorkspaceReconciler{Client: cl}
	// The event object need not exist in the client: delete events still map.
	key := &kubeworkspacesiov1alpha1.SshKey{ObjectMeta: metav1.ObjectMeta{Name: "deleted-key", Namespace: vm.Namespace}}
	requests := r.workspacesForSSHKey(context.Background(), key)
	want := map[types.NamespacedName]bool{
		{Namespace: vm.Namespace, Name: vm.Name}:           true,
		{Namespace: stopped.Namespace, Name: stopped.Name}: true,
	}
	for _, req := range requests {
		if !want[req.NamespacedName] {
			t.Fatalf("unexpected or duplicate request: %v", req)
		}
		delete(want, req.NamespacedName)
	}
	if len(want) != 0 {
		t.Fatalf("missing requests: %v", want)
	}
}

func TestSSHKeySecretLifecycle(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(WorkspaceTypeVM, nil)
	ws.UID = "workspace-uid"
	scheme := resetTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws).Build()
	r := &WorkspaceReconciler{Client: cl, Scheme: scheme}
	name := types.NamespacedName{Namespace: ws.Namespace, Name: sshKeySecretName(ws.Name)}
	// Empty -> first key -> rotation -> last key deleted: the Secret remains
	// attached under one name so every transition can propagate to a live VMI.
	for _, keys := range [][]string{nil, {"key-one"}, {"key-two", "key-three"}, nil} {
		if err := r.reconcileSSHKeySecret(ctx, ws, keys); err != nil {
			t.Fatal(err)
		}
		secret := &corev1.Secret{}
		if err := cl.Get(ctx, name, secret); err != nil {
			t.Fatal(err)
		}
		if got := string(secret.Data["authorized_keys"]); got != strings.Join(keys, "\n") {
			t.Fatalf("unexpected key data: %q", got)
		}
		owner := metav1.GetControllerOf(secret)
		if owner == nil || owner.UID != ws.UID {
			t.Fatalf("Secret must be garbage collected with Workspace: %v", owner)
		}
		rv := secret.ResourceVersion
		if err := r.reconcileSSHKeySecret(ctx, ws, keys); err != nil {
			t.Fatal(err)
		}
		if err := cl.Get(ctx, name, secret); err != nil {
			t.Fatal(err)
		}
		if secret.ResourceVersion != rv {
			t.Fatal("unchanged keys must not cause a Secret update loop")
		}
	}
}

func TestGuestAgentSSHCredentials(t *testing.T) {
	ws := testWorkspace(WorkspaceTypeVM, nil)
	img := &kubeworkspacesiov1alpha1.Image{}
	img.Annotations = map[string]string{AnnotationSSHKeyPropagation: "qemuGuestAgent"}
	img.Spec.DefaultCloudInit = true
	img.Spec.DefaultUser = testGuestUser
	img.Spec.DefaultUserData = testCloudConfig
	vm := generateVirtualMachine(ws, img, []string{"key-one"}, 0)
	credentials, found, err := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "accessCredentials")
	if err != nil || !found || len(credentials) != 1 {
		t.Fatalf("missing guest-agent credentials: %v %v", credentials, err)
	}
	credential := credentials[0].(map[string]interface{})
	secretName, _, _ := unstructured.NestedString(credential, "sshPublicKey", "source", "secret", "secretName")
	users, _, _ := unstructured.NestedStringSlice(credential, "sshPublicKey", "propagationMethod", "qemuGuestAgent", "users")
	if secretName != sshKeySecretName(ws.Name) || len(users) != 1 || users[0] != img.Spec.DefaultUser {
		t.Fatalf("unexpected credential source or user: %v", credential)
	}
	encoded, err := vm.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "key-one") {
		t.Fatal("managed keys must not be baked into cloud-init data")
	}
	// Key changes only update the Secret, not the VM template or running VMI.
	rotated := generateVirtualMachine(ws, img, []string{"key-two"}, 0)
	if virtualMachineNeedsUpdate(rotated, vm) {
		t.Fatal("key rotation must not change the VM template")
	}
	img.Spec.DefaultUser = ""
	legacy := generateVirtualMachine(ws, img, []string{"key-one"}, 0)
	if _, found, _ := unstructured.NestedSlice(legacy.Object, "spec", "template", "spec", "accessCredentials"); found {
		t.Fatal("a guest account is required for credential propagation")
	}
	if !virtualMachineNeedsUpdate(legacy, vm) {
		t.Fatal("removing credential configuration must update the VM template")
	}
	img.Spec.DefaultUser = testGuestUser
	img.Annotations = nil
	legacy = generateVirtualMachine(ws, img, []string{"key-one"}, 0)
	if _, found, _ := unstructured.NestedSlice(legacy.Object, "spec", "template", "spec", "accessCredentials"); found {
		t.Fatal("cloud-init capability alone must not opt a guest into agent-owned keys")
	}
	if name := sshKeySecretName(strings.Repeat("a", 253)); len(name) > 253 {
		t.Fatalf("Secret name exceeds Kubernetes limit: %d", len(name))
	}
}
