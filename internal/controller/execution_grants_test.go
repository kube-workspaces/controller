package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

var _ = Describe("VM execution grant admission schema", func() {
	It("persists controller execution epochs and rejects rollback or removal", func() {
		ws := testWorkspace(WorkspaceTypeVM, nil)
		ws.Name = ""
		ws.GenerateName = "execution-epoch-"
		ws.Namespace = "default"
		Expect(k8sClient.Create(ctx, ws)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, ws)).To(Succeed()) })
		// Revocation requests must also be monotonic: removing a request
		// before the controller acknowledges it must not re-enable old grants.
		ws.Spec.ExecutionRevocationEpoch = 3
		Expect(k8sClient.Update(ctx, ws)).To(Succeed())
		for _, epoch := range []int64{2, 0, -1} {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ws), ws)).To(Succeed())
			ws.Spec.ExecutionRevocationEpoch = epoch
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, ws))).To(BeTrue())
		}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ws), ws)).To(Succeed())
		ws.Status.ManagementExecution = &v1alpha1.ManagementExecutionStatus{VMUID: "vm-uid", RevocationEpoch: 3}
		ws.Status.Conditions = []v1alpha1.WorkspaceCondition{}
		Expect(k8sClient.Status().Update(ctx, ws)).To(Succeed())
		for _, epoch := range []int64{2, -1} {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ws), ws)).To(Succeed())
			ws.Status.ManagementExecution.RevocationEpoch = epoch
			Expect(apierrors.IsInvalid(k8sClient.Status().Update(ctx, ws))).To(BeTrue())
		}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ws), ws)).To(Succeed())
		ws.Status.ManagementExecution = nil
		Expect(apierrors.IsInvalid(k8sClient.Status().Update(ctx, ws))).To(BeTrue())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ws), ws)).To(Succeed())
		ws.Status.ManagementExecution.VMUID = "reset-vm-uid"
		ws.Status.ManagementExecution.RevocationEpoch = 4
		Expect(k8sClient.Status().Update(ctx, ws)).To(Succeed())
	})
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
