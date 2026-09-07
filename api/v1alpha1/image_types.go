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

// ImageSpec defines the desired state of Image.
type ImageSpec struct {
	// Container image reference (e.g. "codercom/code-server:latest").
	Image string `json:"image"`
	// Display name (e.g. "Code Server (VS Code)").
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// Description of the image.
	// +optional
	Description string `json:"description,omitempty"`
	// Category groups this image for UI display (e.g. "Desktop", "IDE", "Tool", "Game").
	// +optional
	Category string `json:"category,omitempty"`
	// Tags are optional labels for filtering/searching images.
	// +optional
	Tags []string `json:"tags,omitempty"`
	// Default container port.
	DefaultPort int32 `json:"defaultPort"`
	// Default URL path for connecting (e.g. "/vnc.html?resize=remote").
	// +optional
	DefaultPath string `json:"defaultPath,omitempty"`
	// Icon identifier (e.g. "vscode", "desktop").
	// +optional
	Icon string `json:"icon,omitempty"`
	// Default command-line args injected at workspace creation.
	// +optional
	DefaultArgs []string `json:"defaultArgs,omitempty"`
	// Default environment variables injected at workspace creation.
	// Supports {{namespace}} and {{name}} placeholders.
	// +optional
	DefaultEnv []ImageEnvVar `json:"defaultEnv,omitempty"`
	// DefaultCredentials are the default login credentials for this image.
	// +optional
	DefaultCredentials *ImageCredentials `json:"defaultCredentials,omitempty"`
	// Privileged indicates that containers using this image should run in privileged mode.
	// +optional
	Privileged bool `json:"privileged,omitempty"`
	// HomepageURL is the project homepage or documentation URL.
	// +optional
	HomepageURL string `json:"homepageURL,omitempty"`
	// SourceURL is the source code repository URL.
	// +optional
	SourceURL string `json:"sourceURL,omitempty"`
	// ImageHomepageURL is the container image registry page (e.g. Docker Hub).
	// +optional
	ImageHomepageURL string `json:"imageHomepageURL,omitempty"`
	// DefaultUser is the default user for this image.
	// +optional
	DefaultUser string `json:"defaultUser,omitempty"`
	// DefaultPassword is the default password for DefaultUser. Only set when the
	// image has a known default password for this user; leave empty/unset when
	// there is no known default (e.g. the guest uses no password).
	// +optional
	DefaultPassword string `json:"defaultPassword,omitempty"`
	// DefaultCloudInit indicates the image has cloud-init baked in. When true,
	// user-data (e.g. a user/password from DefaultUser/DefaultPassword) can be
	// seeded into the guest at first boot.
	// +optional
	DefaultCloudInit bool `json:"defaultCloudInit,omitempty"`
	// DefaultUserData is reserved user-data for the guest (cloud-init). Empty by
	// default; later used to seed first-boot configuration when DefaultCloudInit
	// is true. Not yet consumed by the controller.
	// +optional
	DefaultUserData string `json:"defaultUserData,omitempty"`
	// DefaultHomedir is the default home directory for the default user.
	// +optional
	DefaultHomedir string `json:"defaultHomedir,omitempty"`
	// DefaultShell is the default shell for exec/console sessions (e.g. "/bin/bash").
	// Falls back to /bin/bash then /bin/sh if not specified.
	// +optional
	DefaultShell string `json:"defaultShell,omitempty"`
	// Links are additional relevant URLs for this image.
	// +optional
	Links []ImageLink `json:"links,omitempty"`
	// Proxy configuration for this image.
	// +optional
	ProxyConfig *ImageProxyConfig `json:"proxyConfig,omitempty"`
	// DefaultUID is the UID that the main container runs as. When set, the controller
	// will configure pod securityContext with runAsUser and fsGroup set to this value,
	// ensuring mounted volumes are accessible to the container process.
	// +optional
	DefaultUID *int64 `json:"defaultUID,omitempty"`
	// DefaultInitContainers are init containers injected into workspace pods at creation time.
	// Useful for volume permission fixups (e.g. chown to match the container's UID).
	// Supports {{namespace}}, {{name}}, and {{uid}} placeholders in args and env values.
	// +optional
	DefaultInitContainers []corev1.Container `json:"defaultInitContainers,omitempty"`
	// AdditionalPorts lists extra container ports to expose beyond the primary defaultPort.
	// These are added to the workspace pod and exposed via the Service.
	// +optional
	AdditionalPorts []ImagePort `json:"additionalPorts,omitempty"`
	// DefaultSharedMemory indicates that workspaces created from this image should
	// automatically mount /dev/shm as an emptyDir with medium=Memory. Required for
	// images that use shared memory for video encoding (e.g. Selkies/KasmVNC streaming,
	// Chrome/Chromium, ML frameworks).
	// +optional
	DefaultSharedMemory bool `json:"defaultSharedMemory,omitempty"`
	// WorkspaceTypes lists the workspace types this image can be used with
	// ("container", "vm", "scratch"). When empty, the image is offered for
	// "container" workspaces only. VM images must be containerDisk images
	// containing a bootable guest disk.
	// +optional
	WorkspaceTypes []string `json:"workspaceTypes,omitempty"`
}

// ImageLink represents a named URL link for an image.
type ImageLink struct {
	// Title is the display label for the link.
	Title string `json:"title"`
	// URL is the link target.
	URL string `json:"url"`
}

// ImageEnvVar represents an environment variable with optional placeholder support.
type ImageEnvVar struct {
	// Name of the environment variable.
	Name string `json:"name"`
	// Value supports {{namespace}} and {{name}} placeholders.
	Value string `json:"value"`
}

// ImageCredentials represents default login credentials for a workspace image.
type ImageCredentials struct {
	// Username is the default username.
	// +optional
	Username string `json:"username,omitempty"`
	// Password is the default password.
	// +optional
	Password string `json:"password,omitempty"`
}

// ImagePort represents an additional container port to expose.
type ImagePort struct {
	// Name of the port (used as Service port name).
	Name string `json:"name"`
	// Port number to expose.
	Port int32 `json:"port"`
	// Protocol (defaults to TCP).
	// +optional
	Protocol string `json:"protocol,omitempty"`
}

// ImageProxyConfig describes how the reverse proxy should behave for this image.
type ImageProxyConfig struct {
	// NeedsNoopSW: serve a no-op ServiceWorker at /sw.js to prevent SW registration errors.
	// +optional
	NeedsNoopSW bool `json:"needsNoopSW,omitempty"`
	// WebSocketPaths: paths that use WebSocket (informational — all paths support WS transparently).
	// +optional
	WebSocketPaths []string `json:"websocketPaths,omitempty"`
	// RewriteHostAbsolutePaths: rewrite requests with absolute paths that escape the proxy
	// prefix by using the Referer header to determine the target workspace.
	// +optional
	RewriteHostAbsolutePaths bool `json:"rewriteHostAbsolutePaths,omitempty"`
	// CustomRequestHeaders: additional headers to inject into proxied requests.
	// +optional
	CustomRequestHeaders map[string]string `json:"customRequestHeaders,omitempty"`
	// InjectBaseTag: inject a <base> tag into HTML responses.
	// +optional
	InjectBaseTag bool `json:"injectBaseTag,omitempty"`
	// Scheme: URL scheme the proxy uses to reach the workspace backend.
	// Defaults to "http" when empty.
	// +kubebuilder:validation:Enum=http;https
	// +optional
	Scheme string `json:"scheme,omitempty"`
	// TLSSkipVerify: when connecting over HTTPS, do not verify the backend's
	// certificate. Required for workspaces serving self-signed certificates.
	// +optional
	TLSSkipVerify bool `json:"tlsSkipVerify,omitempty"`
	// TLSInsecure: Deprecated: this conflated scheme selection with certificate
	// verification, making "HTTPS with a valid CA" impossible to express. Use
	// scheme: https plus tlsSkipVerify instead. Still honoured as a fallback when
	// scheme is unset: it implies both HTTPS and skip-verify.
	// +optional
	TLSInsecure bool `json:"tlsInsecure,omitempty"`
	// PreservePathPrefix: forward the full proxy path (including /proxy/{ns}/{name}) to the
	// workspace pod instead of stripping it. Required for apps that are configured with a
	// base URL matching the proxy prefix (e.g. filebrowser --baseurl, JupyterLab --base-url).
	// +optional
	PreservePathPrefix bool `json:"preservePathPrefix,omitempty"`
	// AudioPort: backend port for audio WebSocket connections. When set, the proxy routes
	// requests matching /audio/ to this port instead of the default workspace port.
	// +optional
	AudioPort int32 `json:"audioPort,omitempty"`
}

// ImageStatus defines the observed state of Image.
type ImageStatus struct {
	// Conditions is an array of current conditions.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=images,singular=image,scope=Cluster
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.image"
// +kubebuilder:printcolumn:name="Category",type="string",JSONPath=".spec.category"
// +kubebuilder:printcolumn:name="Port",type="integer",JSONPath=".spec.defaultPort"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Image is the Schema for the images API.
// It describes an available workspace image with its default configuration.
type Image struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImageSpec   `json:"spec,omitempty"`
	Status ImageStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ImageList contains a list of Image.
type ImageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Image `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Image{}, &ImageList{})
}
