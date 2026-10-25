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

// ImageRepositorySpec defines the desired state of ImageRepository.
type ImageRepositorySpec struct {
	// URL of the git repository (https:// or ssh://). SSH auth is backlog.
	URL string `json:"url"`
	// Ref is the git ref to checkout (branch/tag/commit). If empty, controller may try common defaults.
	// +optional
	Ref string `json:"ref,omitempty"`
	// Paths are directories in the repo to scan for Image CR YAML files (relative to repo root).
	// Only files directly in each directory are scanned; subdirectories must be listed separately.
	// A trailing slash is optional. If empty, defaults to ["images"].
	// +optional
	Paths []string `json:"paths,omitempty"`
	// Auth config for private repos.
	// +optional
	Auth *ImageRepositoryAuth `json:"auth,omitempty"`
	// SyncInterval is how often to re-sync (e.g. "10m"). If unset, uses controller default with jitter.
	// +optional
	SyncInterval *metav1.Duration `json:"syncInterval,omitempty"`
	// Suspend pauses reconciliation/syncing.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// ImageRepositoryAuth defines authentication for git operations.
type ImageRepositoryAuth struct {
	// SecretRef references a Secret with credentials. For HTTPS basic auth, keys: username/password or token.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`
	// Type indicates auth type (https-basic). ssh-key is backlog.
	// +optional
	Type string `json:"type,omitempty"`
}

// ImageRepositoryStatus defines the observed state of ImageRepository.
type ImageRepositoryStatus struct {
	// Conditions is an array of current conditions.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// LastSyncTime is when the repo was last synced.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
	// LastSyncCommit is the git commit SHA synced.
	// +optional
	LastSyncCommit string `json:"lastSyncCommit,omitempty"`
	// ImportedImages lists Image names imported from this repo.
	// +optional
	ImportedImages []string `json:"importedImages,omitempty"`
	// ObservedGeneration from last sync.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=imagerepositories,singular=imagerepository,scope=Cluster,shortName=imgrep
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".spec.url"
// +kubebuilder:printcolumn:name="Ref",type="string",JSONPath=".spec.ref"
// +kubebuilder:printcolumn:name="LastSync",type="date",JSONPath=".status.lastSyncTime"
// +kubebuilder:printcolumn:name="Commit",type="string",JSONPath=".status.lastSyncCommit"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ImageRepository is the Schema for the imagerepositories API.
// It represents a git repository that stores kube-workspaces Image CR manifests.
type ImageRepository struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImageRepositorySpec   `json:"spec,omitempty"`
	Status ImageRepositoryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ImageRepositoryList contains a list of ImageRepository.
type ImageRepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageRepository `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ImageRepository{}, &ImageRepositoryList{})
}
