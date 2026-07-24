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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

var _ = Describe("User Controller", func() {
	const (
		userName     = "test-user"
		userEmail    = "test@example.com"
		displayName  = "Test User"
		authConfigNm = "default"
	)

	var (
		reconciler *UserReconciler
		recorder   *record.FakeRecorder
	)

	BeforeEach(func() {
		recorder = record.NewFakeRecorder(20)
		reconciler = &UserReconciler{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			EventRecorder: recorder,
		}
	})

	Context("When creating a new User", func() {
		AfterEach(func() {
			// Clean up the user
			user := &kubeworkspacesiov1alpha1.User{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: userName}, user)
			if err == nil {
				// Remove finalizer for clean deletion in tests
				user.Finalizers = nil
				_ = k8sClient.Update(ctx, user)
				_ = k8sClient.Delete(ctx, user)
			}
			// Clean up AuthConfig
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: authConfigNm}, authConfig)
			if err == nil {
				_ = k8sClient.Delete(ctx, authConfig)
			}
		})

		It("should add a finalizer on first reconcile", func() {
			user := &kubeworkspacesiov1alpha1.User{
				ObjectMeta: metav1.ObjectMeta{
					Name: userName,
				},
				Spec: kubeworkspacesiov1alpha1.UserSpec{
					Email:       userEmail,
					DisplayName: displayName,
					Role:        kubeworkspacesiov1alpha1.UserRoleEditor,
				},
			}
			Expect(k8sClient.Create(ctx, user)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: userName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeTrue())

			// Verify finalizer was added
			updatedUser := &kubeworkspacesiov1alpha1.User{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userName}, updatedUser)).To(Succeed())
			Expect(updatedUser.Finalizers).To(ContainElement(UserFinalizer))
		})

		It("should skip reconciliation for disabled users", func() {
			// Create AuthConfig first so reconcile doesn't error
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigNm,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: false,
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			user := &kubeworkspacesiov1alpha1.User{
				ObjectMeta: metav1.ObjectMeta{
					Name:       userName,
					Finalizers: []string{UserFinalizer},
				},
				Spec: kubeworkspacesiov1alpha1.UserSpec{
					Email:    userEmail,
					Role:     kubeworkspacesiov1alpha1.UserRoleEditor,
					Disabled: true,
				},
			}
			Expect(k8sClient.Create(ctx, user)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: userName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
		})

		It("should return not-found error gracefully for non-existent user", func() {
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "nonexistent-user"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})
	})

	Context("When AuthConfig has personal namespaces enabled", func() {
		const nsTemplate = "ws-{{username}}"

		BeforeEach(func() {
			// Create the workspace-editor-role ClusterRole (required by the controller)
			clusterRole := &rbacv1.ClusterRole{
				ObjectMeta: metav1.ObjectMeta{
					Name: "workspace-editor-role",
				},
				Rules: []rbacv1.PolicyRule{
					{
						APIGroups: []string{"kubeworkspaces.io"},
						Resources: []string{"workspaces"},
						Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
					},
				},
			}
			err := k8sClient.Create(ctx, clusterRole)
			if err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		})

		AfterEach(func() {
			// Clean up user
			user := &kubeworkspacesiov1alpha1.User{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: userName}, user)
			if err == nil {
				user.Finalizers = nil
				_ = k8sClient.Update(ctx, user)
				_ = k8sClient.Delete(ctx, user)
			}
			// Clean up AuthConfig
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: authConfigNm}, authConfig)
			if err == nil {
				_ = k8sClient.Delete(ctx, authConfig)
			}
			// Clean up namespace
			ns := &corev1.Namespace{}
			expectedNs := "ws-" + userName
			err = k8sClient.Get(ctx, types.NamespacedName{Name: expectedNs}, ns)
			if err == nil {
				_ = k8sClient.Delete(ctx, ns)
			}
			// Clean up ClusterRole
			cr := &rbacv1.ClusterRole{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "workspace-editor-role"}, cr)
			if err == nil {
				_ = k8sClient.Delete(ctx, cr)
			}
		})

		It("should create a personal namespace and RoleBinding", func() {
			// Create AuthConfig with personal namespaces enabled
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigNm,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: true,
					PersonalNamespaces: &kubeworkspacesiov1alpha1.PersonalNamespaceConfig{
						Enabled:  true,
						Template: nsTemplate,
					},
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			// Create User with finalizer already set (to avoid the requeue-for-finalizer cycle)
			user := &kubeworkspacesiov1alpha1.User{
				ObjectMeta: metav1.ObjectMeta{
					Name:       userName,
					Finalizers: []string{UserFinalizer},
				},
				Spec: kubeworkspacesiov1alpha1.UserSpec{
					Email:       userEmail,
					DisplayName: displayName,
					Role:        kubeworkspacesiov1alpha1.UserRoleEditor,
				},
			}
			Expect(k8sClient.Create(ctx, user)).To(Succeed())

			// Reconcile
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: userName},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify namespace was created
			expectedNs := "ws-" + userName
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: expectedNs}, ns)).To(Succeed())
			Expect(ns.Labels[LabelPersonalNamespace]).To(Equal("true"))
			Expect(ns.Labels[LabelUserName]).To(Equal(userName))
			Expect(ns.Annotations[AnnotationUserEmail]).To(Equal(userEmail))

			// Verify RoleBinding was created
			rb := &rbacv1.RoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      userName + "-personal",
				Namespace: expectedNs,
			}, rb)).To(Succeed())
			Expect(rb.RoleRef.Name).To(Equal("workspace-editor-role"))
			Expect(rb.Subjects).To(HaveLen(1))
			Expect(rb.Subjects[0].Name).To(Equal(userEmail))

			// Verify user status was updated
			updatedUser := &kubeworkspacesiov1alpha1.User{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userName}, updatedUser)).To(Succeed())
			Expect(updatedUser.Status.PersonalNamespace).To(Equal(expectedNs))
		})

		It("should create a ResourceQuota when configured", func() {
			// Use a different user name to avoid namespace collision with previous test
			const quotaUser = "quota-user"
			const quotaEmail = "quota@example.com"
			quotaNsTemplate := "ws-{{username}}"

			// Create AuthConfig with ResourceQuota
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigNm,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: true,
					PersonalNamespaces: &kubeworkspacesiov1alpha1.PersonalNamespaceConfig{
						Enabled:  true,
						Template: quotaNsTemplate,
						ResourceQuota: map[string]string{
							"pods":            "10",
							"requests.cpu":    "4",
							"requests.memory": "8Gi",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			user := &kubeworkspacesiov1alpha1.User{
				ObjectMeta: metav1.ObjectMeta{
					Name:       quotaUser,
					Finalizers: []string{UserFinalizer},
				},
				Spec: kubeworkspacesiov1alpha1.UserSpec{
					Email: quotaEmail,
					Role:  kubeworkspacesiov1alpha1.UserRoleEditor,
				},
			}
			Expect(k8sClient.Create(ctx, user)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: quotaUser},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify ResourceQuota
			expectedNs := "ws-" + quotaUser
			rq := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      quotaUser + "-quota",
				Namespace: expectedNs,
			}, rq)).To(Succeed())
			Expect(rq.Spec.Hard).To(HaveKey(corev1.ResourceName("pods")))
			Expect(rq.Spec.Hard).To(HaveKey(corev1.ResourceName("requests.cpu")))
			Expect(rq.Spec.Hard).To(HaveKey(corev1.ResourceName("requests.memory")))

			// Clean up
			user.Finalizers = nil
			_ = k8sClient.Update(ctx, user)
			_ = k8sClient.Delete(ctx, user)
		})
	})

	Context("When reconciling namespace access", func() {
		const sharedNs = "shared-workspace"

		BeforeEach(func() {
			// Create the shared namespace
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: sharedNs,
				},
			}
			err := k8sClient.Create(ctx, ns)
			if err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			// Create workspace ClusterRoles
			for _, roleName := range []string{"workspace-admin-role", "workspace-editor-role", "workspace-viewer-role"} {
				cr := &rbacv1.ClusterRole{
					ObjectMeta: metav1.ObjectMeta{
						Name: roleName,
					},
					Rules: []rbacv1.PolicyRule{
						{
							APIGroups: []string{"kubeworkspaces.io"},
							Resources: []string{"workspaces"},
							Verbs:     []string{"get", "list"},
						},
					},
				}
				err := k8sClient.Create(ctx, cr)
				if err != nil && !errors.IsAlreadyExists(err) {
					Expect(err).NotTo(HaveOccurred())
				}
			}
		})

		AfterEach(func() {
			// Clean up user
			user := &kubeworkspacesiov1alpha1.User{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: userName}, user)
			if err == nil {
				user.Finalizers = nil
				_ = k8sClient.Update(ctx, user)
				_ = k8sClient.Delete(ctx, user)
			}
			// Clean up AuthConfig
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: authConfigNm}, authConfig)
			if err == nil {
				_ = k8sClient.Delete(ctx, authConfig)
			}
			// Clean up RoleBindings
			rbList := &rbacv1.RoleBindingList{}
			_ = k8sClient.List(ctx, rbList, client.InNamespace(sharedNs))
			for i := range rbList.Items {
				_ = k8sClient.Delete(ctx, &rbList.Items[i])
			}
			// Clean up namespace
			ns := &corev1.Namespace{}
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: sharedNs}, ns)
			// Note: envtest may not actually delete namespaces, but we try
			// Clean up ClusterRoles
			for _, roleName := range []string{"workspace-admin-role", "workspace-editor-role", "workspace-viewer-role"} {
				cr := &rbacv1.ClusterRole{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: roleName}, cr)
				if err == nil {
					_ = k8sClient.Delete(ctx, cr)
				}
			}
		})

		It("should create RoleBindings for shared namespace access", func() {
			// Create minimal AuthConfig (auth disabled, no personal namespaces)
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigNm,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: false,
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			// Create User with namespace access
			user := &kubeworkspacesiov1alpha1.User{
				ObjectMeta: metav1.ObjectMeta{
					Name:       userName,
					Finalizers: []string{UserFinalizer},
				},
				Spec: kubeworkspacesiov1alpha1.UserSpec{
					Email: userEmail,
					Role:  kubeworkspacesiov1alpha1.UserRoleEditor,
					NamespaceAccess: []kubeworkspacesiov1alpha1.NamespaceAccessEntry{
						{
							Namespace: sharedNs,
							Role:      kubeworkspacesiov1alpha1.UserRoleViewer,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: userName},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify RoleBinding was created in the shared namespace
			rb := &rbacv1.RoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      userName + "-shared",
				Namespace: sharedNs,
			}, rb)).To(Succeed())
			Expect(rb.RoleRef.Name).To(Equal("workspace-viewer-role"))
			Expect(rb.Subjects[0].Name).To(Equal(userEmail))
		})

		It("should map admin role to workspace-admin-role ClusterRole", func() {
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigNm,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: false,
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			user := &kubeworkspacesiov1alpha1.User{
				ObjectMeta: metav1.ObjectMeta{
					Name:       userName,
					Finalizers: []string{UserFinalizer},
				},
				Spec: kubeworkspacesiov1alpha1.UserSpec{
					Email: userEmail,
					Role:  kubeworkspacesiov1alpha1.UserRoleAdmin,
					NamespaceAccess: []kubeworkspacesiov1alpha1.NamespaceAccessEntry{
						{
							Namespace: sharedNs,
							Role:      kubeworkspacesiov1alpha1.UserRoleAdmin,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: userName},
			})
			Expect(err).NotTo(HaveOccurred())

			rb := &rbacv1.RoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      userName + "-shared",
				Namespace: sharedNs,
			}, rb)).To(Succeed())
			Expect(rb.RoleRef.Name).To(Equal("workspace-admin-role"))
		})
	})

	Context("When handling user deletion", func() {
		const personalNs = "ws-delete-user"

		BeforeEach(func() {
			// Create workspace-editor-role
			cr := &rbacv1.ClusterRole{
				ObjectMeta: metav1.ObjectMeta{
					Name: "workspace-editor-role",
				},
				Rules: []rbacv1.PolicyRule{
					{
						APIGroups: []string{"kubeworkspaces.io"},
						Resources: []string{"workspaces"},
						Verbs:     []string{"get", "list"},
					},
				},
			}
			err := k8sClient.Create(ctx, cr)
			if err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		})

		AfterEach(func() {
			// Clean up user
			user := &kubeworkspacesiov1alpha1.User{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "delete-user"}, user)
			if err == nil {
				user.Finalizers = nil
				_ = k8sClient.Update(ctx, user)
				_ = k8sClient.Delete(ctx, user)
			}
			// Clean up ClusterRole
			cr := &rbacv1.ClusterRole{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "workspace-editor-role"}, cr)
			if err == nil {
				_ = k8sClient.Delete(ctx, cr)
			}
		})

		It("should clean up resources when user is deleted", func() {
			// Create a namespace simulating a personal namespace
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: personalNs,
					Labels: map[string]string{
						LabelPersonalNamespace: "true",
						LabelUserName:          "delete-user",
					},
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			// Create a RoleBinding in that namespace
			rb := &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "delete-user-personal",
					Namespace: personalNs,
					Labels: map[string]string{
						LabelManagedByUser: "user-controller",
						LabelUserName:      "delete-user",
					},
				},
				Subjects: []rbacv1.Subject{
					{Kind: "User", Name: "delete@example.com"},
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "ClusterRole",
					Name:     "workspace-editor-role",
				},
			}
			Expect(k8sClient.Create(ctx, rb)).To(Succeed())

			// Create User with deletion timestamp (simulate delete by marking for deletion)
			user := &kubeworkspacesiov1alpha1.User{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "delete-user",
					Finalizers: []string{UserFinalizer},
				},
				Spec: kubeworkspacesiov1alpha1.UserSpec{
					Email: "delete@example.com",
					Role:  kubeworkspacesiov1alpha1.UserRoleEditor,
				},
				Status: kubeworkspacesiov1alpha1.UserStatus{
					PersonalNamespace: personalNs,
				},
			}
			Expect(k8sClient.Create(ctx, user)).To(Succeed())

			// Update status (status subresource)
			user.Status.PersonalNamespace = personalNs
			Expect(k8sClient.Status().Update(ctx, user)).To(Succeed())

			// Delete the user (triggers finalizer)
			Expect(k8sClient.Delete(ctx, user)).To(Succeed())

			// Reconcile should process the deletion
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "delete-user"},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify RoleBinding was deleted
			deletedRb := &rbacv1.RoleBinding{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      "delete-user-personal",
				Namespace: personalNs,
			}, deletedRb)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			// Verify the user's finalizer was removed
			deletedUser := &kubeworkspacesiov1alpha1.User{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "delete-user"}, deletedUser)
			// User should be fully gone since finalizer was removed and delete was pending
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("Helper function tests", func() {
		It("should render namespace template correctly", func() {
			Expect(renderNamespaceTemplate("{{username}}", "john")).To(Equal("john"))
			Expect(renderNamespaceTemplate("ws-{{username}}", "john")).To(Equal("ws-john"))
			Expect(renderNamespaceTemplate("", "john")).To(Equal("john"))
			Expect(renderNamespaceTemplate("user-{{username}}-ns", "alice")).To(Equal("user-alice-ns"))
		})

		It("should map roles to correct ClusterRole names", func() {
			Expect(roleToClusterRole(kubeworkspacesiov1alpha1.UserRoleAdmin)).To(Equal("workspace-admin-role"))
			Expect(roleToClusterRole(kubeworkspacesiov1alpha1.UserRoleEditor)).To(Equal("workspace-editor-role"))
			Expect(roleToClusterRole(kubeworkspacesiov1alpha1.UserRoleViewer)).To(Equal("workspace-viewer-role"))
		})

		It("should set conditions correctly", func() {
			conditions := []metav1.Condition{}
			condition := metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
				Reason:             "TestReason",
				Message:            "test message",
			}

			// Add new condition
			setCondition(&conditions, condition)
			Expect(conditions).To(HaveLen(1))
			Expect(conditions[0].Reason).To(Equal("TestReason"))

			// Update existing condition
			updatedCondition := metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "UpdatedReason",
				Message:            "updated message",
			}
			setCondition(&conditions, updatedCondition)
			Expect(conditions).To(HaveLen(1))
			Expect(conditions[0].Reason).To(Equal("UpdatedReason"))
			Expect(conditions[0].Status).To(Equal(metav1.ConditionFalse))
		})
	})
})

// ignoreNotFound is a helper for test cleanup that ignores not-found errors.
func ignoreNotFound(err error) error {
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// Ensure unused import is used.
var _ = context.Background
