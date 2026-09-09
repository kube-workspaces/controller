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

// SshKeySpec defines the desired state of SshKey.
type SshKeySpec struct {
	// Name is a human-friendly label for this key (e.g. "laptop-2025").
	// +optional
	Name string `json:"name,omitempty"`
	// PublicKey is the SSH public key line (e.g. "ssh-ed25519 AAAA... user@host").
	PublicKey string `json:"publicKey"`
}

// SshKey is the Schema for the sshkeys API.
// It stores an SSH public key owned by a user. The workspace controller seeds
// the keys into VM guest cloud-init user-data so the guest's sshd trusts them
// (enabling passwordless SSH into VM workspaces). Keys live in the user's
// personal namespace and apply to every vm workspace created there.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=sshkeys,singular=sshkey,scope=Namespaced
// +kubebuilder:printcolumn:name="Name",type="string",JSONPath=".spec.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type SshKey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SshKeySpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// SshKeyList contains a list of SshKey.
type SshKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SshKey `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SshKey{}, &SshKeyList{})
}
