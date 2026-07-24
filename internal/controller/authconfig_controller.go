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
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
	authConfig.Status.Enabled = authConfig.Spec.Enabled

	// If auth is not enabled, just update status and return
	if !authConfig.Spec.Enabled {
		condition := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "Disabled",
			Message:            "Authentication is disabled",
		}
		setCondition(&authConfig.Status.Conditions, condition)
		if err := r.Status().Update(ctx, &authConfig); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Validate OIDC configuration
	if authConfig.Spec.OIDC == nil || authConfig.Spec.OIDC.IssuerURL == "" {
		condition := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "MissingOIDCConfig",
			Message:            "OIDC configuration is required when auth is enabled",
		}
		setCondition(&authConfig.Status.Conditions, condition)
		if err := r.Status().Update(ctx, &authConfig); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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

	if err := r.Status().Update(ctx, &authConfig); err != nil {
		return ctrl.Result{}, err
	}

	// Re-verify periodically
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *AuthConfigReconciler) verifyIssuer(ctx context.Context, issuerURL string) bool {
	log := logf.FromContext(ctx)

	wellKnownURL := issuerURL + "/.well-known/openid-configuration"

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: false,
			},
		},
	}

	resp, err := httpClient.Get(wellKnownURL)
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
		Complete(r)
}
