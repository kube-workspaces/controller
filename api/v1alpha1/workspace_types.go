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

// WorkspaceSpec defines the desired state of Workspace.
// Modelled closely on Kubeflow Notebook spec - wraps a full PodSpec.
type WorkspaceSpec struct {
	// Type selects the workload used to run the workspace.
	// "container" (default) runs the pod template as a StatefulSet.
	// "scratch" runs it as a plain Deployment (no persistent identity).
	// "vm" runs it as a KubeVirt VirtualMachine; the main container image must be
	// a containerDisk image containing a bootable guest disk.
	// +kubebuilder:validation:Enum=container;vm;scratch
	// +kubebuilder:default=container
	// +optional
	Type string `json:"type,omitempty"`
	// Template describes the pod that will be created for the workspace.
	Template WorkspaceTemplateSpec `json:"template"`
}

// WorkspaceTemplateSpec wraps a PodSpec for the workspace pod.
type WorkspaceTemplateSpec struct {
	Spec corev1.PodSpec `json:"spec"`
}

// WorkspaceCondition describes the state of a workspace at a certain point.
type WorkspaceCondition struct {
	// Type is the type of the condition. Possible values are Running|Waiting|Terminated
	Type string `json:"type"`
	// Status is the status of the condition. Can be True, False, Unknown.
	Status string `json:"status"`
	// Last time we probed the condition.
	// +optional
	LastProbeTime metav1.Time `json:"lastProbeTime,omitempty"`
	// Last time the condition transitioned from one status to another.
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
	// Reason the container is in the current state (brief).
	// +optional
	Reason string `json:"reason,omitempty"`
	// Message regarding why the container is in the current state.
	// +optional
	Message string `json:"message,omitempty"`
}

// WorkspaceStatus defines the observed state of Workspace.
type WorkspaceStatus struct {
	// Conditions is an array of current conditions
	Conditions []WorkspaceCondition `json:"conditions"`
	// ReadyReplicas is the number of Pods created by the StatefulSet controller that have a Ready Condition.
	ReadyReplicas int32 `json:"readyReplicas"`
	// ContainerState is the state of the underlying workspace container.
	ContainerState corev1.ContainerState `json:"containerState"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=workspaces,singular=workspace,scope=Namespaced
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Workspace is the Schema for the workspaces API.
// It represents a container-based workspace (IDE, desktop, etc.) running in Kubernetes.
type Workspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkspaceSpec   `json:"spec,omitempty"`
	Status WorkspaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkspaceList contains a list of Workspace
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workspace{}, &WorkspaceList{})
}
