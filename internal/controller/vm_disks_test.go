package controller

import (
	"context"
	"strings"
	"testing"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

func TestVMDataDiskClaim(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(WorkspaceTypeVM, nil)
	ws.UID = "first-owner"
	disk := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "DataVolume", "metadata": map[string]interface{}{
			"name": "data", "namespace": ws.Namespace, "labels": map[string]interface{}{vmDiskTypeLabel: "vm-disk"}},
		"spec": map[string]interface{}{"source": map[string]interface{}{"blank": map[string]interface{}{}}},
	}}
	cl := fake.NewClientBuilder().WithScheme(resetTestScheme(t)).WithObjects(ws, disk).Build()
	r := &WorkspaceReconciler{Client: cl, APIReader: cl}
	if err := r.claimVMDataVolume(ctx, cl, ws, "data"); err != nil {
		t.Fatal(err)
	}
	other := ws.DeepCopy()
	other.Name, other.UID = "second-vm", "second-owner"
	if err := r.claimVMDataVolume(ctx, cl, other, "data"); err == nil {
		t.Fatal("must not permit two writable consumers")
	}
	if err := cl.Delete(ctx, ws); err != nil {
		t.Fatal(err)
	}
	vmi := testVirtualMachine()
	vmi.SetGroupVersionKind(kubeVirtVirtualMachineInstanceGVK)
	if err := cl.Create(ctx, vmi); err != nil {
		t.Fatal(err)
	}
	if err := r.claimVMDataVolume(ctx, cl, other, "data"); err == nil {
		t.Fatal("must wait for the deleted workspace's VMI to disappear")
	}
	if err := cl.Delete(ctx, vmi); err != nil {
		t.Fatal(err)
	}
	if err := r.claimVMDataVolume(ctx, cl, other, "data"); err != nil {
		t.Fatalf("retained disk should be reusable after old VMI deletion: %v", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: "data"}, disk); err != nil {
		t.Fatal(err)
	}
	if len(disk.GetOwnerReferences()) != 0 || disk.GetAnnotations()[vmDiskOwnerAnnotation] != "second-vm/second-owner" {
		t.Fatal("disk ownership must be a claim, not a garbage-collection reference")
	}
}

func TestVMDataDiskCloudInit(t *testing.T) {
	ws := testWorkspace(WorkspaceTypeVM, nil)
	ws.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data's directory"}}
	img := &kubeworkspacesiov1alpha1.Image{}
	img.Spec.DefaultCloudInit = true
	img.Spec.DefaultUserData = "#cloud-config\nbootcmd:\n  - echo existing\nruncmd:\n  - echo provision\n"
	data, err := vmCloudInitUserData(ws, img, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]interface{}{}
	if err := yaml.Unmarshal([]byte(data), &body); err != nil {
		t.Fatal(err)
	}
	commands := body["bootcmd"].([]interface{})
	if len(commands) != 2 || commands[0] != "echo existing" || !strings.Contains(commands[1].(string), "'\"'\"'") {
		t.Fatal("preserve existing commands and shell-quote guest paths")
	}
	vm := generateVirtualMachine(ws, img, nil)
	disks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	last := disks[len(disks)-1].(map[string]interface{})
	if last["serial"] != vmDiskSerial("data") || len(vmDiskSerial("data")) > 20 {
		t.Fatal("virtio serial must match the stable by-id guest path")
	}
	ws.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath = "/etc"
	if _, err := vmCloudInitUserData(ws, img, nil); err == nil {
		t.Fatal("must reject system-directory mount paths")
	}
}
