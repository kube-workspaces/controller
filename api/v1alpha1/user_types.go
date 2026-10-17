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

// UserRole defines the role level for a user.
// +kubebuilder:validation:Enum=admin;editor;viewer
type UserRole string

const (
	UserRoleAdmin  UserRole = "admin"
	UserRoleEditor UserRole = "editor"
	UserRoleViewer UserRole = "viewer"
)

// NamespaceAccessEntry defines access to an additional shared namespace.
type NamespaceAccessEntry struct {
	// Namespace is the target namespace name.
	Namespace string `json:"namespace"`
	// Role is the access level in this namespace.
	// +kubebuilder:validation:Enum=admin;editor;viewer
	Role UserRole `json:"role"`
}

// ExecutionPermission is an independently delegated VM management ability.
// +kubebuilder:validation:Enum=workspace.exec;workspace.shell;workspace.exec.elevated;workspace.execution.manage;workspace.execution.recordings.read
type ExecutionPermission string

const (
	ExecutionPermissionExec           ExecutionPermission = "workspace.exec"
	ExecutionPermissionShell          ExecutionPermission = "workspace.shell"
	ExecutionPermissionElevated       ExecutionPermission = "workspace.exec.elevated"
	ExecutionPermissionManage         ExecutionPermission = "workspace.execution.manage"
	ExecutionPermissionReadRecordings ExecutionPermission = "workspace.execution.recordings.read"
)

// ExecutionGrant delegates abilities within existing namespace/workspace access.
// It never creates namespace access or permits the recipient to delegate onward.
type ExecutionGrant struct {
	// Namespace is the exact namespace; wildcard grants are not supported.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`
	// Workspaces optionally narrows the grant to exact Workspace names.
	// Omit to cover accessible workspaces throughout Namespace.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +listType=set
	Workspaces []string `json:"workspaces,omitempty"`
	// Permissions are additive; elevation alone never permits exec or shell.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=5
	// +listType=set
	Permissions []ExecutionPermission `json:"permissions"`
	// ExpiresAt is the exclusive expiry bound; omitted means no grant expiry.
	// Active executions must still renew short-lived authorization leases.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// LocalAuthSpec defines local (username/password) authentication for a user.
type LocalAuthSpec struct {
	// Enabled controls whether this user may authenticate with a local password,
	// in addition to any other configured authentication method (e.g. OIDC).
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// PasswordSecretRef references the Secret and key holding the bcrypt password
	// hash for this user (key is conventionally "passwordHash").
	// +optional
	PasswordSecretRef SecretKeyRef `json:"passwordSecretRef,omitempty"`
	// MustChangePassword indicates the user must set a new password before
	// continuing to use the system. Set on creation/reset, cleared on change.
	// +optional
	MustChangePassword bool `json:"mustChangePassword,omitempty"`
}

// UserSpec defines the desired state of User.
type UserSpec struct {
	// Email is the unique identifier for the user (immutable).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Format=email
	Email string `json:"email"`
	// DisplayName is the human-readable name of the user.
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// Role is the cluster-wide default role for this user.
	// +kubebuilder:default=editor
	Role UserRole `json:"role"`
	// Disabled indicates whether the user account is disabled.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
	// Groups are the groups this user belongs to.
	// +optional
	Groups []string `json:"groups,omitempty"`
	// NamespaceAccess defines additional shared namespace grants beyond the personal namespace.
	// +optional
	NamespaceAccess []NamespaceAccessEntry `json:"namespaceAccess,omitempty"`
	// ExecutionGrants are administrator-managed, scoped VM execution abilities.
	// Empty grants give non-admin users no execution, elevation or recording rights.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	ExecutionGrants []ExecutionGrant `json:"executionGrants,omitempty"`
	// LocalAuth configures local username/password authentication for this user.
	// May be set alongside other authentication methods (e.g. OIDC) for the same identity.
	// +optional
	LocalAuth *LocalAuthSpec `json:"localAuth,omitempty"`
}

// UserStatus defines the observed state of User.
type UserStatus struct {
	// PersonalNamespace is the name of the user's personal namespace (if enabled).
	// +optional
	PersonalNamespace string `json:"personalNamespace,omitempty"`
	// AvatarURL is the URL to the user's avatar image (from OIDC or Gravatar).
	// +optional
	AvatarURL string `json:"avatarURL,omitempty"`
	// LastLogin is the timestamp of the user's last login.
	// +optional
	LastLogin *metav1.Time `json:"lastLogin,omitempty"`
	// LoginCount is the number of times the user has logged in.
	// +optional
	LoginCount int64 `json:"loginCount,omitempty"`
	// FailedLoginAttempts is the number of consecutive failed local login attempts.
	// Reset to 0 on successful login.
	// +optional
	FailedLoginAttempts int `json:"failedLoginAttempts,omitempty"`
	// LockedUntil, if set and in the future, indicates local login is temporarily
	// disabled due to repeated failed attempts.
	// +optional
	LockedUntil *metav1.Time `json:"lockedUntil,omitempty"`
	// Conditions represent the latest available observations of the User's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=users,singular=user,scope=Cluster
// +kubebuilder:printcolumn:name="Email",type="string",JSONPath=".spec.email"
// +kubebuilder:printcolumn:name="Role",type="string",JSONPath=".spec.role"
// +kubebuilder:printcolumn:name="Disabled",type="boolean",JSONPath=".spec.disabled"
// +kubebuilder:printcolumn:name="Namespace",type="string",JSONPath=".status.personalNamespace"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// User is the Schema for the users API.
// It represents a user account in the kube-workspaces system.
type User struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UserSpec   `json:"spec,omitempty"`
	Status UserStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// UserList contains a list of User.
type UserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []User `json:"items"`
}

func init() {
	SchemeBuilder.Register(&User{}, &UserList{})
}
