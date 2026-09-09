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
	"encoding/base64"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

const testWorkspaceName = "my-ws"

// testCloudConfig is a minimal cloud-config used across the user-data tests.
const testCloudConfig = "#cloud-config\ncustom: true\n"

const testMemoryLimit = "4Gi"

const testNvidiaGPUResource = "nvidia.com/gpu"

func testWorkspace(wsType string, annotations map[string]string) *kubeworkspacesiov1alpha1.Workspace {
	return &kubeworkspacesiov1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testWorkspaceName,
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
	if dep.Spec.Selector.MatchLabels["deployment"] != testWorkspaceName {
		t.Errorf("unexpected selector: %v", dep.Spec.Selector.MatchLabels)
	}
	if dep.Spec.Template.Labels[LabelWorkspaceName] != testWorkspaceName {
		t.Errorf("pod template missing workspace-name label: %v", dep.Spec.Template.Labels)
	}

	stopped := generateDeployment(testWorkspace(WorkspaceTypeScratch, map[string]string{AnnotationStopped: "true"}))
	if *stopped.Spec.Replicas != 0 {
		t.Errorf("stopped workspace should have 0 replicas, got %d", *stopped.Spec.Replicas)
	}
}

func TestGenerateVirtualMachine(t *testing.T) {
	vm := generateVirtualMachine(testWorkspace(WorkspaceTypeVM, nil), nil, nil)

	if vm.GetName() != testWorkspaceName || vm.GetNamespace() != "workspaces" {
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
	if labels[LabelWorkspaceName] != testWorkspaceName {
		t.Errorf("VMI template missing workspace-name label: %v", labels)
	}
}

func TestGenerateVirtualMachineStopped(t *testing.T) {
	vm := generateVirtualMachine(testWorkspace(WorkspaceTypeVM, map[string]string{AnnotationStopped: "true"}), nil, nil)
	running, _, _ := unstructured.NestedBool(vm.Object, "spec", "running")
	if running {
		t.Error("expected spec.running=false for a stopped workspace")
	}
}

func TestGenerateVirtualMachineWithCloudInit(t *testing.T) {
	ws := testWorkspace(WorkspaceTypeVM, nil)
	const userData = "#cloud-config\npassword: secret\n"
	img := &kubeworkspacesiov1alpha1.Image{
		Spec: kubeworkspacesiov1alpha1.ImageSpec{
			Image:           "quay.io/containerdisks/fedora:latest",
			DefaultUserData: userData,
		},
	}
	vm := generateVirtualMachine(ws, img, nil)

	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes with cloud-init, got %d", len(volumes))
	}
	ci := volumes[1].(map[string]interface{})["cloudInitNoCloud"].(map[string]interface{})
	if got, ok := ci["userData"].(string); !ok || got != userData {
		t.Errorf("expected inline plaintext userData, got %v", ci)
	}

	disks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	if len(disks) != 2 {
		t.Fatalf("expected 2 disks with cloud-init, got %d", len(disks))
	}
	if disks[1].(map[string]interface{})["name"] != "cloudinitdisk" {
		t.Errorf("unexpected second disk: %v", disks[1])
	}
}

func TestGenerateVirtualMachineNoCloudInit(t *testing.T) {
	vm := generateVirtualMachine(testWorkspace(WorkspaceTypeVM, nil), nil, nil)
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if len(volumes) != 1 {
		t.Fatalf("expected 1 volume without cloud-init, got %d", len(volumes))
	}
}

func TestGenerateVirtualMachineVideoDevice(t *testing.T) {
	cases := []struct {
		name string
		img  *kubeworkspacesiov1alpha1.Image
		want string // expected domain.devices.video.type; "" means the video key is absent
	}{
		{"virtio explicit", &kubeworkspacesiov1alpha1.Image{
			Spec: kubeworkspacesiov1alpha1.ImageSpec{
				Image:       "quay.io/containerdisks/fedora:latest",
				VideoDevice: "virtio",
			},
		}, "virtio"},
		{"nil image", nil, ""},
		{"empty video device", &kubeworkspacesiov1alpha1.Image{
			Spec: kubeworkspacesiov1alpha1.ImageSpec{
				Image: "quay.io/containerdisks/fedora:latest",
			},
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := generateVirtualMachine(testWorkspace(WorkspaceTypeVM, nil), tc.img, nil)
			video, found, _ := unstructured.NestedMap(vm.Object, "spec", "template", "spec", "domain", "devices", "video")
			if tc.want == "" {
				if found {
					t.Errorf("expected no domain.devices.video, got %v", video)
				}
				return
			}
			if !found {
				t.Fatalf("expected domain.devices.video %q, key absent", tc.want)
			}
			if got, _ := video["type"].(string); got != tc.want {
				t.Errorf("expected video type %q, got %q", tc.want, got)
			}
		})
	}
}

// testWorkspaceWithGPU returns a VM workspace whose main container requests
// the given GPU resource name; count defaults to "1". The resource limit is
// added so the controller can discover it when building the domain gpus[].
func testWorkspaceWithGPU(wsType, gpuResource string) *kubeworkspacesiov1alpha1.Workspace {
	ws := testWorkspace(wsType, nil)
	ws.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceName(gpuResource)] = resource.MustParse("1")
	return ws
}

func TestGenerateVirtualMachineGPU(t *testing.T) {
	ws := testWorkspaceWithGPU(WorkspaceTypeVM, testNvidiaGPUResource)
	vm := generateVirtualMachine(ws, nil, nil)

	// The GPU resource must appear in domain.resources.limits (already copied
	// from the container) so KubeVirt has the scheduling resource.
	limits, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "spec", "domain", "resources", "limits")
	if limits[testNvidiaGPUResource] != "1" {
		t.Errorf("expected nvidia.com/gpu limit 1 in domain resources, got %v", limits)
	}

	// And it must be declared as a passthrough device under domain.devices.gpus.
	gpus, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "gpus")
	if len(gpus) != 1 {
		t.Fatalf("expected 1 gpu device, got %d", len(gpus))
	}
	gpu := gpus[0].(map[string]interface{})
	if gpu["deviceName"] != testNvidiaGPUResource {
		t.Errorf("unexpected gpu deviceName: %v", gpu["deviceName"])
	}
	if gpu["name"] != "gpu0" {
		t.Errorf("expected gpu slot name gpu0, got %v", gpu["name"])
	}
}

func TestGenerateVirtualMachineNoGPU(t *testing.T) {
	vm := generateVirtualMachine(testWorkspace(WorkspaceTypeVM, nil), nil, nil)
	gpus, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "gpus")
	if len(gpus) != 0 {
		t.Errorf("expected no gpus without a GPU request, got %d", len(gpus))
	}
}

func TestGenerateVirtualMachineScheduling(t *testing.T) {
	ws := testWorkspace(WorkspaceTypeVM, nil)
	ws.Spec.Template.Spec.NodeSelector = map[string]string{"nvidia.com/gpu.present": "true"}
	ws.Spec.Template.Spec.Tolerations = []corev1.Toleration{
		{Key: testNvidiaGPUResource, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}
	vm := generateVirtualMachine(ws, nil, nil)

	ns, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "spec", "nodeSelector")
	if ns["nvidia.com/gpu.present"] != "true" {
		t.Errorf("expected nodeSelector to be carried into the VMI template, got %v", ns)
	}

	tols, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "tolerations")
	if len(tols) != 1 {
		t.Fatalf("expected 1 toleration in the VMI template, got %d", len(tols))
	}
	tol := tols[0].(map[string]interface{})
	if tol["key"] != testNvidiaGPUResource || tol["effect"] != "NoSchedule" {
		t.Errorf("unexpected toleration in the VMI template: %v", tol)
	}
}

func TestGpuDevicesFromLimits(t *testing.T) {
	cases := []struct {
		name     string
		limits   corev1.ResourceList
		expected []string // deviceName order
	}{
		{"nvidia known resource", corev1.ResourceList{testNvidiaGPUResource: resource.MustParse("2")}, []string{testNvidiaGPUResource}},
		{"amd known resource", corev1.ResourceList{"amd.com/gpu": resource.MustParse("1")}, []string{"amd.com/gpu"}},
		{"custom /gpu suffix", corev1.ResourceList{"example.com/gpu": resource.MustParse("1")}, []string{"example.com/gpu"}},
		{"non-gpu ignored", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}, nil},
		{"gpu plus cpu", corev1.ResourceList{testNvidiaGPUResource: resource.MustParse("1"), corev1.ResourceCPU: resource.MustParse("2")}, []string{testNvidiaGPUResource}},
		{"multiple gpus sorted", corev1.ResourceList{"intel.com/gpu": resource.MustParse("1"), "amd.com/gpu": resource.MustParse("2"), testNvidiaGPUResource: resource.MustParse("1")}, []string{"amd.com/gpu", "intel.com/gpu", testNvidiaGPUResource}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gpuDevicesFromLimits(tc.limits)
			if len(got) != len(tc.expected) {
				t.Fatalf("expected %d gpus, got %d", len(tc.expected), len(got))
			}
			for i, g := range got {
				m := g.(map[string]interface{})
				if m["deviceName"] != tc.expected[i] {
					t.Errorf("device %d: expected deviceName %q, got %v", i, tc.expected[i], m["deviceName"])
				}
			}
		})
	}
}

func TestGenerateVirtualMachinePersistentRootDisk(t *testing.T) {
	ws := testWorkspace(WorkspaceTypeVM, nil)
	img := &kubeworkspacesiov1alpha1.Image{
		Spec: kubeworkspacesiov1alpha1.ImageSpec{
			Image:                  "quay.io/containerdisks/debian:12",
			PersistentRootDisk:     true,
			PersistentRootDiskSize: "20Gi",
			MemoryLimit:            testMemoryLimit,
			DefaultUserData:        "#cloud-config\npackages: [task-gnome-desktop]\n",
		},
	}
	vm := generateVirtualMachine(ws, img, nil)

	// No containerDisk; root is a dataVolume referencing the template.
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes (root dataVolume + cloud-init), got %d", len(volumes))
	}
	root := volumes[0].(map[string]interface{})
	if _, ok := root["containerDisk"]; ok {
		t.Error("root disk must not be a containerDisk when persistentRootDisk is set")
	}
	dv, ok := root["dataVolume"].(map[string]interface{})
	if !ok || dv["name"] != "rootdisk" {
		t.Errorf("root volume should reference the rootdisk DataVolume, got %v", root)
	}

	// dataVolumeTemplates carries the registry import and PVC sizing.
	templates, _, _ := unstructured.NestedSlice(vm.Object, "spec", "dataVolumeTemplates")
	if len(templates) != 1 {
		t.Fatalf("expected 1 dataVolumeTemplate, got %d", len(templates))
	}
	tmpl := templates[0].(map[string]interface{})
	if tmpl["metadata"].(map[string]interface{})["name"] != "rootdisk" {
		t.Fatalf("unexpected dataVolumeTemplate metadata: %v", tmpl["metadata"])
	}
	url, _, _ := unstructured.NestedString(tmpl, "spec", "source", "registry", "url")
	if url != "docker://quay.io/containerdisks/fedora:latest" {
		t.Errorf("unexpected registry source URL: %q", url)
	}
	size, _, _ := unstructured.NestedString(tmpl, "spec", "pvc", "resources", "requests", "storage")
	if size != "20Gi" {
		t.Errorf("unexpected PVC size: %q", size)
	}

	// Memory override is applied to both limits and requests (QoS).
	limits, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "spec", "domain", "resources", "limits")
	if limits["memory"] != testMemoryLimit {
		t.Errorf("expected memory limit 4Gi, got %v", limits)
	}
	reqs, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "spec", "domain", "resources", "requests")
	if reqs["memory"] != testMemoryLimit {
		t.Errorf("expected memory request 4Gi, got %v", reqs)
	}
}

func TestGenerateVirtualMachineMemoryOverrideWithoutPersistentRoot(t *testing.T) {
	ws := testWorkspace(WorkspaceTypeVM, nil)
	img := &kubeworkspacesiov1alpha1.Image{
		Spec: kubeworkspacesiov1alpha1.ImageSpec{
			Image:       "quay.io/containerdisks/fedora:latest",
			MemoryLimit: "2Gi",
		},
	}
	vm := generateVirtualMachine(ws, img, nil)

	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	root := volumes[0].(map[string]interface{})
	if _, ok := root["containerDisk"]; !ok {
		t.Error("root disk should remain a containerDisk when persistentRootDisk is unset")
	}
	if _, found, _ := unstructured.NestedSlice(vm.Object, "spec", "dataVolumeTemplates"); found {
		t.Error("no dataVolumeTemplates expected without persistentRootDisk")
	}
	limits, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "spec", "domain", "resources", "limits")
	if limits["memory"] != "2Gi" {
		t.Errorf("expected memory limit 2Gi, got %v", limits)
	}
	reqs, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "spec", "domain", "resources", "requests")
	if reqs["memory"] != "2Gi" {
		t.Errorf("expected memory request 2Gi, got %v", reqs)
	}
}

func TestGenerateVirtualMachineMemoryRequestDecouplesPodFromGuest(t *testing.T) {
	ws := testWorkspace(WorkspaceTypeVM, nil)
	img := &kubeworkspacesiov1alpha1.Image{
		Spec: kubeworkspacesiov1alpha1.ImageSpec{
			Image:              "quay.io/containerdisks/debian:13",
			PersistentRootDisk: true,
			MemoryLimit:        testMemoryLimit,
			MemoryRequest:      "5Gi",
		},
	}
	vm := generateVirtualMachine(ws, img, nil)

	// Pod allocation (both request and limit) reflects MemoryRequest...
	limits, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "spec", "domain", "resources", "limits")
	if limits["memory"] != "5Gi" {
		t.Errorf("expected pod memory limit 5Gi, got %v", limits)
	}
	reqs, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "spec", "domain", "resources", "requests")
	if reqs["memory"] != "5Gi" {
		t.Errorf("expected pod memory request 5Gi, got %v", reqs)
	}
	// ...while the guest RAM stays pinned to MemoryLimit.
	guest, _, _ := unstructured.NestedString(vm.Object, "spec", "template", "spec", "domain", "memory", "guest")
	if guest != testMemoryLimit {
		t.Errorf("expected guest memory 4Gi, got %q", guest)
	}
}

func TestCloudInitUserData(t *testing.T) {
	nilImage := cloudInitUserData(nil, nil)
	if nilImage != "" {
		t.Errorf("expected empty user-data for nil image, got %q", nilImage)
	}

	image := &kubeworkspacesiov1alpha1.Image{
		Spec: kubeworkspacesiov1alpha1.ImageSpec{
			Image: "quay.io/containerdisks/debian:12",
		},
	}
	if ud := cloudInitUserData(image, nil); ud != "" {
		t.Errorf("expected empty user-data without cloud-init opt-in, got %q", ud)
	}

	image.Spec.DefaultCloudInit = true
	image.Spec.DefaultUser = RootUser
	image.Spec.DefaultPassword = "debian"
	if ud := cloudInitUserData(image, nil); ud == "" {
		t.Error("expected generated user-data when cloud-init defaults are set")
	} else if !strings.Contains(ud, "list: |\n    root:debian") {
		t.Errorf("expected root:debian password in generated user-data, got %q", ud)
	}

	image.Spec.DefaultUserData = testCloudConfig
	if ud := cloudInitUserData(image, nil); ud != testCloudConfig {
		t.Errorf("explicit DefaultUserData should win, got %q", ud)
	}

	partial := &kubeworkspacesiov1alpha1.Image{}
	partial.Spec.DefaultCloudInit = true
	partial.Spec.DefaultUser = RootUser
	if ud := cloudInitUserData(partial, nil); ud != "" {
		t.Errorf("expected empty user-data when password is missing, got %q", ud)
	}
}

func TestInjectSSHAuthorizedKeys(t *testing.T) {
	const k1 = "ssh-ed25519 AAAAZm9vYmFy user1@host"
	const k2 = "ssh-rsa AAAAcmNkc2E= user2@host"

	if ud := injectSSHAuthorizedKeys("", nil, ""); ud != "" {
		t.Errorf("no keys => unchanged, got %q", ud)
	}
	if ud := injectSSHAuthorizedKeys(testCloudConfig, nil, ""); ud != testCloudConfig {
		t.Errorf("no keys => unchanged existing data, got %q", ud)
	}

	// Keys only, no existing data: emit a minimal cloud-config.
	ud := injectSSHAuthorizedKeys("", []string{k1}, "")
	if !strings.Contains(ud, "#cloud-config") || !strings.Contains(ud, "ssh_authorized_keys") || !strings.Contains(ud, k1) {
		t.Errorf("expected minimal cloud-config with key, got %q", ud)
	}
	if strings.Contains(ud, "runcmd") {
		t.Errorf("no default user => no runcmd re-seeding, got %q", ud)
	}

	// Merge into existing generated user-data (password config preserved) and
	// emit a reboot-safe runcmd entry carrying base64-encoded keys.
	image := &kubeworkspacesiov1alpha1.Image{}
	image.Spec.DefaultCloudInit = true
	image.Spec.DefaultUser = "debian"
	image.Spec.DefaultPassword = "secret"
	full := cloudInitUserData(image, []string{k1, k2})
	wantBlob := base64.StdEncoding.EncodeToString([]byte(k1 + "\n" + k2))
	for _, want := range []string{"#cloud-config", "debian:secret", "ssh_authorized_keys", k1, k2, "runcmd", wantBlob, "/home/debian/.ssh/authorized_keys"} {
		if !strings.Contains(full, want) {
			t.Errorf("expected user-data to contain %q, got %q", want, full)
		}
	}
	if strings.Count(full, wantBlob) != 1 {
		t.Errorf("expected a single runcmd entry, got %q", full)
	}

	// Seed for the root account targets /root/.ssh.
	root := injectSSHAuthorizedKeys("", []string{k1}, RootUser)
	if !strings.Contains(root, "/root/.ssh/authorized_keys") {
		t.Errorf("expected root account key seeding, got %q", root)
	}

	// Merge into explicit DefaultUserData (the baked debian-gnome case).
	baked := "#cloud-config\nssh_pwauth: true\npackages:\n  - htop\n"
	merged := injectSSHAuthorizedKeys(baked, []string{k1, k1}, "debian")
	if !strings.Contains(merged, "ssh_authorized_keys") || !strings.Contains(merged, k1) {
		t.Errorf("expected keys merged into baked user-data, got %q", merged)
	}
	if !strings.Contains(merged, "runcmd") || !strings.Contains(merged, "/home/debian/.ssh/authorized_keys") {
		t.Errorf("expected runcmd re-seeding in baked user-data, got %q", merged)
	}
	if !strings.Contains(merged, "htop") {
		t.Errorf("expected packages list preserved in merged user-data, got %q", merged)
	}

	// Re-injecting the same key set is a no-op (runcmd and keys deduplicated).
	again := injectSSHAuthorizedKeys(merged, []string{k1}, "debian")
	if again != merged {
		t.Errorf("expected idempotent re-injection, got %q", again)
	}

	// Dedupe: k1 twice yields one entry.
	dedup := injectSSHAuthorizedKeys("", []string{k1, k1}, "")
	if strings.Count(dedup, k1) != 1 {
		t.Errorf("expected deduplicated keys, got %q", dedup)
	}

	// Invalid existing YAML is left alone rather than corrupted.
	broken := injectSSHAuthorizedKeys("#cloud-config\n: :\n  -", []string{k1}, "debian")
	if !strings.Contains(broken, ": :") {
		t.Errorf("expected invalid user-data left as-is, got %q", broken)
	}
}

func TestMacAddressForWorkspace(t *testing.T) {
	a := macAddressForWorkspace("cf-debian-gnome-vm-0")
	b := macAddressForWorkspace("cf-debian-gnome-vm-0")
	if a != b {
		t.Errorf("expected deterministic MAC for a workspace, got %q vs %q", a, b)
	}
	other := macAddressForWorkspace("cf-debian-vm-0")
	if a == other {
		t.Errorf("expected distinct MACs for different workspaces, got %q", a)
	}
	octet, err := strconv.ParseUint(a[:2], 16, 8)
	if err != nil || octet&0x01 != 0 || octet&0x02 != 0x02 {
		t.Errorf("expected locally-administered unicast MAC (first octet 0x02..0xfe even), got %q", a)
	}
	for _, wantColonCount := range []int{5} {
		if got := strings.Count(a, ":"); got != wantColonCount {
			t.Errorf("expected MAC with 5 colons, got %q (%d)", a, got)
		}
	}
}

func TestGenerateServiceSelectors(t *testing.T) {
	ws := testWorkspace(WorkspaceTypeContainer, nil)

	containerSvc := generateService(ws, WorkspaceTypeContainer)
	if containerSvc.Spec.Selector["statefulset"] != testWorkspaceName {
		t.Errorf("container service should select statefulset label: %v", containerSvc.Spec.Selector)
	}

	for _, wsType := range []string{WorkspaceTypeVM, WorkspaceTypeScratch} {
		svc := generateService(ws, wsType)
		if svc.Spec.Selector[LabelWorkspaceName] != testWorkspaceName {
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
