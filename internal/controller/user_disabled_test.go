package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestDisabledUserRevokesGrantsAndPreservesResources(t *testing.T) {
	ctx := context.Background()
	scheme := resetTestScheme(t)
	user := &workspacev1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "disabled-user", UID: types.UID("original-user-uid"), Finalizers: []string{UserFinalizer}},
		Spec: workspacev1.UserSpec{Email: "user@example.com", Role: workspacev1.UserRoleEditor,
			NamespaceAccess: []workspacev1.NamespaceAccessEntry{{Namespace: "shared", Role: workspacev1.UserRoleViewer}}},
	}
	auth := &workspacev1.AuthConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: workspacev1.AuthConfigSpec{
		PersonalNamespaces: &workspacev1.PersonalNamespaceConfig{Enabled: true, ResourceQuota: map[string]string{"requests.cpu": "2"}},
	}}
	workload := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "retained-data", Namespace: user.Name}, Data: map[string]string{"value": "retained"}}
	unmanaged := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "operator-grant", Namespace: "shared", Labels: map[string]string{LabelUserName: user.Name}}}
	other := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "other-user", Namespace: "shared", Labels: map[string]string{LabelManagedByUser: ManagedByValue, LabelUserName: "someone-else"}}}
	writes := 0
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(user).WithObjects(user, auth, workload, unmanaged, other).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, name string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			return c.SubResource(name).Update(ctx, obj, opts...)
		}}).Build()
	r := &UserReconciler{Client: cl, EventRecorder: record.NewFakeRecorder(20)}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(user)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	personalKey := client.ObjectKey{Name: user.Name + "-personal", Namespace: user.Name}
	sharedKey := client.ObjectKey{Name: user.Name + "-shared", Namespace: "shared"}
	for _, key := range []client.ObjectKey{personalKey, sharedKey} {
		if err := cl.Get(ctx, key, &rbacv1.RoleBinding{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cl.Get(ctx, req.NamespacedName, user); err != nil {
		t.Fatal(err)
	}
	preservedSpec := user.DeepCopy().Spec
	user.Spec.Disabled = true
	if err := cl.Update(ctx, user); err != nil {
		t.Fatal(err)
	}
	result, err := r.Reconcile(ctx, req)
	if err != nil || result.RequeueAfter != 30*time.Second {
		t.Fatalf("disable reconcile: %v %v", result, err)
	}
	for _, key := range []client.ObjectKey{personalKey, sharedKey} {
		if err := cl.Get(ctx, key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
			t.Fatalf("grant retained: %v", err)
		}
	}
	for _, obj := range []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: user.Name}}, &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: user.Name + "-quota", Namespace: user.Name}}, workload, unmanaged, other} {
		if err := cl.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("disable removed retained %T: %v", obj, err)
		}
	}
	if err := cl.Get(ctx, req.NamespacedName, user); err != nil {
		t.Fatal(err)
	}
	if user.UID != "original-user-uid" || user.Status.PersonalNamespace != user.Name || len(user.Status.Conditions) != 1 || user.Status.Conditions[0].Reason != "Disabled" || user.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Fatalf("wrong disabled identity/status: %+v", user)
	}
	writesBefore := writes
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if writes != writesBefore {
		t.Fatal("unchanged disabled status caused another write")
	}
	// A restored managed grant is removed on the periodic disabled-user check.
	restored := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: sharedKey.Name, Namespace: sharedKey.Namespace, Labels: map[string]string{LabelManagedByUser: ManagedByValue, LabelUserName: user.Name}}}
	if err := cl.Create(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, sharedKey, restored); !apierrors.IsNotFound(err) {
		t.Fatal("restored disabled grant survived recheck")
	}
	assertReenabledUserGrants(t, r, user, preservedSpec, personalKey, sharedKey)
}

func assertReenabledUserGrants(t *testing.T, r *UserReconciler, user *workspacev1.User, preservedSpec workspacev1.UserSpec, keys ...client.ObjectKey) {
	t.Helper()
	ctx := context.Background()
	cl := r.Client
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(user)}
	if err := cl.Get(ctx, req.NamespacedName, user); err != nil {
		t.Fatal(err)
	}
	user.Spec.Disabled = false
	if !reflect.DeepEqual(user.Spec, preservedSpec) {
		t.Fatal("disablement changed desired grants or account fields")
	}
	if err := cl.Update(ctx, user); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		binding := &rbacv1.RoleBinding{}
		if err := cl.Get(ctx, key, binding); err != nil {
			t.Fatal(err)
		}
		if len(binding.Subjects) != 1 || binding.Subjects[0].Name != user.Spec.Email {
			t.Fatal("wrong restored grant subject")
		}
	}
	if err := cl.Get(ctx, req.NamespacedName, user); err != nil {
		t.Fatal(err)
	}
	if user.Status.Conditions[0].Reason != "Reconciled" || user.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Fatal("re-enable did not restore ready status")
	}
}

func TestDisabledUserRetriesFailedGrantRevocation(t *testing.T) {
	ctx := context.Background()
	user := &workspacev1.User{ObjectMeta: metav1.ObjectMeta{Name: "disabled-user", Finalizers: []string{UserFinalizer}}, Spec: workspacev1.UserSpec{Disabled: true}, Status: workspacev1.UserStatus{PersonalNamespace: "personal"}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "personal"}}
	grant := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "grant", Namespace: "personal", UID: "grant-uid", Labels: map[string]string{LabelManagedByUser: ManagedByValue, LabelUserName: user.Name}}}
	fail := true
	cl := fake.NewClientBuilder().WithScheme(resetTestScheme(t)).WithStatusSubresource(user).WithObjects(user, ns, grant).
		WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*rbacv1.RoleBinding); ok {
				options := &client.DeleteOptions{}
				for _, option := range opts {
					option.ApplyToDelete(options)
				}
				if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != obj.GetUID() || options.Preconditions.ResourceVersion == nil {
					t.Fatal("grant deletion lacks conditional identity/version")
				}
				if fail {
					return errors.New("injected revocation failure")
				}
			}
			return c.Delete(ctx, obj, opts...)
		}}).Build()
	r := &UserReconciler{Client: cl}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(user)}
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("revocation failure was ignored")
	}
	if err := cl.Get(ctx, req.NamespacedName, user); err != nil {
		t.Fatal(err)
	}
	if len(user.Status.Conditions) != 0 {
		t.Fatal("reported completed revocation after failure")
	}
	fail = false
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(grant), grant); !apierrors.IsNotFound(err) {
		t.Fatal("grant was not retried")
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(ns), ns); err != nil {
		t.Fatal("revocation removed namespace")
	}
}
