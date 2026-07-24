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

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MaintenanceConfig defines maintenance mode settings.
type MaintenanceConfig struct {
	// Enabled controls whether maintenance mode is active.
	// When true, non-admin users see the maintenance page and cannot create/manage workspaces.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Message is an optional message displayed to users during maintenance.
	// Supports plain text. If empty, a default message is shown.
	// +optional
	Message string `json:"message,omitempty"`
}

// PlatformFormFieldLock defines a locked form field with its enforced value.
type PlatformFormFieldLock struct {
	// Field is the name of the form field to lock (e.g. "image_pull_policy", "gpu", "shared_memory", "cpu_limit", "memory_limit").
	Field string `json:"field"`
	// Value is the enforced value for the field. For boolean fields use "true"/"false".
	// For select fields use the option value. Empty string means use the default.
	// +optional
	Value string `json:"value,omitempty"`
	// Message is an optional hint displayed to users explaining why the field is locked.
	// +optional
	Message string `json:"message,omitempty"`
}

// PlatformFormConfig defines workspace creation form behavior.
type PlatformFormConfig struct {
	// LockedFields lists form fields that non-admin users cannot modify.
	// Admins can always override locked fields.
	// +optional
	LockedFields []PlatformFormFieldLock `json:"lockedFields,omitempty"`
}

// PlatformConfigSpec defines the desired state of PlatformConfig.
type PlatformConfigSpec struct {
	// Maintenance configures maintenance mode for the platform.
	// +optional
	Maintenance *MaintenanceConfig `json:"maintenance,omitempty"`
	// Form configures workspace creation form behavior including field locking.
	// +optional
	Form *PlatformFormConfig `json:"form,omitempty"`
}

// PlatformConfigStatus defines the observed state of PlatformConfig.
type PlatformConfigStatus struct {
	// MaintenanceActive reflects whether maintenance mode is currently active.
	// +optional
	MaintenanceActive bool `json:"maintenanceActive,omitempty"`
	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=platformconfigs,singular=platformconfig,scope=Cluster
// +kubebuilder:printcolumn:name="Maintenance",type="boolean",JSONPath=".spec.maintenance.enabled"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PlatformConfig is the Schema for the platformconfigs API.
// It defines platform-wide configuration for kube-workspaces that is not related to authentication.
// Typically only one instance named "default" should exist.
type PlatformConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlatformConfigSpec   `json:"spec,omitempty"`
	Status PlatformConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PlatformConfigList contains a list of PlatformConfig.
type PlatformConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PlatformConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PlatformConfig{}, &PlatformConfigList{})
}
