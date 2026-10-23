package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func validateVMGPUProfile(profile *workspacev1.VMGPUProfile) error {
	if profile == nil {
		return nil
	}
	parts := strings.Split(profile.ResourceName, "/")
	if len(parts) != 2 || len(profile.ResourceName) > 253 || strings.HasPrefix(profile.ResourceName, "requests.") || len(validation.IsDNS1123Subdomain(parts[0])) != 0 ||
		len(validation.IsQualifiedName(profile.ResourceName)) != 0 ||
		parts[0] == "kubernetes.io" || strings.HasSuffix(parts[0], ".kubernetes.io") ||
		parts[0] == "k8s.io" || strings.HasSuffix(parts[0], ".k8s.io") {
		return fmt.Errorf("VM GPU profile requires a vendor extended resource")
	}
	if len(profile.ID) > 63 || len(validation.IsDNS1123Label(profile.ID)) != 0 ||
		profile.DisplayName == "" || len(profile.DisplayName) > 128 ||
		profile.GuestDriverVersion == "" || len(profile.GuestDriverVersion) > 128 ||
		profile.Count != 1 || (profile.Kind != "pci" && profile.Kind != "mediated") || len(profile.NodeSelector) == 0 {
		return fmt.Errorf("invalid versioned VM GPU profile")
	}
	for key, value := range profile.NodeSelector {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return fmt.Errorf("invalid VM GPU worker selector")
		}
	}
	return nil
}

// preflightWindowsGPU validates the snapshot rather than deriving devices from
// arbitrary container limits. Platform profiles are versioned operator contracts.
func (r *WorkspaceReconciler) preflightWindowsGPU(ctx context.Context, instance *workspacev1.Workspace) error {
	profile := instance.Spec.VMProfile.GPU
	if profile == nil {
		return nil
	}
	if err := validateVMGPUProfile(profile); err != nil {
		return err
	}
	platform := &workspacev1.PlatformConfig{}
	if err := r.Get(ctx, types.NamespacedName{Name: "default"}, platform); err != nil {
		return fmt.Errorf("VM GPU operator profiles are unavailable")
	}
	approved := false
	for i := range platform.Spec.VMGPUProfiles {
		candidate := &platform.Spec.VMGPUProfiles[i]
		if candidate.ID == profile.ID {
			if approved || !reflect.DeepEqual(candidate, profile) {
				return fmt.Errorf("VM GPU snapshot does not match its versioned operator profile")
			}
			approved = true
		}
	}
	if !approved {
		return fmt.Errorf("VM GPU profile is not approved by the operator")
	}
	if err := r.preflightWindowsNode(ctx, instance); err != nil {
		return err
	}
	nodes := &corev1.NodeList{}
	host := instance.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
	if err := r.List(ctx, nodes, client.MatchingLabels{"kubernetes.io/hostname": host}); err != nil || len(nodes.Items) != 1 {
		return fmt.Errorf("VM GPU requires an available certified worker")
	}
	node := &nodes.Items[0]
	for key, value := range profile.NodeSelector {
		actual, found := node.Labels[key]
		if !found || actual != value {
			return fmt.Errorf("selected worker is not eligible for this VM GPU profile")
		}
	}
	quantity := node.Status.Allocatable[corev1.ResourceName(profile.ResourceName)]
	if quantity.Cmp(resource.MustParse("1")) < 0 {
		return fmt.Errorf("selected worker does not advertise the VM GPU resource")
	}
	installations := &unstructured.UnstructuredList{}
	installations.SetGroupVersionKind(kubeVirtVirtualMachineGVK.GroupVersion().WithKind("KubeVirtList"))
	if err := r.List(ctx, installations); err != nil || len(installations.Items) != 1 {
		return fmt.Errorf("VM GPU KubeVirt allowlist is unavailable")
	}
	field := "pciHostDevices"
	if profile.Kind == "mediated" {
		field = "mediatedDevices"
	}
	allowed, _, _ := unstructured.NestedSlice(installations.Items[0].Object, "spec", "configuration", "permittedHostDevices", field)
	for _, entry := range allowed {
		if item, ok := entry.(map[string]interface{}); ok && item["resourceName"] == profile.ResourceName {
			return nil
		}
	}
	return fmt.Errorf("VM GPU resource is not in the matching KubeVirt allowlist")
}

func applyWindowsGPU(instance *workspacev1.Workspace, domain map[string]interface{}) {
	profile := instance.Spec.VMProfile.GPU
	if profile == nil {
		return
	}
	devices := domain["devices"].(map[string]interface{})
	devices["gpus"] = []interface{}{map[string]interface{}{
		"name": "gpu0", "deviceName": profile.ResourceName,
		"virtualGPUOptions": map[string]interface{}{"display": map[string]interface{}{"enabled": false}},
	}}
	resources := domain["resources"].(map[string]interface{})
	resources["requests"].(map[string]interface{})[profile.ResourceName] = "1"
	resources["limits"] = map[string]interface{}{profile.ResourceName: "1"}
}

func windowsGPUHardware(vm *unstructured.Unstructured) map[string]interface{} {
	gpus, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "gpus")
	result := map[string]interface{}{}
	if len(gpus) > 0 {
		result["gpus"] = gpus
	}
	// Include every extended resource, so a changed resource name/count cannot
	// slip through just because a device list was left unchanged.
	for _, field := range []string{"requests", "limits"} {
		values, _, _ := unstructured.NestedMap(vm.Object, "spec", "template", "spec", "domain", "resources", field)
		for name, value := range values {
			if strings.Contains(name, "/") {
				quantity, err := resource.ParseQuantity(fmt.Sprint(value))
				if err == nil {
					value = quantity.String()
				}
				result[field+"/"+name] = value
			}
		}
	}
	return result
}

// guardWindowsGPUTransition prevents incidental reconcile-time GPU mutations.
// A simultaneous Start may proceed only after the old VM is actually halted
// and has no VMI. Merely setting the stopped annotation is not sufficient.
func (r *WorkspaceReconciler) guardWindowsGPUTransition(ctx context.Context, instance *workspacev1.Workspace, desired, current *unstructured.Unstructured) error {
	if reflect.DeepEqual(windowsGPUHardware(desired), windowsGPUHardware(current)) {
		return nil // Stop must remain possible even if the operator removes a profile.
	}
	running, hasRunning, _ := unstructured.NestedBool(current.Object, "spec", "running")
	strategy, _, _ := unstructured.NestedString(current.Object, "spec", "runStrategy")
	if (hasRunning && running) || (!hasRunning && strategy != "Halted") {
		return fmt.Errorf("VM GPU changes require the existing virtual machine fully stopped")
	}
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(kubeVirtVirtualMachineInstanceGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, vmi); !apierrors.IsNotFound(err) {
		return fmt.Errorf("VM GPU changes require the previous VMI to be absent")
	}
	return r.preflightWindowsGPU(ctx, instance)
}
