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

// SecretKeyRef is a reference to a key in a Secret.
type SecretKeyRef struct {
	// Name is the name of the secret.
	Name string `json:"name"`
	// Key is the key within the secret.
	Key string `json:"key"`
}

// OIDCConfig defines the OIDC provider configuration.
type OIDCConfig struct {
	// IssuerURL is the OIDC issuer URL (e.g. https://dex.example.com).
	// +kubebuilder:validation:Required
	IssuerURL string `json:"issuerURL"`
	// ClientID is the OAuth2 client ID.
	// +kubebuilder:validation:Required
	ClientID string `json:"clientID"`
	// ClientSecret is a reference to the secret containing the OAuth2 client secret.
	// +kubebuilder:validation:Required
	ClientSecret SecretKeyRef `json:"clientSecret"`
	// Scopes are the OIDC scopes to request.
	// +optional
	// +kubebuilder:default={"openid","email","profile","groups"}
	Scopes []string `json:"scopes,omitempty"`
	// UsernameClaim is the JWT claim to use as the username.
	// +optional
	// +kubebuilder:default=email
	UsernameClaim string `json:"usernameClaim,omitempty"`
	// GroupsClaim is the JWT claim to use for group membership.
	// +optional
	// +kubebuilder:default=groups
	GroupsClaim string `json:"groupsClaim,omitempty"`
}

// SessionConfig defines session/token configuration.
type SessionConfig struct {
	// SigningKey is a reference to the secret containing the JWT signing key.
	// +kubebuilder:validation:Required
	SigningKey SecretKeyRef `json:"signingKey"`
	// TokenExpiry is how long session tokens are valid.
	// +optional
	// +kubebuilder:default="24h"
	TokenExpiry string `json:"tokenExpiry,omitempty"`
	// RefreshExpiry is how long refresh tokens are valid.
	// +optional
	// +kubebuilder:default="7d"
	RefreshExpiry string `json:"refreshExpiry,omitempty"`
}

// PersonalNamespaceConfig defines personal namespace behavior.
type PersonalNamespaceConfig struct {
	// Enabled controls whether personal namespaces are created for users.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Template is the namespace name template. Supports {{username}} placeholder.
	// +optional
	// +kubebuilder:default="{{username}}"
	Template string `json:"template,omitempty"`
	// Labels are applied to created personal namespaces.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// Annotations are applied to created personal namespaces.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// ResourceQuota defines optional resource limits for personal namespaces.
	// +optional
	ResourceQuota map[string]string `json:"resourceQuota,omitempty"`
}

// RegistrationConfig defines user registration/provisioning behavior.
type RegistrationConfig struct {
	// AutoProvision controls whether User CRs are auto-created on first OIDC login.
	// +optional
	// +kubebuilder:default=true
	AutoProvision bool `json:"autoProvision,omitempty"`
	// DefaultRole is the role assigned to newly provisioned users.
	// +optional
	// +kubebuilder:default=editor
	// +kubebuilder:validation:Enum=admin;editor;viewer
	DefaultRole UserRole `json:"defaultRole,omitempty"`
	// AllowedDomains restricts registration to specific email domains. Empty means allow all.
	// When both AllowedDomains and AllowedEmails are set, either match permits login (OR logic).
	// +optional
	AllowedDomains []string `json:"allowedDomains,omitempty"`
	// AllowedEmails restricts login to specific email addresses. Empty means allow all.
	// Emails in spec.adminEmails always bypass this restriction.
	// When both AllowedDomains and AllowedEmails are set, either match permits login (OR logic).
	// +optional
	AllowedEmails []string `json:"allowedEmails,omitempty"`
	// RequireApproval if true, new users are created as disabled until an admin enables them.
	// +optional
	RequireApproval bool `json:"requireApproval,omitempty"`
}

// AuthorizationConfig defines authorization behavior.
type AuthorizationConfig struct {
	// RestrictNamespaceAccess when true, non-admin users can only access namespaces
	// explicitly assigned to them (via User CR namespaceAccess + personal namespace).
	// When false (default), all authenticated users can see all namespaces.
	// Admins always have access to everything regardless of this setting.
	// +optional
	RestrictNamespaceAccess bool `json:"restrictNamespaceAccess,omitempty"`
}

// BootstrapAdminConfig defines auto-creation of the default local admin user.
type BootstrapAdminConfig struct {
	// Email is the identifier used for the auto-created admin user.
	// +optional
	// +kubebuilder:default="admin@local"
	Email string `json:"email,omitempty"`
	// Skip disables auto-creation of the bootstrap admin user, e.g. when an
	// admin user has already been provisioned manually.
	// +optional
	Skip bool `json:"skip,omitempty"`
}

// LocalAuthConfig defines local (username/password) authentication behavior.
// LocalAuth may be enabled independently of, or alongside, OIDC.
type LocalAuthConfig struct {
	// Enabled controls whether local username/password authentication is available.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// BootstrapAdmin configures auto-creation of a default admin user when local
	// auth is first enabled.
	// +optional
	BootstrapAdmin *BootstrapAdminConfig `json:"bootstrapAdmin,omitempty"`
}

// AuthConfigSpec defines the desired state of AuthConfig.
type AuthConfigSpec struct {
	// Enabled is the master switch for authentication. When false, the system operates without auth.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// OIDC contains the OIDC provider configuration.
	// +optional
	OIDC *OIDCConfig `json:"oidc,omitempty"`
	// Session contains session/token configuration.
	// +optional
	Session *SessionConfig `json:"session,omitempty"`
	// PersonalNamespaces configures personal namespace behavior.
	// +optional
	PersonalNamespaces *PersonalNamespaceConfig `json:"personalNamespaces,omitempty"`
	// Registration configures user registration/provisioning behavior.
	// +optional
	Registration *RegistrationConfig `json:"registration,omitempty"`
	// Authorization configures authorization behavior.
	// +optional
	Authorization *AuthorizationConfig `json:"authorization,omitempty"`
	// AdminEmails is a list of email addresses that are always granted admin role.
	// Used for bootstrapping admin access.
	// +optional
	AdminEmails []string `json:"adminEmails,omitempty"`
	// LocalAuth configures local username/password authentication. It may be
	// enabled independently of, or alongside, OIDC.
	// +optional
	LocalAuth *LocalAuthConfig `json:"localAuth,omitempty"`
}

// AuthConfigStatus defines the observed state of AuthConfig.
type AuthConfigStatus struct {
	// Enabled reflects whether auth is currently active.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// IssuerReachable indicates whether the OIDC issuer is reachable.
	// +optional
	IssuerReachable bool `json:"issuerReachable,omitempty"`
	// LastVerified is when the OIDC issuer was last verified.
	// +optional
	LastVerified *metav1.Time `json:"lastVerified,omitempty"`
	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=authconfigs,singular=authconfig,scope=Cluster
// +kubebuilder:printcolumn:name="Enabled",type="boolean",JSONPath=".spec.enabled"
// +kubebuilder:printcolumn:name="Issuer",type="string",JSONPath=".spec.oidc.issuerURL"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AuthConfig is the Schema for the authconfigs API.
// It defines the authentication and authorization configuration for kube-workspaces.
// Typically only one instance named "default" should exist.
type AuthConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AuthConfigSpec   `json:"spec,omitempty"`
	Status AuthConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AuthConfigList contains a list of AuthConfig.
type AuthConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AuthConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AuthConfig{}, &AuthConfigList{})
}
