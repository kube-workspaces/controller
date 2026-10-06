package controller

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"testing"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func windowsTestWorkspace() *workspacev1.Workspace {
	image := "registry.example/private/windows@sha256:" + strings.Repeat("a", 64)
	return &workspacev1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "windows-test", Namespace: "test", UID: types.UID("workspace-uid")}, Spec: workspacev1.WorkspaceSpec{Type: WorkspaceTypeVM, VMProfile: &workspacev1.ResolvedVMProfile{ID: workspacev1.Windows11AMD64V1, Image: image, Generation: "generation-one", RootDiskSize: "80Gi", CPUCores: 2, GuestMemory: "4Gi", ImportSecretName: "private-import"}, Template: workspacev1.WorkspaceTemplateSpec{Spec: corev1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/hostname": "worker"}, Containers: []corev1.Container{{Name: "workspace", Image: image}}}}}}
}

type fakeWindowsAgent struct{ complete bool }

func (a *fakeWindowsAgent) CompleteBootstrap(context.Context, string, string, string, string) (bool, error) {
	return a.complete, nil
}

func TestWindowsNativeSetupAndPhysicalBootstrapRemoval(t *testing.T) {
	ctx := context.Background()
	scheme := resetTestScheme(t)
	ws := windowsTestWorkspace()
	ws.Spec.VMProfile.ImportSecretName = ""
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "launcher", Namespace: ws.Namespace, UID: types.UID("active-pod")}}
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(kubeVirtVirtualMachineInstanceGVK)
	vmi.SetName(ws.Name)
	vmi.SetNamespace(ws.Namespace)
	_ = unstructured.SetNestedStringMap(vmi.Object, map[string]string{string(pod.UID): "worker"}, "status", "activePods")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws, vmi).Build()
	agent := &fakeWindowsAgent{}
	r := &WorkspaceReconciler{Client: c, Scheme: scheme, GuestAgent: agent}
	if err := r.reconcileWindowsBootstrap(ctx, ws); err != nil {
		t.Fatal(err)
	}
	name := types.NamespacedName{Name: windowsBootstrapSecretName(ws), Namespace: ws.Namespace}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, name, secret); err != nil {
		t.Fatal(err)
	}
	password := string(secret.Data["password"])
	ready, err := r.reconcileWindowsProvisioning(ctx, ws, pod)
	if err != nil || ready || ws.Annotations[windowsProvisionedGeneration] != "" {
		t.Fatal("agent connection incorrectly established native setup completion")
	}
	agent.complete = true
	ready, err = r.reconcileWindowsProvisioning(ctx, ws, pod)
	if err != nil || ready {
		t.Fatalf("must detach media before readiness: %v", err)
	}
	vm := generateWindowsVirtualMachine(ws)
	running, _, _ := unstructured.NestedBool(vm.Object, "spec", "running")
	if running {
		t.Fatal("bootstrap removal must request graceful shutdown")
	}
	if err := r.finishWindowsDetach(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if ws.Annotations[windowsDetachingGeneration] == "" {
		t.Fatal("detachment finished while VMI was active")
	}
	if err := c.Delete(ctx, vmi); err != nil {
		t.Fatal(err)
	}
	if err := r.finishWindowsDetach(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, name, secret); err != nil {
		t.Fatal(err)
	}
	if len(secret.Data["autounattend.xml"]) != 0 || string(secret.Data["password"]) != password {
		t.Fatal("cleanup retained answer media or changed initial credentials")
	}
	if err := r.reconcileWindowsBootstrap(ctx, ws); err != nil {
		t.Fatal(err)
	}
	vm = generateWindowsVirtualMachine(ws)
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if len(volumes) != 1 {
		t.Fatal("ordinary restart reattached bootstrap media")
	}
	if err := c.Delete(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileWindowsBootstrap(ctx, ws); err == nil {
		t.Fatal("missing credentials silently regenerated after guest provisioning")
	}
}

func TestWindowsRejectsLookalikeLauncherAndWaitsForGracefulReboot(t *testing.T) {
	ctx := context.Background()
	ws := windowsTestWorkspace()
	ws.Annotations = map[string]string{windowsRebootAnnotation: "request"}
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(kubeVirtVirtualMachineInstanceGVK)
	vmi.SetName(ws.Name)
	vmi.SetNamespace(ws.Namespace)
	c := fake.NewClientBuilder().WithScheme(resetTestScheme(t)).WithObjects(ws, vmi).Build()
	r := &WorkspaceReconciler{Client: c, GuestAgent: &fakeWindowsAgent{complete: true}}
	if _, err := r.reconcileWindowsProvisioning(ctx, ws, &corev1.Pod{}); err == nil {
		t.Fatal("accepted a pod outside VMI activePods")
	}
	if err := r.finishWindowsReboot(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if ws.Annotations[windowsRebootAnnotation] == "" {
		t.Fatal("restart request consumed before VMI disappeared")
	}
	if err := c.Delete(ctx, vmi); err != nil {
		t.Fatal(err)
	}
	if err := r.finishWindowsReboot(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if ws.Annotations[windowsRebootAnnotation] != "" {
		t.Fatal("restart request not consumed after shutdown")
	}
}

func TestWindowsHardwareAndSourceIdentity(t *testing.T) {
	ws := windowsTestWorkspace()
	ws.Spec.VMProfile.ImportCertConfigMapName = "private-ca"
	if err := validateWindowsProfile(ws); err != nil {
		t.Fatal(err)
	}
	vm := generateVirtualMachine(ws, &workspacev1.Image{Spec: workspacev1.ImageSpec{DefaultCloudInit: true, DefaultUser: "linux", DefaultUserData: "linux-data", VideoDevice: "virtio"}}, []string{"linux-key"})
	for _, path := range [][]string{{"spec", "template", "spec", "domain", "firmware", "bootloader", "efi", "secureBoot"}, {"spec", "template", "spec", "domain", "firmware", "bootloader", "efi", "persistent"}, {"spec", "template", "spec", "domain", "devices", "tpm", "persistent"}, {"spec", "template", "spec", "domain", "features", "smm", "enabled"}} {
		value, _, _ := unstructured.NestedBool(vm.Object, path...)
		if !value {
			t.Errorf("required Windows hardware missing at %v", path)
		}
	}
	text := fmt.Sprint(vm.Object)
	if strings.Contains(text, "cloudInit") || strings.Contains(text, "accessCredentials") || strings.Contains(text, "linux-key") {
		t.Fatal("Linux provisioning leaked into Windows")
	}
	templates, _, _ := unstructured.NestedSlice(vm.Object, "spec", "dataVolumeTemplates")
	source := templates[0].(map[string]interface{})["spec"].(map[string]interface{})["source"].(map[string]interface{})["registry"].(map[string]interface{})
	if source["secretRef"] != "private-import" || source["certConfigMap"] != "private-ca" || source["pullMethod"] != "pod" || source["url"] != "docker://"+ws.Spec.VMProfile.Image {
		t.Fatalf("wrong private root import: %#v", source)
	}
	before := windowsIdentity(ws)
	if windowsIdentity(ws.DeepCopy()) != before {
		t.Fatal("ordinary reconcile changed firmware identity")
	}
	ws.Spec.VMProfile.Generation = "generation-two"
	if windowsIdentity(ws) == before {
		t.Fatal("Reset generation retained firmware identity")
	}
	if len(windowsHostname(ws)) > 15 {
		t.Fatal("Windows hostname exceeds its native limit")
	}
}

func TestWindowsImportCredentialsUseCDIPodKeys(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := workspacev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, valid := range []bool{false, true} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			ws := windowsTestWorkspace()
			credentials := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "private-import", Namespace: ws.Namespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{".dockerconfigjson": []byte(`{"auths":{}}`)}}
			if valid {
				credentials.Data = map[string][]byte{"accessKeyId": []byte("operator"), "secretKey": []byte("private-token")}
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws, credentials).Build()
			r := WorkspaceReconciler{Client: c, Scheme: scheme}
			err := r.reconcileWindowsBootstrap(context.Background(), ws)
			if valid && err != nil {
				t.Fatal(err)
			}
			if !valid && err == nil {
				t.Fatal("Docker pull Secret accepted despite missing CDI pod-import keys")
			}
		})
	}
}

func TestWindowsProfileRejectsLinuxVolumesAndInvalidResources(t *testing.T) {
	cases := []func(*workspacev1.Workspace){
		func(ws *workspacev1.Workspace) { ws.Spec.VMProfile.GuestMemory = DefaultGuestMemory },
		func(ws *workspacev1.Workspace) { ws.Spec.VMProfile.CPUCores = 1 },
		func(ws *workspacev1.Workspace) { ws.Spec.VMProfile.Image = "registry.example/windows:latest" },
		func(ws *workspacev1.Workspace) {
			ws.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/home"}}
		},
		func(ws *workspacev1.Workspace) { ws.Spec.Template.Spec.NodeSelector["kubernetes.io/arch"] = "arm64" },
	}
	for i, change := range cases {
		ws := windowsTestWorkspace()
		change(ws)
		if validateWindowsProfile(ws) == nil {
			t.Errorf("invalid Windows case %d accepted", i)
		}
	}
}

func TestWindowsBootstrapStableAndSecretOwned(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := workspacev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ws := windowsTestWorkspace()
	ws.Spec.VMProfile.ImportSecretName = ""
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws).Build()
	reconciler := WorkspaceReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()
	if err := reconciler.reconcileWindowsBootstrap(ctx, ws); err != nil {
		t.Fatal(err)
	}
	first := &corev1.Secret{}
	key := types.NamespacedName{Name: windowsBootstrapSecretName(ws), Namespace: ws.Namespace}
	if err := c.Get(ctx, key, first); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(first, ws) {
		t.Fatal("bootstrap not owned by workspace")
	}
	if err := reconciler.reconcileWindowsBootstrap(ctx, ws); err != nil {
		t.Fatal(err)
	}
	second := &corev1.Secret{}
	if err := c.Get(ctx, key, second); err != nil {
		t.Fatal(err)
	}
	if string(first.Data["password"]) != string(second.Data["password"]) {
		t.Fatal("credentials rotated on reconcile")
	}
	ws.Spec.VMProfile.Generation = "fresh-generation"
	if err := reconciler.reconcileWindowsBootstrap(ctx, ws); err != nil {
		t.Fatal(err)
	}
	fresh := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: windowsBootstrapSecretName(ws), Namespace: ws.Namespace}, fresh); err != nil {
		t.Fatal(err)
	}
	if string(fresh.Data["password"]) == string(first.Data["password"]) {
		t.Fatal("new generation reused credentials")
	}
}

func TestWindowsAnswerFileEscapingAndNoDiskWipe(t *testing.T) {
	answer, err := windowsAnswerFile("KW-TEST", "workspace", `a&<>"password`)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Settings []struct {
			Components []struct {
				Accounts struct {
					Locals struct {
						Local struct {
							Password struct {
								Value string `xml:"Value"`
							} `xml:"Password"`
						} `xml:"LocalAccount"`
					} `xml:"LocalAccounts"`
				} `xml:"UserAccounts"`
			} `xml:"component"`
		} `xml:"settings"`
	}
	if err := xml.Unmarshal([]byte(answer), &parsed); err != nil {
		t.Fatal(err)
	}
	value := parsed.Settings[1].Components[1].Accounts.Locals.Local.Password.Value
	if value != `a&<>"password` {
		t.Fatal("password XML was not safely escaped")
	}
	if strings.Contains(answer, "DiskConfiguration") || strings.Contains(answer, "AutoLogon") {
		t.Fatal("clone bootstrap can wipe disks or automatically log in")
	}
}
