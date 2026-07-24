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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodDefaultSpec defines configuration to inject into matching workspace pods.
type PodDefaultSpec struct {
	// Selector is a label selector that determines which workspaces this PodDefault applies to.
	// If empty, applies to all workspaces in the namespace.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// Desc is a human-readable description of what this PodDefault provides.
	// +optional
	Desc string `json:"desc,omitempty"`
	// Env are additional environment variables to inject into workspace containers.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// VolumeMounts are additional volume mounts to inject into workspace containers.
	// +optional
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`
	// Volumes are additional volumes to add to the workspace pod.
	// +optional
	Volumes []corev1.Volume `json:"volumes,omitempty"`
	// ServiceAccountName overrides the pod's service account.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// Annotations are additional annotations to add to the workspace pod.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// Labels are additional labels to add to the workspace pod.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// PodDefaultStatus defines the observed state of PodDefault.
type PodDefaultStatus struct {
	// Conditions is an array of current conditions.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=poddefaults,singular=poddefault,scope=Namespaced
// +kubebuilder:printcolumn:name="Description",type="string",JSONPath=".spec.desc"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PodDefault is the Schema for the poddefaults API.
// It defines configuration that is automatically injected into matching workspace pods at creation time.
type PodDefault struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PodDefaultSpec   `json:"spec,omitempty"`
	Status PodDefaultStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodDefaultList contains a list of PodDefault.
type PodDefaultList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodDefault `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PodDefault{}, &PodDefaultList{})
}
