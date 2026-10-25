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
	"errors"
	"strings"
	"testing"
	"time"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var _ = Describe("RoleBinding role changes", func() {
	It("should replace an existing admin grant with a viewer grant", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "role-change-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, ns)).To(Succeed()) })
		user := &workspacev1.User{ObjectMeta: metav1.ObjectMeta{Name: "role-change-user"}, Spec: workspacev1.UserSpec{Email: "role-change@example.com"}}
		r := &UserReconciler{Client: k8sClient}
		Expect(r.ensureRoleBinding(ctx, user, ns.Name, "workspace-admin-role", "shared")).To(Succeed())
		key := client.ObjectKey{Name: user.Name + "-shared", Namespace: ns.Name}
		before := &rbacv1.RoleBinding{}
		Expect(k8sClient.Get(ctx, key, before)).To(Succeed())
		Expect(r.ensureRoleBinding(ctx, user, ns.Name, "workspace-viewer-role", "shared")).To(Succeed())
		after := &rbacv1.RoleBinding{}
		Expect(k8sClient.Get(ctx, key, after)).To(Succeed())
		Expect(after.RoleRef.Name).To(Equal("workspace-viewer-role"))
		Expect(after.UID).NotTo(Equal(before.UID))
		Expect(after.Subjects[0].Name).To(Equal(user.Spec.Email))
		Expect(k8sClient.Delete(ctx, after)).To(Succeed())
	})
})

func TestUserCleanupRetriesFailedDeletions(t *testing.T) {
	for _, failingKind := range []string{"RoleBinding", "Namespace"} {
		t.Run(failingKind, func(t *testing.T) {
			ctx := context.Background()
			scheme := resetTestScheme(t)
			user := &workspacev1.User{
				ObjectMeta: metav1.ObjectMeta{Name: "cleanup-user", UID: "cleanup-user-uid", Finalizers: []string{UserFinalizer}},
				Status:     workspacev1.UserStatus{PersonalNamespace: "personal"},
			}
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "personal", Labels: map[string]string{LabelManagedByUser: ManagedByValue, LabelUserName: user.Name, LabelPersonalNamespace: LabelValueTrue}, Annotations: map[string]string{AnnotationUserUID: string(user.UID)}}}
			rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "cleanup-user-shared", Namespace: "shared", Labels: map[string]string{LabelManagedByUser: ManagedByValue, LabelUserName: user.Name}}}
			fail := true
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(user).WithObjects(user, ns, rb).
				WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					_, isBinding := obj.(*rbacv1.RoleBinding)
					_, isNamespace := obj.(*corev1.Namespace)
					if fail && ((failingKind == "RoleBinding" && isBinding) || (failingKind == "Namespace" && isNamespace)) {
						return errors.New("injected deletion failure")
					}
					return c.Delete(ctx, obj, opts...)
				}}).Build()
			if err := cl.Delete(ctx, user); err != nil {
				t.Fatal(err)
			}
			r := &UserReconciler{Client: cl}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(user)}
			if _, err := r.Reconcile(ctx, req); err == nil {
				t.Fatal("expected cleanup failure")
			}
			latest := &workspacev1.User{}
			if err := cl.Get(ctx, req.NamespacedName, latest); err != nil {
				t.Fatal(err)
			}
			if len(latest.Finalizers) != 1 {
				t.Fatal("cleanup failure removed the finalizer")
			}
			fail = false
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			for _, obj := range []client.Object{latest, ns, rb} {
				if err := cl.Get(ctx, client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
					t.Fatalf("expected %T to be deleted, got %v", obj, err)
				}
			}
		})
	}
}

func TestInvalidResourceQuotaReturnsError(t *testing.T) {
	scheme := resetTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &UserReconciler{Client: cl}
	user := &workspacev1.User{ObjectMeta: metav1.ObjectMeta{Name: "quota-user"}}
	err := r.ensureResourceQuota(context.Background(), user, "personal", map[string]string{"requests.memory": "not-a-quantity"})
	if err == nil || !strings.Contains(err.Error(), "requests.memory") {
		t.Fatalf("expected actionable quota error, got %v", err)
	}
}

func TestStableStatusDoesNotWrite(t *testing.T) {
	oldTime := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	for _, kind := range []string{"User", "AuthConfig", "Workspace"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			scheme := resetTestScheme(t)
			user := &workspacev1.User{ObjectMeta: metav1.ObjectMeta{Name: "stable-user", Finalizers: []string{UserFinalizer}}, Status: workspacev1.UserStatus{
				Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled", Message: "User resources are in sync", LastTransitionTime: oldTime}},
			}}
			auth := &workspacev1.AuthConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Status: workspacev1.AuthConfigStatus{
				Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Disabled", Message: "Authentication is disabled", LastTransitionTime: oldTime}},
			}}
			ws := testWorkspace(WorkspaceTypeContainer, nil)
			pod := &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: oldTime}}}}
			ws.Status.Conditions = []workspacev1.WorkspaceCondition{{Type: "Ready", Status: "True", LastTransitionTime: oldTime}}
			writes := 0
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(user, auth, ws).WithObjects(user, auth, ws).
				WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, name string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					writes++
					return c.SubResource(name).Update(ctx, obj, opts...)
				}}).Build()
			var err error
			switch kind {
			case "User":
				r := &UserReconciler{Client: cl, EventRecorder: record.NewFakeRecorder(10)}
				_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(user)})
			case "AuthConfig":
				r := &AuthConfigReconciler{Client: cl}
				_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(auth)})
			case "Workspace":
				r := &WorkspaceReconciler{Client: cl}
				err = r.updateWorkspaceStatus(ctx, ws, 0, pod)
			}
			if err != nil {
				t.Fatal(err)
			}
			if writes != 0 {
				t.Fatalf("unchanged %s status caused %d writes", kind, writes)
			}
		})
	}
}

func TestConditionTransitionTimes(t *testing.T) {
	oldTime := metav1.NewTime(time.Now().Add(-time.Hour))
	conditions := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Old", LastTransitionTime: oldTime}}
	setCondition(&conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "New"})
	if !conditions[0].LastTransitionTime.Equal(&oldTime) {
		t.Fatal("reason change reset transition time")
	}
	setCondition(&conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Failed"})
	if !conditions[0].LastTransitionTime.After(oldTime.Time) {
		t.Fatal("status change did not advance transition time")
	}
}
