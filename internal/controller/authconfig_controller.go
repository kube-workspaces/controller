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

package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

// AuthConfigReconciler reconciles an AuthConfig object.
type AuthConfigReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	EventRecorder record.EventRecorder
}

// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=authconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=authconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=authconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=users,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create,namespace=kube-workspaces-system

func (r *AuthConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Only reconcile the "default" AuthConfig
	if req.Name != "default" {
		log.Info("ignoring non-default AuthConfig", "name", req.Name)
		return ctrl.Result{}, nil
	}

	// Fetch the AuthConfig CR
	var authConfig kubeworkspacesiov1alpha1.AuthConfig
	if err := r.Get(ctx, types.NamespacedName{Name: "default"}, &authConfig); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Update status.enabled to reflect spec
	previousStatus := authConfig.DeepCopy().Status
	updateStatus := func() error {
		if reflect.DeepEqual(previousStatus, authConfig.Status) {
			return nil
		}
		return r.Status().Update(ctx, &authConfig)
	}
	authConfig.Status.Enabled = authConfig.Spec.Enabled

	// If auth is not enabled, just update status and return
	if !authConfig.Spec.Enabled {
		authConfig.Status.IssuerReachable = false
		authConfig.Status.LastVerified = nil
		meta.RemoveStatusCondition(&authConfig.Status.Conditions, "LocalAuthReady")
		meta.RemoveStatusCondition(&authConfig.Status.Conditions, "OIDCReady")
		condition := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "Disabled",
			Message:            "Authentication is disabled",
		}
		setCondition(&authConfig.Status.Conditions, condition)
		if err := updateStatus(); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Validate that at least one authentication method is configured
	hasOIDC := authConfig.Spec.OIDC != nil && authConfig.Spec.OIDC.IssuerURL != ""
	hasLocalAuth := authConfig.Spec.LocalAuth != nil && authConfig.Spec.LocalAuth.Enabled
	if !hasOIDC && !hasLocalAuth {
		authConfig.Status.IssuerReachable = false
		authConfig.Status.LastVerified = nil
		meta.RemoveStatusCondition(&authConfig.Status.Conditions, "LocalAuthReady")
		meta.RemoveStatusCondition(&authConfig.Status.Conditions, "OIDCReady")
		condition := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "MissingAuthMethod",
			Message:            "Either OIDC or localAuth must be configured when auth is enabled",
		}
		setCondition(&authConfig.Status.Conditions, condition)
		if err := updateStatus(); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Reconcile the local-auth bootstrap admin user, independent of OIDC.
	var localErr error
	if hasLocalAuth {
		localErr = r.reconcileBootstrapAdmin(ctx, &authConfig)
		if localErr != nil {
			log.Error(localErr, "failed to reconcile bootstrap admin user")
			if r.EventRecorder != nil {
				r.EventRecorder.Event(&authConfig, "Warning", "BootstrapAdminFailed", localErr.Error())
			}
		}
	}
	methodConditions := func() {
		local := metav1.Condition{Type: "LocalAuthReady", Status: metav1.ConditionFalse, Reason: "NotConfigured", Message: "Local authentication is not configured", ObservedGeneration: authConfig.Generation}
		if hasLocalAuth {
			local.Status, local.Reason, local.Message = metav1.ConditionTrue, "UsableAdministrator", "Local authentication has a usable recovery administrator (temporary passwords require change)"
			if localErr != nil {
				local.Status, local.Reason, local.Message = metav1.ConditionFalse, "BootstrapAdminUnavailable", localErr.Error()
			}
		}
		setCondition(&authConfig.Status.Conditions, local)
		oidc := metav1.Condition{Type: "OIDCReady", Status: metav1.ConditionFalse, Reason: "NotConfigured", Message: "OIDC is not configured", ObservedGeneration: authConfig.Generation}
		if hasOIDC {
			oidc.Reason, oidc.Message = "IssuerUnreachable", "OIDC issuer is not reachable"
			if authConfig.Status.IssuerReachable {
				oidc.Status, oidc.Reason, oidc.Message = metav1.ConditionTrue, "IssuerVerified", "OIDC issuer is reachable; this does not prove a provider sign-in"
			}
		}
		setCondition(&authConfig.Status.Conditions, oidc)
	}

	// If OIDC is not configured, there is no issuer to verify; report ready
	// based on local auth alone.
	if !hasOIDC {
		authConfig.Status.IssuerReachable = false
		authConfig.Status.LastVerified = nil
		condition := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "LocalAuthOnly",
			Message:            "Authentication is enabled via local auth (no OIDC configured)",
		}
		if localErr != nil {
			condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "BootstrapAdminUnavailable", localErr.Error()
		}
		setCondition(&authConfig.Status.Conditions, condition)
		methodConditions()
		if err := updateStatus(); err != nil {
			return ctrl.Result{}, err
		}
		if localErr != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// Verify OIDC issuer is reachable
	reachable := r.verifyIssuer(ctx, authConfig.Spec.OIDC.IssuerURL)
	authConfig.Status.IssuerReachable = reachable
	now := metav1.Now()
	authConfig.Status.LastVerified = &now

	if reachable {
		condition := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "IssuerVerified",
			Message:            fmt.Sprintf("OIDC issuer %s is reachable", authConfig.Spec.OIDC.IssuerURL),
		}
		setCondition(&authConfig.Status.Conditions, condition)
	} else {
		condition := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: now,
			Reason:             "IssuerUnreachable",
			Message:            fmt.Sprintf("OIDC issuer %s is not reachable", authConfig.Spec.OIDC.IssuerURL),
		}
		setCondition(&authConfig.Status.Conditions, condition)
	}
	// An IdP outage must not hide a usable independent local recovery path.
	if !reachable && hasLocalAuth && localErr == nil {
		setCondition(&authConfig.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "LocalRecoveryAvailable", Message: "OIDC is unavailable; local recovery administrator is usable", ObservedGeneration: authConfig.Generation})
	}
	methodConditions()

	if err := updateStatus(); err != nil {
		return ctrl.Result{}, err
	}

	// Re-verify periodically
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *AuthConfigReconciler) verifyIssuer(ctx context.Context, issuerURL string) bool {
	log := logf.FromContext(ctx)

	wellKnownURL := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: false,
			},
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnownURL, nil)
	if err != nil {
		return false
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Info("OIDC issuer unreachable", "url", wellKnownURL, "error", err.Error())
		return false
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		log.Info("OIDC issuer returned non-200", "url", wellKnownURL, "status", resp.StatusCode)
		return false
	}

	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *AuthConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubeworkspacesiov1alpha1.AuthConfig{}).
		// LastVerified intentionally changes on each periodic issuer check. Do
		// not let that status write immediately trigger another issuer check.
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}
