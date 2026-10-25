package controller

import (
	"context"
	"testing"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPersonalNamespaceOwnershipAndRecovery(t *testing.T) {
	for _, mode := range []string{"unmanaged", "other-name", "replacement-uid", "legacy", "owned"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			user := &workspacev1.User{ObjectMeta: metav1.ObjectMeta{Name: "owner", UID: "original-uid"}, Spec: workspacev1.UserSpec{Email: "owner@example.com"}, Status: workspacev1.UserStatus{PersonalNamespace: "retained"}}
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "retained", Labels: map[string]string{LabelManagedByUser: ManagedByValue, LabelUserName: user.Name, LabelPersonalNamespace: LabelValueTrue}, Annotations: map[string]string{AnnotationUserEmail: user.Spec.Email, AnnotationUserUID: string(user.UID)}}}
			switch mode {
			case "unmanaged":
				ns.Labels = nil
			case "other-name":
				ns.Labels[LabelUserName] = "someone-else"
			case "replacement-uid":
				ns.Annotations[AnnotationUserUID] = "old-uid"
			case "legacy":
				delete(ns.Annotations, AnnotationUserUID)
			}
			cl := fake.NewClientBuilder().WithScheme(resetTestScheme(t)).WithObjects(ns).Build()
			r := &UserReconciler{Client: cl}
			cfg := &workspacev1.AuthConfig{Spec: workspacev1.AuthConfigSpec{PersonalNamespaces: &workspacev1.PersonalNamespaceConfig{Enabled: true, Template: "changed-{{username}}", Labels: map[string]string{LabelUserName: "spoof"}, Annotations: map[string]string{AnnotationUserUID: "spoof"}}}}
			name, err := r.reconcilePersonalNamespace(ctx, user, cfg)
			allowed := mode == "legacy" || mode == "owned"
			if (err == nil) != allowed {
				t.Fatalf("ownership result: %v", err)
			}
			if allowed {
				if name != "retained" {
					t.Fatal("template change moved the personal namespace")
				}
				if err := cl.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
					t.Fatal(err)
				}
				if ns.Annotations[AnnotationUserUID] != string(user.UID) || ns.Labels[LabelUserName] != user.Name {
					t.Fatal("config overrode reserved ownership metadata")
				}
			} else {
				var grants rbacv1.RoleBindingList
				if err := cl.List(ctx, &grants); err != nil || len(grants.Items) != 0 {
					t.Fatal("ownership denial created access grants")
				}
				if err := r.cleanupUser(ctx, user); err != nil {
					t.Fatal(err)
				}
				if err := cl.Get(ctx, client.ObjectKeyFromObject(ns), &corev1.Namespace{}); err != nil {
					t.Fatal("cleanup deleted a namespace owned by someone else")
				}
			}
		})
	}
}

func TestBootstrapCollisionAndMixedOutageReadiness(t *testing.T) {
	const skip = "skip"
	for _, mode := range []string{"orphan-secret", "editor", "disabled-admin", "usable-admin", "skip"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			cfg := &workspacev1.AuthConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: workspacev1.AuthConfigSpec{Enabled: true, LocalAuth: &workspacev1.LocalAuthConfig{Enabled: true}, OIDC: &workspacev1.OIDCConfig{IssuerURL: "http://127.0.0.1:1"}}}
			hash, err := hashPassword("Original-password-123!")
			if err != nil {
				t.Fatal(err)
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kw-user-admin-at-local-local-auth", Namespace: LocalAuthSystemNamespace}, Data: map[string][]byte{PasswordSecretHashKey: []byte(hash)}}
			objects := []client.Object{cfg, secret}
			if mode != "orphan-secret" && mode != skip {
				u := &workspacev1.User{ObjectMeta: metav1.ObjectMeta{Name: "admin-at-local", UID: "admin-uid"}, Spec: workspacev1.UserSpec{Email: DefaultBootstrapAdminEmail, Role: workspacev1.UserRoleAdmin, LocalAuth: &workspacev1.LocalAuthSpec{Enabled: true, PasswordSecretRef: workspacev1.SecretKeyRef{Name: secret.Name, Key: PasswordSecretHashKey}}}}
				if mode == "editor" {
					u.Spec.Role = workspacev1.UserRoleEditor
				}
				if mode == "disabled-admin" {
					u.Spec.Disabled = true
				}
				objects = append(objects, u)
			}
			if mode == skip {
				cfg.Spec.LocalAuth.BootstrapAdmin = &workspacev1.BootstrapAdminConfig{Skip: true}
			}
			cl := fake.NewClientBuilder().WithScheme(resetTestScheme(t)).WithStatusSubresource(cfg).WithObjects(objects...).Build()
			r := &AuthConfigReconciler{Client: cl}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "default"}}); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(cfg), cfg); err != nil {
				t.Fatal(err)
			}
			ready := mode == "usable-admin"
			if meta.IsStatusConditionTrue(cfg.Status.Conditions, "LocalAuthReady") != ready || meta.IsStatusConditionTrue(cfg.Status.Conditions, "Ready") != ready || meta.IsStatusConditionTrue(cfg.Status.Conditions, "OIDCReady") {
				t.Fatalf("incorrect mixed outage/bootstrap status: %+v", cfg.Status)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil || string(secret.Data[PasswordSecretHashKey]) != hash {
				t.Fatal("bootstrap changed existing credential material")
			}
			var users workspacev1.UserList
			if err := cl.List(ctx, &users); err != nil {
				t.Fatal(err)
			}
			if (mode == "orphan-secret" || mode == skip) && len(users.Items) != 0 {
				t.Fatal("bootstrap adopted an orphan credential")
			}
		})
	}
}

func TestBootstrapResumesOnlyItsOwnPendingUID(t *testing.T) {
	const resume = "resume"
	for _, mode := range []string{"resume", "other-config", "other-secret-uid"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			cfg := &workspacev1.AuthConfig{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "config-uid"}, Spec: workspacev1.AuthConfigSpec{Enabled: true, LocalAuth: &workspacev1.LocalAuthConfig{Enabled: true}}}
			user := &workspacev1.User{ObjectMeta: metav1.ObjectMeta{Name: "admin-at-local", UID: "pending-user-uid", Annotations: map[string]string{bootstrapOwnerAnnotation: string(cfg.UID)}}, Spec: workspacev1.UserSpec{Email: DefaultBootstrapAdminEmail, Role: workspacev1.UserRoleAdmin}}
			if mode == "other-config" {
				user.Annotations[bootstrapOwnerAnnotation] = "old-config-uid"
			}
			hash, err := hashPassword("Pending-password-123!")
			if err != nil {
				t.Fatal(err)
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: localAuthSecretName(string(user.UID)), Namespace: LocalAuthSystemNamespace, Annotations: map[string]string{AnnotationUserUID: string(user.UID)}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(user, workspacev1.GroupVersion.WithKind("User"))}}, Data: map[string][]byte{PasswordSecretHashKey: []byte(hash)}}
			if mode == "other-secret-uid" {
				secret.Annotations[AnnotationUserUID] = "another-user-uid"
			}
			cl := fake.NewClientBuilder().WithScheme(resetTestScheme(t)).WithObjects(cfg, user, secret).Build()
			r := &AuthConfigReconciler{Client: cl}
			err = r.reconcileBootstrapAdmin(ctx, cfg)
			if (err == nil) != (mode == resume) {
				t.Fatalf("bootstrap ownership result: %v", err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(user), user); err != nil {
				t.Fatal(err)
			}
			if mode == resume && (user.UID != "pending-user-uid" || user.Spec.LocalAuth == nil || user.Spec.LocalAuth.PasswordSecretRef.Name != secret.Name) {
				t.Fatal("resume did not preserve original pending identity/credential")
			}
			if mode != resume && user.Spec.LocalAuth != nil {
				t.Fatal("bootstrap enabled another transaction's credential")
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil || string(secret.Data[PasswordSecretHashKey]) != hash {
				t.Fatal("bootstrap rewrote existing secret")
			}
		})
	}
}
