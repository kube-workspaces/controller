package controller

import (
	"strings"
	"testing"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestHTTPRootImportKeepsCloudInitAndIsolatedDisks(t *testing.T) {
	const ref = "https://images.example.org/arch/cloud.qcow2"
	img := &workspacev1.Image{Spec: workspacev1.ImageSpec{
		Image: ref, WorkspaceTypes: []string{"vm"}, PersistentRootDisk: true,
		PersistentRootDiskSize: "30Gi", DefaultCloudInit: true, DefaultUser: "arch",
		DefaultUserData: "#cloud-config\npackages: [gnome]\n",
	}}
	for _, name := range []string{"arch", "arch-clone"} {
		ws := testWorkspace(WorkspaceTypeVM, nil)
		ws.Name = name
		ws.Spec.Template.Spec.Containers[0].Image = ref
		if err := validateVMRootImage(ws, img); err != nil {
			t.Fatal(err)
		}
		vm := generateVirtualMachine(ws, img, []string{"ssh-ed25519 test"}, 44787)
		templates, _, _ := unstructured.NestedSlice(vm.Object, "spec", "dataVolumeTemplates")
		if len(templates) != 1 {
			t.Fatalf("expected one root import, got %v", templates)
		}
		tmpl := templates[0].(map[string]interface{})
		source, _, _ := unstructured.NestedMap(tmpl, "spec", "source")
		url, _, _ := unstructured.NestedString(source, "http", "url")
		if len(source) != 1 || url != ref {
			t.Fatalf("expected an HTTP import without registry wrapping, got %v", source)
		}
		diskName, _, _ := unstructured.NestedString(tmpl, "metadata", "name")
		size, _, _ := unstructured.NestedString(tmpl, "spec", "pvc", "resources", "requests", "storage")
		if diskName != name+"-rootdisk" || size != "30Gi" {
			t.Fatalf("incorrect disk identity/size: %v", tmpl)
		}
		volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
		if len(volumes) != 2 {
			t.Fatalf("expected root and NoCloud volumes, got %v", volumes)
		}
		data, _, _ := unstructured.NestedString(volumes[1].(map[string]interface{}), "cloudInitNoCloud", "userData")
		if !strings.Contains(data, "gnome") || !strings.Contains(data, "ssh-ed25519 test") {
			t.Fatalf("cloud-init packages or key injection lost: %s", data)
		}
		if virtualMachineNeedsUpdate(vm, vm.DeepCopy()) {
			t.Fatal("unchanged HTTP source must not cause an update loop")
		}
	}
}

func TestVMCloudImageValidation(t *testing.T) {
	for _, tt := range []struct {
		name, ref string
		img       *workspacev1.Image
		wantError bool
	}{
		{"legacy containerdisk", "quay.io/containerdisks/debian:13", nil, false},
		{"HTTP without catalog", "https://images.example.org/cloud.qcow2", nil, true},
		{"ephemeral HTTP", "https://images.example.org/cloud.qcow2", &workspacev1.Image{Spec: workspacev1.ImageSpec{WorkspaceTypes: []string{"vm"}}}, true},
		{"non-VM catalog", "https://images.example.org/cloud.qcow2", &workspacev1.Image{Spec: workspacev1.ImageSpec{PersistentRootDisk: true, WorkspaceTypes: []string{"vm", "container"}}}, true},
		{"valid HTTPS", "https://images.example.org/cloud.qcow2", httpRootImage(), false},
		{"valid HTTP", "http://images.example.org/cloud.qcow2", httpRootImage(), false},
		{"missing host", "https:///cloud.qcow2", httpRootImage(), true},
		{"credentials", "https://user:password@images.example.org/cloud.qcow2", httpRootImage(), true},
		{"fragment", "https://images.example.org/cloud.qcow2#fragment", httpRootImage(), true},
		{"unsupported scheme", "ftp://images.example.org/cloud.qcow2", httpRootImage(), true},
		{"whitespace", "https://images.example.org/cloud image.qcow2", httpRootImage(), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ws := testWorkspace(WorkspaceTypeVM, nil)
			ws.Spec.Template.Spec.Containers[0].Image = tt.ref
			if err := validateVMRootImage(ws, tt.img); (err != nil) != tt.wantError {
				t.Fatalf("validation error = %v, wantError = %v", err, tt.wantError)
			}
		})
	}
}

func httpRootImage() *workspacev1.Image {
	return &workspacev1.Image{Spec: workspacev1.ImageSpec{PersistentRootDisk: true, WorkspaceTypes: []string{"vm"}}}
}
