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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utiluuid "k8s.io/apimachinery/pkg/util/uuid"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const windowsProvisionedGeneration = "kubeworkspaces.io/windows-provisioned-generation"
const windowsDetachingGeneration = "kubeworkspaces.io/windows-detaching-generation"
const windowsRebootAnnotation = "kubeworkspaces.io/reboot"
const windowsEligibleNodeLabel = "kubeworkspaces.io/windows11-amd64-eligible"
const windowsGuestArchitecture = "amd64"

var windowsImageDigest = regexp.MustCompile(`^.+@sha256:[a-f0-9]{64}$`)

func windowsWorkspace(instance *workspacev1.Workspace) bool {
	return instance.Spec.VMProfile != nil && instance.Spec.VMProfile.ID == workspacev1.Windows11AMD64V1
}

// Eligibility is explicitly operator-certified for the tested Windows build;
// KubeVirt's compatibility CPU-model labels are not host eligibility evidence.
func (r *WorkspaceReconciler) preflightWindows(ctx context.Context, instance *workspacev1.Workspace) error {
	if err := validateWindowsProfile(instance); err != nil {
		return err
	}
	if err := r.preflightWindowsNode(ctx, instance); err != nil {
		return err
	}
	return r.preflightWindowsInfrastructure(ctx, instance)
}

func (r *WorkspaceReconciler) preflightWindowsNode(ctx context.Context, instance *workspacev1.Workspace) error {
	nodeName := instance.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
	if nodeName == "" {
		return fmt.Errorf("windows workspaces require a certified worker selector so root and state storage bind together")
	}
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes, client.MatchingLabels{"kubernetes.io/hostname": nodeName}); err != nil {
		return fmt.Errorf("windows worker inventory is unavailable")
	}
	if len(nodes.Items) != 1 {
		return fmt.Errorf("windows worker selector must resolve to one certified worker")
	}
	node := nodes.Items[0]
	if node.Spec.Unschedulable || node.Labels[windowsEligibleNodeLabel] != LabelValueTrue || node.Labels["kubernetes.io/arch"] != windowsGuestArchitecture {
		return fmt.Errorf("windows worker is not schedulable/certified for the selected amd64 profile")
	}
	kvm := node.Status.Allocatable[corev1.ResourceName("devices.kubevirt.io/kvm")]
	if kvm.IsZero() {
		return fmt.Errorf("windows worker does not advertise KVM acceleration")
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		return fmt.Errorf("windows worker is not Ready")
	}
	return nil
}

func (r *WorkspaceReconciler) preflightWindowsInfrastructure(ctx context.Context, instance *workspacev1.Workspace) error {
	kubevirts := &unstructured.UnstructuredList{}
	kubevirts.SetGroupVersionKind(kubeVirtVirtualMachineGVK.GroupVersion().WithKind("KubeVirtList"))
	if err := r.List(ctx, kubevirts); err != nil || len(kubevirts.Items) != 1 {
		return fmt.Errorf("windows requires an available KubeVirt installation")
	}
	kv := kubevirts.Items[0]
	version, _, _ := unstructured.NestedString(kv.Object, "status", "observedKubeVirtVersion")
	phase, _, _ := unstructured.NestedString(kv.Object, "status", "phase")
	if !strings.HasPrefix(version, "v1.9.") || phase != "Deployed" {
		return fmt.Errorf("windows profile requires deployed KubeVirt 1.9.x persistent-state support")
	}
	emulation, _, _ := unstructured.NestedBool(kv.Object, "spec", "configuration", "developerConfiguration", "useEmulation")
	if emulation {
		return fmt.Errorf("windows profile requires KVM, not software emulation")
	}
	volumes := &unstructured.UnstructuredList{}
	volumes.SetGroupVersionKind(schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolumeList"})
	if err := r.List(ctx, volumes, client.InNamespace(instance.Namespace)); err != nil {
		return fmt.Errorf("windows persistent roots require CDI")
	}
	classes := &storagev1.StorageClassList{}
	if err := r.List(ctx, classes); err != nil {
		return fmt.Errorf("windows persistent-state storage inventory is unavailable")
	}
	stateClass, _, _ := unstructured.NestedString(kv.Object, "spec", "configuration", "vmStateStorageClass")
	explicit := stateClass != ""
	if stateClass == "" {
		for _, annotation := range []string{"storageclass.kubevirt.io/is-default-virt-class", "storageclass.kubernetes.io/is-default-class"} {
			matches := []string{}
			for _, sc := range classes.Items {
				if sc.Annotations[annotation] == LabelValueTrue {
					matches = append(matches, sc.Name)
				}
			}
			if len(matches) > 1 {
				return fmt.Errorf("windows backend-state StorageClass is ambiguous")
			}
			if len(matches) == 1 {
				stateClass = matches[0]
				break
			}
		}
	}
	var selected *storagev1.StorageClass
	for i := range classes.Items {
		if classes.Items[i].Name == stateClass {
			selected = &classes.Items[i]
		}
	}
	if selected == nil {
		return fmt.Errorf("windows backend-state StorageClass is missing")
	}
	if explicit && strings.HasSuffix(selected.Provisioner, "/local-path") {
		profile := &unstructured.Unstructured{}
		profile.SetGroupVersionKind(schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "StorageProfile"})
		if err := r.Get(ctx, types.NamespacedName{Name: stateClass}, profile); err != nil {
			return fmt.Errorf("explicit local backend-state class requires a Filesystem/RWO StorageProfile")
		}
		properties, _, _ := unstructured.NestedSlice(profile.Object, "status", "claimPropertySets")
		rwo := false
		for _, item := range properties {
			property, ok := item.(map[string]interface{})
			if !ok || property["volumeMode"] != "Filesystem" {
				continue
			}
			modes, _, _ := unstructured.NestedStringSlice(property, "accessModes")
			for _, mode := range modes {
				if mode == "ReadWriteMany" {
					return fmt.Errorf("local backend-state storage cannot satisfy RWX")
				}
				if mode == "ReadWriteOnce" {
					rwo = true
				}
			}
		}
		if !rwo {
			return fmt.Errorf("explicit local backend-state class requires Filesystem/RWO defaults")
		}
	}
	return nil
}

func validateWindowsProfile(instance *workspacev1.Workspace) error {
	profile := instance.Spec.VMProfile
	if profile == nil || profile.ID != workspacev1.Windows11AMD64V1 {
		return fmt.Errorf("unsupported VM guest profile")
	}
	if instance.Spec.Type != WorkspaceTypeVM {
		return fmt.Errorf("windows profile requires a VM workspace")
	}
	containers := instance.Spec.Template.Spec.Containers
	if len(containers) != 1 || containers[0].Image != profile.Image || !windowsImageDigest.MatchString(profile.Image) {
		return fmt.Errorf("windows profile requires one main container with the resolved OCI image digest")
	}
	if len(containers[0].VolumeMounts) != 0 || len(instance.Spec.Template.Spec.Volumes) != 0 {
		return fmt.Errorf("windows automatic user-disk mounting is not supported")
	}
	if profile.CPUCores < 2 {
		return fmt.Errorf("windows 11 requires at least 2 guest CPU cores")
	}
	memory, err := resource.ParseQuantity(profile.GuestMemory)
	if err != nil || memory.Cmp(resource.MustParse("4Gi")) < 0 {
		return fmt.Errorf("windows 11 requires at least 4Gi guest memory")
	}
	disk, err := resource.ParseQuantity(profile.RootDiskSize)
	if err != nil || disk.Cmp(resource.MustParse("80Gi")) < 0 {
		return fmt.Errorf("windows prepared roots require at least 80Gi persistent storage")
	}
	if profile.Generation == "" {
		return fmt.Errorf("windows provisioning generation is missing")
	}
	if arch := instance.Spec.Template.Spec.NodeSelector["kubernetes.io/arch"]; arch != "" && arch != windowsGuestArchitecture {
		return fmt.Errorf("windows 11 profile requires amd64 scheduling")
	}
	return nil
}

// resolveWindowsProfile runs only before the first VM is created. Catalog data
// is deliberately not consulted once a workspace has its resolved snapshot.
func (r *WorkspaceReconciler) resolveWindowsProfile(ctx context.Context, instance *workspacev1.Workspace, img *workspacev1.Image) (bool, error) {
	if instance.Spec.VMProfile != nil {
		if instance.Spec.VMProfile.Generation == "" {
			instance.Spec.VMProfile.Generation = string(utiluuid.NewUUID())
			return true, r.Update(ctx, instance)
		}
		return false, validateWindowsProfile(instance)
	}
	if img == nil || img.Spec.VMProfile == "" {
		return false, nil
	}
	if img.Spec.VMProfile != workspacev1.Windows11AMD64V1 {
		return false, fmt.Errorf("unsupported Image VM profile")
	}
	if !img.Spec.PersistentRootDisk || img.Spec.DefaultCloudInit || img.Spec.DefaultUserData != "" || img.Spec.RemoteDesktop != nil || img.Spec.DefaultPassword != "" {
		return false, fmt.Errorf("windows images require persistent generalised roots, native Sysprep and no shared password or Linux guest streaming")
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(kubeVirtVirtualMachineGVK)
	err := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, existing)
	if err == nil {
		return false, fmt.Errorf("an existing legacy VM cannot change guest profile without an explicit Reset")
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	profile := &workspacev1.ResolvedVMProfile{ID: img.Spec.VMProfile, Image: img.Spec.Image, Generation: string(utiluuid.NewUUID()), RootDiskSize: img.Spec.PersistentRootDiskSize, CPUCores: 4, GuestMemory: "8Gi"}
	if profile.RootDiskSize == "" {
		profile.RootDiskSize = "80Gi"
	}
	if len(instance.Spec.Template.Spec.Containers) == 1 {
		limits := instance.Spec.Template.Spec.Containers[0].Resources.Limits
		if cpu, ok := limits[corev1.ResourceCPU]; ok {
			profile.CPUCores = int32(cpu.Value())
		}
		if memory, ok := limits[corev1.ResourceMemory]; ok {
			profile.GuestMemory = memory.String()
		}
	}
	// Pod imagePullSecrets have a different key contract from CDI 1.61 pod
	// imports. Import credentials are supplied explicitly in the resolved profile.
	instance.Spec.VMProfile = profile
	if err := validateWindowsProfile(instance); err != nil {
		return false, err
	}
	return true, r.Update(ctx, instance)
}

func windowsIdentity(instance *workspacev1.Workspace) string {
	sum := sha256.Sum256([]byte(string(instance.UID) + "/" + instance.Spec.VMProfile.Generation))
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func windowsHostname(instance *workspacev1.Workspace) string {
	sum := sha256.Sum256([]byte(windowsIdentity(instance)))
	return fmt.Sprintf("KW-%X", sum[:6])
}

func windowsBootstrapSecretName(instance *workspacev1.Workspace) string {
	sum := sha256.Sum256([]byte(instance.Name + "/" + instance.Spec.VMProfile.Generation))
	name := instance.Name
	if len(name) > 36 {
		name = strings.TrimRight(name[:36], "-")
	}
	return fmt.Sprintf("%s-win-%x", name, sum[:6])
}

// windowsAnswerFile is clone-only: the image recipe has already installed all
// drivers/agent and generalised the root. Never partition a workspace root.
func windowsAnswerFile(hostname, username, password string) (string, error) {
	var escaped [3]string
	for i, value := range []string{hostname, username, password} {
		var b bytes.Buffer
		if err := xml.EscapeText(&b, []byte(value)); err != nil {
			return "", err
		}
		escaped[i] = b.String()
	}
	return xml.Header + fmt.Sprintf(`<unattend xmlns="urn:schemas-microsoft-com:unattend" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State"><settings pass="specialize"><component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"><ComputerName>%s</ComputerName><TimeZone>UTC</TimeZone></component></settings><settings pass="oobeSystem"><component name="Microsoft-Windows-International-Core" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"><InputLocale>en-US</InputLocale><SystemLocale>en-US</SystemLocale><UILanguage>en-US</UILanguage><UserLocale>en-US</UserLocale></component><component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"><OOBE><HideEULAPage>true</HideEULAPage><HideOnlineAccountScreens>true</HideOnlineAccountScreens><HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE><ProtectYourPC>3</ProtectYourPC></OOBE><UserAccounts><LocalAccounts><LocalAccount wcm:action="add"><Name>%s</Name><DisplayName>Workspace</DisplayName><Group>Administrators</Group><Password><Value>%s</Value><PlainText>true</PlainText></Password></LocalAccount></LocalAccounts></UserAccounts></component></settings></unattend>`, escaped[0], escaped[1], escaped[2]), nil
}

func (r *WorkspaceReconciler) reconcileWindowsBootstrap(ctx context.Context, instance *workspacev1.Workspace) error {
	if instance.Spec.VMProfile.ImportSecretName != "" {
		registry := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: instance.Spec.VMProfile.ImportSecretName, Namespace: instance.Namespace}, registry); err != nil {
			return fmt.Errorf("windows CDI registry credentials are unavailable")
		}
		if len(registry.Data["accessKeyId"]) == 0 || len(registry.Data["secretKey"]) == 0 {
			return fmt.Errorf("windows CDI pod imports require accessKeyId and secretKey credentials")
		}
	}
	if instance.Spec.VMProfile.ImportCertConfigMapName != "" {
		certificates := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{Name: instance.Spec.VMProfile.ImportCertConfigMapName, Namespace: instance.Namespace}, certificates); err != nil || len(certificates.Data) == 0 {
			return fmt.Errorf("windows CDI registry CA certificates are unavailable")
		}
	}
	name := windowsBootstrapSecretName(instance)
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, secret)
	if err == nil {
		if !metav1.IsControlledBy(secret, instance) {
			return fmt.Errorf("windows bootstrap Secret is owned by another resource")
		}
		if len(secret.Data["password"]) == 0 || (instance.Annotations[windowsProvisionedGeneration] != instance.Spec.VMProfile.Generation && len(secret.Data["autounattend.xml"]) == 0) {
			return fmt.Errorf("windows bootstrap Secret is incomplete")
		}
		return nil // Credentials must never rotate on an incidental reconcile.
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if instance.Annotations[windowsProvisionedGeneration] == instance.Spec.VMProfile.Generation {
		return fmt.Errorf("windows initial credentials are missing; explicit recovery or Reset is required")
	}
	data := make([]byte, 24)
	if _, err := rand.Read(data); err != nil {
		return fmt.Errorf("unable to generate Windows bootstrap credentials")
	}
	password := base64.RawURLEncoding.EncodeToString(data) + "!9aA"
	answer, err := windowsAnswerFile(windowsHostname(instance), "workspace", password)
	if err != nil {
		return fmt.Errorf("unable to render Windows bootstrap")
	}
	secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace, Labels: map[string]string{"kubeworkspaces.io/windows-generation": instance.Spec.VMProfile.Generation}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"autounattend.xml": []byte(answer), "username": []byte("workspace"), "password": []byte(password)}}
	if err := ctrl.SetControllerReference(instance, secret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, secret)
}

func windowsAgentConnected(ctx context.Context, reader client.Reader, instance *workspacev1.Workspace) bool {
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(kubeVirtVirtualMachineInstanceGVK)
	if err := reader.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, vmi); err != nil {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(vmi.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]interface{})
		if ok && condition["type"] == "AgentConnected" && condition["status"] == "True" {
			return true
		}
	}
	return false
}

// Reset is handled only after the old VM and its owned root/state resources
// are gone. Credentials and firmware identity then get a fresh generation.
func (r *WorkspaceReconciler) resetWindowsGeneration(ctx context.Context, instance *workspacev1.Workspace) error {
	if !windowsWorkspace(instance) {
		return nil
	}
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: windowsBootstrapSecretName(instance), Namespace: instance.Namespace}, secret)
	if err == nil {
		if !metav1.IsControlledBy(secret, instance) {
			return fmt.Errorf("windows bootstrap Secret is owned by another resource")
		}
		if err := r.Delete(ctx, secret); err != nil {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	instance.Spec.VMProfile.Generation = string(utiluuid.NewUUID())
	delete(instance.Annotations, windowsProvisionedGeneration)
	delete(instance.Annotations, windowsDetachingGeneration)
	return nil
}

// A completed guest is gracefully restarted once to physically remove its
// bootstrap CD. Its credentials are retained separately in the owned Secret.
func (r *WorkspaceReconciler) windowsReady(ctx context.Context, instance *workspacev1.Workspace, pod *corev1.Pod, running bool) (bool, error) {
	if _, stopped := instance.Annotations[AnnotationStopped]; stopped || instance.Annotations[windowsRebootAnnotation] != "" {
		return false, nil
	}
	if !running || !windowsAgentConnected(ctx, r.Client, instance) {
		return false, nil
	}
	return r.reconcileWindowsProvisioning(ctx, instance, pod)
}

func (r *WorkspaceReconciler) reconcileWindowsProvisioning(ctx context.Context, instance *workspacev1.Workspace, pod *corev1.Pod) (bool, error) {
	generation := instance.Spec.VMProfile.Generation
	if instance.Annotations[windowsDetachingGeneration] == generation {
		return false, nil
	}
	if instance.Annotations[windowsProvisionedGeneration] == generation {
		return true, nil
	}
	if pod == nil || r.GuestAgent == nil {
		return false, nil
	}
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(kubeVirtVirtualMachineInstanceGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, vmi); err != nil {
		return false, err
	}
	// KubeVirt's activePods map binds exec to this VMI's actual launcher UID,
	// rather than accepting a namespace user's lookalike pod label.
	activePods, _, _ := unstructured.NestedStringMap(vmi.Object, "status", "activePods")
	if _, active := activePods[string(pod.UID)]; !active {
		return false, fmt.Errorf("windows provisioning requires the active KubeVirt launcher")
	}
	complete, err := r.GuestAgent.CompleteBootstrap(ctx, instance.Namespace, pod.Name, instance.Name, windowsHostname(instance))
	if err != nil || !complete {
		return false, err
	}
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations[windowsProvisionedGeneration] = generation
	instance.Annotations[windowsDetachingGeneration] = generation
	if err := r.Update(ctx, instance); err != nil {
		return false, err
	}
	return false, nil
}

func (r *WorkspaceReconciler) finishWindowsDetach(ctx context.Context, instance *workspacev1.Workspace) error {
	if instance.Annotations[windowsDetachingGeneration] != instance.Spec.VMProfile.Generation {
		return nil
	}
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(kubeVirtVirtualMachineInstanceGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, vmi); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: windowsBootstrapSecretName(instance), Namespace: instance.Namespace}, secret); err != nil {
		return fmt.Errorf("windows initial credentials are unavailable during bootstrap removal")
	}
	if !metav1.IsControlledBy(secret, instance) {
		return fmt.Errorf("windows bootstrap Secret is owned by another resource")
	}
	delete(secret.Data, "autounattend.xml")
	if err := r.Update(ctx, secret); err != nil {
		return err
	}
	delete(instance.Annotations, windowsDetachingGeneration)
	return r.Update(ctx, instance)
}

func (r *WorkspaceReconciler) finishWindowsReboot(ctx context.Context, instance *workspacev1.Workspace) error {
	if instance.Annotations[windowsRebootAnnotation] == "" {
		return nil
	}
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(kubeVirtVirtualMachineInstanceGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, vmi); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	delete(instance.Annotations, windowsRebootAnnotation)
	return r.Update(ctx, instance)
}

func (r *WorkspaceReconciler) finishWindowsLifecycle(ctx context.Context, instance *workspacev1.Workspace) error {
	if err := r.finishWindowsDetach(ctx, instance); err != nil {
		return err
	}
	return r.finishWindowsReboot(ctx, instance)
}

func windowsStatusConditions(instance *workspacev1.Workspace, status *workspacev1.WorkspaceStatus) {
	reason, message := "WindowsReady", "Windows setup and bootstrap removal completed"
	if status.ReadyReplicas == 0 {
		reason, message = "WindowsProvisioning", "Waiting for Windows native setup, guest agent and bootstrap removal"
	}
	ready := workspacev1.WorkspaceCondition{Type: "Ready", Status: "False", Reason: reason, Message: message, LastTransitionTime: metav1.Now(), LastProbeTime: metav1.Now()}
	if status.ReadyReplicas > 0 {
		ready.Status = "True"
	}
	filtered := make([]workspacev1.WorkspaceCondition, 0, len(status.Conditions)+1)
	for _, condition := range status.Conditions {
		if condition.Type != "Ready" {
			filtered = append(filtered, condition)
		}
	}
	status.Conditions = append(filtered, ready)
	// Stable conditions avoid a status-watch feedback loop while OOBE is
	// pending; the bounded requeue performs the next guest check instead.
	for i := range status.Conditions {
		for _, old := range instance.Status.Conditions {
			current := &status.Conditions[i]
			if current.Type == old.Type && current.Status == old.Status && current.Reason == old.Reason && current.Message == old.Message {
				current.LastTransitionTime, current.LastProbeTime = old.LastTransitionTime, old.LastProbeTime
			}
		}
	}
}

func generateWindowsVirtualMachine(instance *workspacev1.Workspace, agentPort int32) *unstructured.Unstructured {
	profile := instance.Spec.VMProfile
	identity := windowsIdentity(instance)
	macsum := sha256.Sum256([]byte(identity))
	mac := fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", macsum[0], macsum[1], macsum[2], macsum[3], macsum[4])
	diskName := rootDiskDataVolumeName(instance.Name)
	registry := map[string]interface{}{"url": "docker://" + profile.Image, "pullMethod": "pod"}
	if profile.ImportSecretName != "" {
		registry["secretRef"] = profile.ImportSecretName
	}
	if profile.ImportCertConfigMapName != "" {
		registry["certConfigMap"] = profile.ImportCertConfigMapName
	}
	storage := map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}, "volumeMode": "Filesystem", "resources": map[string]interface{}{"requests": map[string]interface{}{"storage": profile.RootDiskSize}}}
	if profile.StorageClassName != "" {
		storage["storageClassName"] = profile.StorageClassName
	}
	metadata := map[string]interface{}{"name": diskName}
	if node := instance.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]; node != "" {
		metadata["annotations"] = map[string]interface{}{"volume.kubernetes.io/selected-node": node}
	}
	disks := []interface{}{map[string]interface{}{"name": "rootdisk", "disk": map[string]interface{}{"bus": "virtio"}, "bootOrder": int64(1)}}
	volumes := []interface{}{map[string]interface{}{"name": "rootdisk", "dataVolume": map[string]interface{}{"name": diskName}}}
	if instance.Annotations[windowsProvisionedGeneration] != profile.Generation {
		disks = append(disks, map[string]interface{}{"name": "sysprep", "cdrom": map[string]interface{}{"bus": "sata", "readonly": true}})
		volumes = append(volumes, map[string]interface{}{"name": "sysprep", "sysprep": map[string]interface{}{"secret": map[string]interface{}{"name": windowsBootstrapSecretName(instance)}}})
	}
	guest := resource.MustParse(profile.GuestMemory)
	podMemory := guest.DeepCopy()
	podMemory.Add(resource.MustParse("1Gi"))
	// Agent data plane: forward the guest agent port through the masquerade
	// interface when the digest-pinned Image declares one. Same forward shape
	// as the generic vmDefaultNetwork path; absent (0) by default.
	defaultInterface := map[string]interface{}{"name": "default", "model": "virtio", "macAddress": mac, "masquerade": map[string]interface{}{}}
	if agentPort > 0 {
		defaultInterface["ports"] = []interface{}{map[string]interface{}{"port": int64(agentPort), "protocol": "TCP"}}
	}
	domain := map[string]interface{}{
		"machine": map[string]interface{}{"type": "q35"}, "cpu": map[string]interface{}{"cores": int64(profile.CPUCores), "model": "host-passthrough"},
		"memory": map[string]interface{}{"guest": guest.String()}, "resources": map[string]interface{}{"requests": map[string]interface{}{"memory": podMemory.String()}},
		"firmware": map[string]interface{}{"uuid": identity, "bootloader": map[string]interface{}{"efi": map[string]interface{}{"secureBoot": true, "persistent": true}}},
		"features": map[string]interface{}{"acpi": map[string]interface{}{"enabled": true}, "smm": map[string]interface{}{"enabled": true}, "hyperv": map[string]interface{}{"relaxed": map[string]interface{}{"enabled": true}, "vapic": map[string]interface{}{"enabled": true}, "spinlocks": map[string]interface{}{"enabled": true, "spinlocks": int64(8191)}}},
		"clock":    map[string]interface{}{"utc": map[string]interface{}{}, "timer": map[string]interface{}{"hpet": map[string]interface{}{"present": false}, "pit": map[string]interface{}{"tickPolicy": "delay"}, "rtc": map[string]interface{}{"tickPolicy": "catchup"}, "hyperv": map[string]interface{}{"present": true}}},
		"devices":  map[string]interface{}{"tpm": map[string]interface{}{"persistent": true}, "autoattachGraphicsDevice": true, "autoattachSerialConsole": false, "video": map[string]interface{}{"type": "vga"}, "disks": disks, "inputs": []interface{}{map[string]interface{}{"name": "tablet", "type": "tablet", "bus": "usb"}}, "interfaces": []interface{}{defaultInterface}},
	}
	_, stopped := instance.Annotations[AnnotationStopped]
	stopped = stopped || instance.Annotations[windowsDetachingGeneration] == profile.Generation
	stopped = stopped || instance.Annotations[windowsRebootAnnotation] != ""
	spec := map[string]interface{}{"running": !stopped, "dataVolumeTemplates": []interface{}{map[string]interface{}{"metadata": metadata, "spec": map[string]interface{}{"source": map[string]interface{}{"registry": registry}, "storage": storage}}}, "template": map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]interface{}{LabelWorkspaceName: instance.Name}}, "spec": map[string]interface{}{"architecture": "amd64", "terminationGracePeriodSeconds": int64(180), "domain": domain, "networks": []interface{}{map[string]interface{}{"name": "default", "pod": map[string]interface{}{}}}, "volumes": volumes}}}
	applyVMScheduling(spec, instance.Spec.Template.Spec)
	nodeSelector := instance.Spec.Template.Spec.NodeSelector
	if nodeSelector == nil {
		nodeSelector = map[string]string{}
	}
	selector := map[string]interface{}{}
	for k, v := range nodeSelector {
		selector[k] = v
	}
	selector["kubernetes.io/arch"] = windowsGuestArchitecture
	_ = unstructured.SetNestedMap(spec, selector, "template", "spec", "nodeSelector")
	vm := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": kubeVirtVirtualMachineGVK.GroupVersion().String(), "kind": kubeVirtVirtualMachineGVK.Kind, "metadata": map[string]interface{}{"name": instance.Name, "namespace": instance.Namespace, "annotations": map[string]interface{}{"kubeworkspaces.io/windows-generation": profile.Generation}}, "spec": spec}}
	vm.SetGroupVersionKind(kubeVirtVirtualMachineGVK)
	return vm
}
