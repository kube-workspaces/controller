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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

const (
	// UserFinalizer is the finalizer added to User CRs.
	UserFinalizer = "kubeworkspaces.io/user-finalizer"
	// LabelManagedByUser indicates the resource is managed by the user controller.
	LabelManagedByUser = "kubeworkspaces.io/managed-by"
	// LabelUserName is the label referencing the owning user.
	LabelUserName = "kubeworkspaces.io/user"
	// LabelPersonalNamespace marks a namespace as a personal namespace.
	LabelPersonalNamespace = "kubeworkspaces.io/personal-namespace"
	// AnnotationUserEmail stores the user's email on managed resources.
	AnnotationUserEmail = "kubeworkspaces.io/user-email"
	// AnnotationNamespaceEnabled marks a namespace as enabled for workspace filtering.
	AnnotationNamespaceEnabled = "kubeworkspaces.io/namespace-enabled"
	// ManagedByValue is the value used for the managed-by label.
	ManagedByValue = "user-controller"
	// LabelValueTrue is the string "true" used in labels.
	LabelValueTrue = "true"
)

// UserReconciler reconciles a User object.
type UserReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	EventRecorder record.EventRecorder
}

// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=users,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=users/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=users/finalizers,verbs=update
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=authconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete

func (r *UserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the User CR
	var user kubeworkspacesiov1alpha1.User
	if err := r.Get(ctx, req.NamespacedName, &user); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !user.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&user, UserFinalizer) {
			if err := r.cleanupUser(ctx, &user); err != nil {
				log.Error(err, "failed to cleanup user resources")
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&user, UserFinalizer)
			if err := r.Update(ctx, &user); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&user, UserFinalizer) {
		controllerutil.AddFinalizer(&user, UserFinalizer)
		if err := r.Update(ctx, &user); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// If user is disabled, skip reconciliation of namespaces/RBAC
	if user.Spec.Disabled {
		log.Info("user is disabled, skipping reconciliation", "user", user.Name)
		return ctrl.Result{}, nil
	}

	// Fetch AuthConfig to determine personal namespace settings
	authConfig, err := r.getAuthConfig(ctx)
	if err != nil {
		log.Error(err, "failed to get AuthConfig")
		// Continue without personal namespace creation if AuthConfig not found
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Reconcile personal namespace if enabled
	if authConfig != nil && authConfig.Spec.PersonalNamespaces != nil && authConfig.Spec.PersonalNamespaces.Enabled {
		nsName, err := r.reconcilePersonalNamespace(ctx, &user, authConfig)
		if err != nil {
			log.Error(err, "failed to reconcile personal namespace")
			r.EventRecorder.Event(&user, corev1.EventTypeWarning, "NamespaceFailed", err.Error())
			return ctrl.Result{}, err
		}

		// Update status with personal namespace
		if user.Status.PersonalNamespace != nsName {
			user.Status.PersonalNamespace = nsName
			if err := r.Status().Update(ctx, &user); err != nil {
				return ctrl.Result{}, err
			}
			r.EventRecorder.Event(&user, corev1.EventTypeNormal, "NamespaceCreated",
				fmt.Sprintf("Personal namespace %q created", nsName))
		}
	}

	// Reconcile shared namespace access
	if err := r.reconcileNamespaceAccess(ctx, &user); err != nil {
		log.Error(err, "failed to reconcile namespace access")
		r.EventRecorder.Event(&user, corev1.EventTypeWarning, "RBACFailed", err.Error())
		return ctrl.Result{}, err
	}

	// Update Ready condition
	// Re-fetch user to preserve status fields set by other actors (e.g. lastLogin from API)
	if err := r.Get(ctx, req.NamespacedName, &user); err != nil {
		return ctrl.Result{}, err
	}
	// Re-set personalNamespace in case it was just created above
	if authConfig != nil && authConfig.Spec.PersonalNamespaces != nil && authConfig.Spec.PersonalNamespaces.Enabled {
		nsName := renderNamespaceTemplate(authConfig.Spec.PersonalNamespaces.Template, user.Name)
		user.Status.PersonalNamespace = nsName
	}
	meta := metav1.Now()
	readyCondition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: meta,
		Reason:             "Reconciled",
		Message:            "User resources are in sync",
	}
	setCondition(&user.Status.Conditions, readyCondition)
	if err := r.Status().Update(ctx, &user); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *UserReconciler) getAuthConfig(ctx context.Context) (*kubeworkspacesiov1alpha1.AuthConfig, error) {
	var authConfig kubeworkspacesiov1alpha1.AuthConfig
	err := r.Get(ctx, types.NamespacedName{Name: "default"}, &authConfig)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &authConfig, nil
}

func (r *UserReconciler) reconcilePersonalNamespace(ctx context.Context, user *kubeworkspacesiov1alpha1.User, authConfig *kubeworkspacesiov1alpha1.AuthConfig) (string, error) {
	nsConfig := authConfig.Spec.PersonalNamespaces
	nsName := renderNamespaceTemplate(nsConfig.Template, user.Name)

	// Create or update namespace
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, ns, func() error {
		if ns.Labels == nil {
			ns.Labels = make(map[string]string)
		}
		ns.Labels[LabelPersonalNamespace] = LabelValueTrue
		ns.Labels[LabelUserName] = user.Name
		ns.Labels[LabelManagedByUser] = ManagedByValue

		// Apply additional labels from config
		for k, v := range nsConfig.Labels {
			ns.Labels[k] = v
		}

		if ns.Annotations == nil {
			ns.Annotations = make(map[string]string)
		}
		ns.Annotations[AnnotationUserEmail] = user.Spec.Email
		// Ensure personal namespace is enabled for workspace filtering
		ns.Annotations[AnnotationNamespaceEnabled] = LabelValueTrue
		// Apply additional annotations from config
		for k, v := range nsConfig.Annotations {
			ns.Annotations[k] = v
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to create/update namespace %s: %w", nsName, err)
	}

	log := logf.FromContext(ctx)
	log.Info("namespace reconciled", "namespace", nsName, "operation", op)

	// Create RoleBinding for user in their personal namespace
	if err := r.ensureRoleBinding(ctx, user, nsName, "workspace-editor-role", "personal"); err != nil {
		return nsName, fmt.Errorf("failed to create personal namespace RoleBinding: %w", err)
	}

	// Apply ResourceQuota if configured
	if len(nsConfig.ResourceQuota) > 0 {
		if err := r.ensureResourceQuota(ctx, user, nsName, nsConfig.ResourceQuota); err != nil {
			return nsName, fmt.Errorf("failed to create ResourceQuota: %w", err)
		}
	}

	return nsName, nil
}

func (r *UserReconciler) reconcileNamespaceAccess(ctx context.Context, user *kubeworkspacesiov1alpha1.User) error {
	for _, access := range user.Spec.NamespaceAccess {
		clusterRole := roleToClusterRole(access.Role)
		if err := r.ensureRoleBinding(ctx, user, access.Namespace, clusterRole, "shared"); err != nil {
			return fmt.Errorf("failed to create RoleBinding in %s: %w", access.Namespace, err)
		}
	}

	// Clean up stale RoleBindings in namespaces no longer in spec
	if err := r.cleanupStaleRoleBindings(ctx, user); err != nil {
		return fmt.Errorf("failed to cleanup stale RoleBindings: %w", err)
	}

	return nil
}

func (r *UserReconciler) ensureRoleBinding(ctx context.Context, user *kubeworkspacesiov1alpha1.User, namespace, clusterRole, bindingType string) error {
	rbName := fmt.Sprintf("%s-%s", user.Name, bindingType)

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rbName,
			Namespace: namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		if rb.Labels == nil {
			rb.Labels = make(map[string]string)
		}
		rb.Labels[LabelManagedByUser] = ManagedByValue
		rb.Labels[LabelUserName] = user.Name

		if rb.Annotations == nil {
			rb.Annotations = make(map[string]string)
		}
		rb.Annotations[AnnotationUserEmail] = user.Spec.Email

		rb.Subjects = []rbacv1.Subject{
			{
				Kind: "User",
				Name: user.Spec.Email,
			},
		}
		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     clusterRole,
		}
		return nil
	})
	return err
}

func (r *UserReconciler) ensureResourceQuota(ctx context.Context, user *kubeworkspacesiov1alpha1.User, namespace string, quotaSpec map[string]string) error {
	rq := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-quota", user.Name),
			Namespace: namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rq, func() error {
		if rq.Labels == nil {
			rq.Labels = make(map[string]string)
		}
		rq.Labels[LabelManagedByUser] = ManagedByValue
		rq.Labels[LabelUserName] = user.Name

		hard := make(corev1.ResourceList)
		for k, v := range quotaSpec {
			hard[corev1.ResourceName(k)] = resource.MustParse(v)
		}
		rq.Spec.Hard = hard
		return nil
	})
	return err
}

func (r *UserReconciler) cleanupStaleRoleBindings(ctx context.Context, user *kubeworkspacesiov1alpha1.User) error {
	// List all RoleBindings managed by this user
	var rbList rbacv1.RoleBindingList
	if err := r.List(ctx, &rbList, client.MatchingLabels{
		LabelManagedByUser: ManagedByValue,
		LabelUserName:      user.Name,
	}); err != nil {
		return err
	}

	// Build set of desired namespaces
	desiredNamespaces := make(map[string]bool)
	if user.Status.PersonalNamespace != "" {
		desiredNamespaces[user.Status.PersonalNamespace] = true
	}
	for _, access := range user.Spec.NamespaceAccess {
		desiredNamespaces[access.Namespace] = true
	}

	// Delete RoleBindings in namespaces not in desired set
	for i := range rbList.Items {
		rb := &rbList.Items[i]
		if !desiredNamespaces[rb.Namespace] {
			if err := r.Delete(ctx, rb); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
	}

	return nil
}

func (r *UserReconciler) cleanupUser(ctx context.Context, user *kubeworkspacesiov1alpha1.User) error {
	log := logf.FromContext(ctx)

	// Remove all RoleBindings managed by this user
	var rbList rbacv1.RoleBindingList
	if err := r.List(ctx, &rbList, client.MatchingLabels{
		LabelManagedByUser: ManagedByValue,
		LabelUserName:      user.Name,
	}); err != nil {
		return err
	}
	for i := range rbList.Items {
		if err := r.Delete(ctx, &rbList.Items[i]); err != nil && !errors.IsNotFound(err) {
			log.Error(err, "failed to delete RoleBinding", "name", rbList.Items[i].Name)
		}
	}

	// Delete personal namespace if it exists
	if user.Status.PersonalNamespace != "" {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: user.Status.PersonalNamespace,
			},
		}
		if err := r.Delete(ctx, ns); err != nil && !errors.IsNotFound(err) {
			log.Error(err, "failed to delete personal namespace", "namespace", user.Status.PersonalNamespace)
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *UserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubeworkspacesiov1alpha1.User{}).
		Owns(&corev1.Namespace{}).
		Complete(r)
}

// Helper functions

func renderNamespaceTemplate(template, username string) string {
	if template == "" {
		return username
	}
	return strings.ReplaceAll(template, "{{username}}", username)
}

func roleToClusterRole(role kubeworkspacesiov1alpha1.UserRole) string {
	switch role {
	case kubeworkspacesiov1alpha1.UserRoleAdmin:
		return "workspace-admin-role"
	case kubeworkspacesiov1alpha1.UserRoleViewer:
		return "workspace-viewer-role"
	default:
		return "workspace-editor-role"
	}
}

// setCondition sets a condition on the conditions slice, replacing any existing condition of the same type.
func setCondition(conditions *[]metav1.Condition, condition metav1.Condition) {
	if conditions == nil {
		return
	}
	for i, c := range *conditions {
		if c.Type == condition.Type {
			(*conditions)[i] = condition
			return
		}
	}
	*conditions = append(*conditions, condition)
}
