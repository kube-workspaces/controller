package controller

import (
	"context"
	"reflect"
	"testing"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testVMGPUProfile() *workspacev1.VMGPUProfile {
	return &workspacev1.VMGPUProfile{ID: "intel-sriov-experimental-v1", DisplayName: "Experimental Intel VF", ResourceName: "intel.com/render-vf", Kind: "pci", Count: 1, NodeSelector: map[string]string{"example.com/vm-gpu": "qualified"}, GuestDriverVersion: "32.0.101.6452", Experimental: true}
}

func testVMGPUPrerequisites(profile *workspacev1.VMGPUProfile) (*workspacev1.PlatformConfig, *corev1.Node, *unstructured.Unstructured) {
	platform := &workspacev1.PlatformConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: workspacev1.PlatformConfigSpec{VMGPUProfiles: []workspacev1.VMGPUProfile{*profile.DeepCopy()}}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-node", Labels: map[string]string{"kubernetes.io/hostname": "worker", "kubernetes.io/arch": "amd64", windowsEligibleNodeLabel: "true", "example.com/vm-gpu": "qualified"}}, Status: corev1.NodeStatus{
		Allocatable: corev1.ResourceList{"intel.com/render-vf": resource.MustParse("1"), "devices.kubevirt.io/kvm": resource.MustParse("1000")},
		Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
	}}
	kv := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "kubevirt.io/v1", "kind": "KubeVirt", "metadata": map[string]interface{}{"name": "kubevirt", "namespace": "kubevirt"}, "spec": map[string]interface{}{"configuration": map[string]interface{}{"permittedHostDevices": map[string]interface{}{"pciHostDevices": []interface{}{map[string]interface{}{"resourceName": profile.ResourceName}}}}}}}
	return platform, node, kv
}

func TestWindowsGPUPreflightRejectsUnapprovedOrIneligibleAllocation(t *testing.T) {
	for _, scenario := range []string{"valid", "forged snapshot", "unknown profile", "wrong worker", "unhealthy device", "wrong allowlist kind", "cordoned"} {
		t.Run(scenario, func(t *testing.T) {
			ws := windowsTestWorkspace()
			ws.Spec.VMProfile.GPU = testVMGPUProfile()
			platform, node, kv := testVMGPUPrerequisites(ws.Spec.VMProfile.GPU)
			switch scenario {
			case "forged snapshot":
				ws.Spec.VMProfile.GPU.ResourceName = "other.example/escape"
			case "unknown profile":
				platform.Spec.VMGPUProfiles = nil
			case "wrong worker":
				delete(node.Labels, "example.com/vm-gpu")
			case "unhealthy device":
				node.Status.Allocatable["intel.com/render-vf"] = resource.MustParse("0")
			case "wrong allowlist kind":
				unstructured.RemoveNestedField(kv.Object, "spec", "configuration", "permittedHostDevices", "pciHostDevices")
				_ = unstructured.SetNestedSlice(kv.Object, []interface{}{map[string]interface{}{"resourceName": "intel.com/render-vf"}}, "spec", "configuration", "permittedHostDevices", "mediatedDevices")
			case "cordoned":
				node.Spec.Unschedulable = true
			}
			scheme := resetTestScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform, node, kv).Build()
			r := &WorkspaceReconciler{Client: c, Scheme: scheme}
			err := r.preflightWindowsGPU(context.Background(), ws)
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("preflight %s: %v", scenario, err)
			}
		})
	}
}

func TestWindowsGPUChangesPreserveRootAndIdentityAndRequireActualStop(t *testing.T) {
	ctx := context.Background()
	ws := windowsTestWorkspace()
	ws.Annotations = map[string]string{AnnotationStopped: "true", windowsProvisionedGeneration: ws.Spec.VMProfile.Generation}
	baseline := generateWindowsVirtualMachine(ws, 44787)
	ws.Spec.VMProfile.GPU = testVMGPUProfile()
	desired := generateWindowsVirtualMachine(ws, 44787)
	platform, node, kv := testVMGPUPrerequisites(ws.Spec.VMProfile.GPU)
	scheme := resetTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform, node, kv).Build()
	r := &WorkspaceReconciler{Client: c, Scheme: scheme}
	if err := r.guardWindowsGPUTransition(ctx, ws, desired, baseline); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{{"spec", "dataVolumeTemplates"}, {"spec", "template", "spec", "domain", "firmware"}, {"spec", "template", "spec", "domain", "devices", "tpm"}, {"spec", "template", "spec", "volumes"}} {
		before, _, _ := unstructured.NestedFieldCopy(baseline.Object, path...)
		after, _, _ := unstructured.NestedFieldCopy(desired.Object, path...)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("GPU changed preserved state at %v", path)
		}
	}
	gpus, _, _ := unstructured.NestedSlice(desired.Object, "spec", "template", "spec", "domain", "devices", "gpus")
	if len(gpus) != 1 || gpus[0].(map[string]interface{})["deviceName"] != "intel.com/render-vf" {
		t.Fatal("vendor resource without /gpu suffix was not attached")
	}
	for _, field := range []string{"requests", "limits"} {
		value, _, _ := unstructured.NestedString(desired.Object, "spec", "template", "spec", "domain", "resources", field, "intel.com/render-vf")
		if value != "1" {
			t.Fatal("GPU attachment lacks matching extended-resource allocation")
		}
	}
	video, _, _ := unstructured.NestedString(desired.Object, "spec", "template", "spec", "domain", "devices", "video", "type")
	display, _, _ := unstructured.NestedBool(gpus[0].(map[string]interface{}), "virtualGPUOptions", "display", "enabled")
	if video != "vga" || display {
		t.Fatal("GPU profile removed VGA recovery or enabled GPU display")
	}
	_ = unstructured.SetNestedField(baseline.Object, true, "spec", "running")
	if err := r.guardWindowsGPUTransition(ctx, ws, desired, baseline); err == nil {
		t.Fatal("stopped annotation allowed mutation of a still-running VM")
	}
	_ = unstructured.SetNestedField(baseline.Object, false, "spec", "running")
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(kubeVirtVirtualMachineInstanceGVK)
	vmi.SetName(ws.Name)
	vmi.SetNamespace(ws.Namespace)
	if err := c.Create(ctx, vmi); err != nil {
		t.Fatal(err)
	}
	if err := r.guardWindowsGPUTransition(ctx, ws, desired, baseline); err == nil {
		t.Fatal("terminating/remaining VMI allowed a GPU transition")
	}
	if err := c.Delete(ctx, vmi); err != nil {
		t.Fatal(err)
	}
	ws.Spec.VMProfile.GPU = nil
	if err := r.guardWindowsGPUTransition(ctx, ws, baseline, desired); err != nil {
		t.Fatalf("software rollback failed: %v", err)
	}
	// A removed operator profile must not prevent ordinary Stop of unchanged hardware.
	if err := c.Delete(ctx, platform); err != nil {
		t.Fatal(err)
	}
	if err := r.guardWindowsGPUTransition(ctx, ws, desired, desired.DeepCopy()); err != nil {
		t.Fatalf("unchanged GPU stop requires unavailable policy: %v", err)
	}
}

func TestWindowsGPURejectsGenericContainerLimits(t *testing.T) {
	ws := windowsTestWorkspace()
	ws.Spec.Template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{"intel.com/render-vf": resource.MustParse("1")}
	if validateWindowsProfile(ws) == nil {
		t.Fatal("Windows accepted generic container GPU resource")
	}
}
