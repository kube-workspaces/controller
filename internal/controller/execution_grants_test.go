package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

var _ = Describe("VM execution grant admission schema", func() {
	It("persists separately granted elevation without granting namespace membership", func() {
		user := &v1alpha1.User{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "execution-grants-"},
			Spec: v1alpha1.UserSpec{
				Email: "execution-schema@example.com", Role: v1alpha1.UserRoleViewer,
				ExecutionGrants: []v1alpha1.ExecutionGrant{
					{Namespace: "team", Workspaces: []string{"vm"}, Permissions: []v1alpha1.ExecutionPermission{v1alpha1.ExecutionPermissionExec}},
					{Namespace: "team", Workspaces: []string{"vm"}, Permissions: []v1alpha1.ExecutionPermission{v1alpha1.ExecutionPermissionElevated}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, user)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, user)).To(Succeed()) })
		Expect(user.Spec.ExecutionGrants).To(HaveLen(2))
		Expect(user.Spec.NamespaceAccess).To(BeEmpty())
	})

	It("rejects wildcard scope, missing or unknown permissions and duplicate set entries", func() {
		for _, grant := range []v1alpha1.ExecutionGrant{
			{Namespace: "*", Permissions: []v1alpha1.ExecutionPermission{v1alpha1.ExecutionPermissionExec}},
			{Namespace: "team"},
			{Namespace: "team", Permissions: []v1alpha1.ExecutionPermission{"workspace.*"}},
			{Namespace: "team", Permissions: []v1alpha1.ExecutionPermission{v1alpha1.ExecutionPermissionShell}, Workspaces: []string{"*"}},
			{Namespace: "team", Permissions: []v1alpha1.ExecutionPermission{v1alpha1.ExecutionPermissionShell, v1alpha1.ExecutionPermissionShell}},
			{Namespace: "team", Permissions: []v1alpha1.ExecutionPermission{v1alpha1.ExecutionPermissionShell}, Workspaces: []string{"vm", "vm"}},
		} {
			user := &v1alpha1.User{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "invalid-execution-grant-"},
				Spec:       v1alpha1.UserSpec{Email: "invalid-execution-schema@example.com", ExecutionGrants: []v1alpha1.ExecutionGrant{grant}},
			}
			err := k8sClient.Create(ctx, user)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "grant %#v unexpectedly admitted: %v", grant, err)
		}
	})
})
