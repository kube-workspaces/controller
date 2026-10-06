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

package v1alpha1

// Windows11AMD64V1 is the controller-supported Windows guest hardware contract.
const Windows11AMD64V1 = "windows11-amd64-v1"

// ResolvedVMProfile pins a workspace's guest profile and prepared root source.
// It is persisted before the VM is created so catalog edits/removal cannot
// silently change the hardware or source of an existing guest.
type ResolvedVMProfile struct {

	// ID selects a versioned controller-supported profile, not arbitrary QEMU configuration.
	// +kubebuilder:validation:Enum=windows11-amd64-v1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="VM guest profile ID is immutable"
	ID string `json:"id"`
	// Image is the exact OCI digest used for this provisioning generation.
	// +kubebuilder:validation:Pattern=`^.+@sha256:[a-f0-9]{64}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Resolved VM source is immutable"
	Image string `json:"image"`
	// Generation identifies root, firmware/TPM identity and bootstrap credentials.
	// The controller creates it once and replaces it only during an explicit Reset.
	// +optional
	Generation string `json:"generation,omitempty"`
	// RootDiskSize is the resolved persistent root capacity (including image virtual size).
	// +kubebuilder:default="80Gi"
	// +optional
	RootDiskSize string `json:"rootDiskSize,omitempty"`
	// CPUCores is the resolved guest CPU count (minimum 2 for Windows 11).
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:default=4
	// +optional
	CPUCores int32 `json:"cpuCores,omitempty"`
	// GuestMemory is resolved guest RAM, independent of launcher overhead.
	// +kubebuilder:default="8Gi"
	// +optional
	GuestMemory string `json:"guestMemory,omitempty"`
	// ImportSecretName names same-namespace CDI pod-import credentials with
	// accessKeyId (registry username) and secretKey (password/token).
	// +optional
	ImportSecretName string `json:"importSecretName,omitempty"`
	// ImportCertConfigMapName names same-namespace private registry CA PEM files.
	// Empty uses the CDI importer's normal trust roots.
	// +optional
	ImportCertConfigMapName string `json:"importCertConfigMapName,omitempty"`
	// StorageClassName selects the root StorageClass; empty uses the cluster default.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
}
