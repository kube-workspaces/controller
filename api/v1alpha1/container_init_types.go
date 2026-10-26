package v1alpha1

import corev1 "k8s.io/api/core/v1"

// ContainerInitDefaults opts a container image into cloud-config delivery.
type ContainerInitDefaults struct {
	// Mode selects normal startup, injected entrypoint wrapping, or data-only
	// delivery to an image whose own entrypoint runs container-init.
	// +kubebuilder:validation:Enum=disabled;injected;baked-in
	Mode string `json:"mode"`
}

// ContainerInitSpec is a creation-time snapshot, independent of later Image
// edits. Inline UserData is visible in the Workspace CR; a Secret reference can
// instead keep private configuration out of that document.
// +kubebuilder:validation:XValidation:rule="!(has(self.userData) && has(self.userDataSecretRef))",message="Choose inline userData or userDataSecretRef, not both"
// +kubebuilder:validation:XValidation:rule="self.mode == 'disabled' || (has(self.userData) && size(self.userData) > 0) || has(self.userDataSecretRef)",message="Enabled container initialization requires user-data"
type ContainerInitSpec struct {
	// +kubebuilder:validation:Enum=disabled;injected;baked-in
	Mode string `json:"mode"`
	// UserData is an inline #cloud-config document, mounted through a generated
	// Workspace-owned Secret. Maximum 256 KiB (also enforced in bytes at runtime).
	// +kubebuilder:validation:MaxLength=262144
	// +optional
	UserData string `json:"userData,omitempty"`
	// UserDataSecretRef references a same-namespace Secret with a user-data key.
	// The referenced Secret is independently managed and is never adopted/deleted.
	// +kubebuilder:validation:XValidation:rule="has(self.name) && size(self.name) > 0",message="A user-data Secret name is required"
	// +optional
	UserDataSecretRef *corev1.LocalObjectReference `json:"userDataSecretRef,omitempty"`
}
