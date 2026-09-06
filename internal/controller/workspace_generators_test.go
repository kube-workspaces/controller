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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

func testWorkspace(wsType string, annotations map[string]string) *kubeworkspacesiov1alpha1.Workspace {
	return &kubeworkspacesiov1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "my-ws",
			Namespace:   "workspaces",
			Annotations: annotations,
		},
		Spec: kubeworkspacesiov1alpha1.WorkspaceSpec{
			Type: wsType,
			Template: kubeworkspacesiov1alpha1.WorkspaceTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "my-ws",
							Image: "quay.io/containerdisks/fedora:latest",
							Ports: []corev1.ContainerPort{
								{ContainerPort: 22, Name: "ssh"},
								{ContainerPort: 5901, Name: "vnc"},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("2"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
							},
						},
					},
				},
			},
		},
	}
}

func TestWorkspaceTypeOf(t *testing.T) {
	if got := workspaceTypeOf(testWorkspace("", nil)); got != WorkspaceTypeContainer {
		t.Errorf("empty type should default to container, got %q", got)
	}
	if got := workspaceTypeOf(testWorkspace("vm", nil)); got != WorkspaceTypeVM {
		t.Errorf("expected vm, got %q", got)
	}
}

func TestGenerateDeployment(t *testing.T) {
	dep := generateDeployment(testWorkspace(WorkspaceTypeScratch, nil))

	if *dep.Spec.Replicas != 1 {
		t.Errorf("expected 1 replica, got %d", *dep.Spec.Replicas)
	}
	if dep.Spec.Selector.MatchLabels["deployment"] != "my-ws" {
		t.Errorf("unexpected selector: %v", dep.Spec.Selector.MatchLabels)
	}
	if dep.Spec.Template.Labels[LabelWorkspaceName] != "my-ws" {
		t.Errorf("pod template missing workspace-name label: %v", dep.Spec.Template.Labels)
	}

	stopped := generateDeployment(testWorkspace(WorkspaceTypeScratch, map[string]string{AnnotationStopped: "true"}))
	if *stopped.Spec.Replicas != 0 {
		t.Errorf("stopped workspace should have 0 replicas, got %d", *stopped.Spec.Replicas)
	}
}

func TestGenerateVirtualMachine(t *testing.T) {
	vm := generateVirtualMachine(testWorkspace(WorkspaceTypeVM, nil))

	if vm.GetName() != "my-ws" || vm.GetNamespace() != "workspaces" {
		t.Errorf("unexpected VM identity: %s/%s", vm.GetNamespace(), vm.GetName())
	}
	if vm.GroupVersionKind() != kubeVirtVirtualMachineGVK {
		t.Errorf("unexpected GVK: %v", vm.GroupVersionKind())
	}

	running, _, _ := unstructured.NestedBool(vm.Object, "spec", "running")
	if !running {
		t.Error("expected spec.running=true for a non-stopped workspace")
	}

	// containerDisk root from the main container image
	image, _, _ := unstructured.NestedString(vm.Object,
		"spec", "template", "spec", "volumes", "0", "containerDisk", "image")
	_ = image // checked via slice below (NestedString doesn't index arrays)

	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if len(volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(volumes))
	}
	vol := volumes[0].(map[string]interface{})
	cd := vol["containerDisk"].(map[string]interface{})
	if cd["image"] != "quay.io/containerdisks/fedora:latest" {
		t.Errorf("unexpected containerDisk image: %v", cd["image"])
	}

	// Resources mapped to the domain
	reqs, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "spec", "domain", "resources", "requests")
	if reqs["cpu"] != "500m" || reqs["memory"] != "1Gi" {
		t.Errorf("unexpected domain requests: %v", reqs)
	}

	// One masquerade interface on the pod network forwarding both declared ports
	ifaces, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "interfaces")
	if len(ifaces) != 1 {
		t.Fatalf("expected 1 interface, got %d", len(ifaces))
	}
	iface := ifaces[0].(map[string]interface{})
	if _, ok := iface["masquerade"]; !ok {
		t.Errorf("interface missing masquerade: %v", iface)
	}
	ports, _, _ := unstructured.NestedSlice(iface, "ports")
	if len(ports) != 2 {
		t.Errorf("expected 2 forwarded ports, got %d", len(ports))
	}

	// workspace-name label on the VMI template (drives the pod watch)
	labels, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "metadata", "labels")
	if labels[LabelWorkspaceName] != "my-ws" {
		t.Errorf("VMI template missing workspace-name label: %v", labels)
	}
}

func TestGenerateVirtualMachineStopped(t *testing.T) {
	vm := generateVirtualMachine(testWorkspace(WorkspaceTypeVM, map[string]string{AnnotationStopped: "true"}))
	running, _, _ := unstructured.NestedBool(vm.Object, "spec", "running")
	if running {
		t.Error("expected spec.running=false for a stopped workspace")
	}
}

func TestGenerateServiceSelectors(t *testing.T) {
	ws := testWorkspace(WorkspaceTypeContainer, nil)

	containerSvc := generateService(ws, WorkspaceTypeContainer)
	if containerSvc.Spec.Selector["statefulset"] != "my-ws" {
		t.Errorf("container service should select statefulset label: %v", containerSvc.Spec.Selector)
	}

	for _, wsType := range []string{WorkspaceTypeVM, WorkspaceTypeScratch} {
		svc := generateService(ws, wsType)
		if svc.Spec.Selector[LabelWorkspaceName] != "my-ws" {
			t.Errorf("%s service should select workspace-name label: %v", wsType, svc.Spec.Selector)
		}
		if _, ok := svc.Spec.Selector["statefulset"]; ok {
			t.Errorf("%s service must not use the statefulset selector: %v", wsType, svc.Spec.Selector)
		}
	}

	// First container port maps to Service port 80
	svc := generateService(ws, WorkspaceTypeVM)
	if svc.Spec.Ports[0].Port != DefaultServingPort {
		t.Errorf("expected first service port 80, got %d", svc.Spec.Ports[0].Port)
	}
	if svc.Spec.Ports[0].TargetPort.IntValue() != 22 {
		t.Errorf("expected target port 22, got %d", svc.Spec.Ports[0].TargetPort.IntValue())
	}
}
