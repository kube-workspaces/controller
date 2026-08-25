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
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

var _ = Describe("AuthConfig Controller", func() {
	const authConfigName = "default"

	var (
		reconciler *AuthConfigReconciler
		recorder   *record.FakeRecorder
	)

	BeforeEach(func() {
		recorder = record.NewFakeRecorder(20)
		reconciler = &AuthConfigReconciler{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			EventRecorder: recorder,
		}
	})

	AfterEach(func() {
		// Clean up AuthConfig
		authConfig := &kubeworkspacesiov1alpha1.AuthConfig{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: authConfigName}, authConfig)
		if err == nil {
			_ = k8sClient.Delete(ctx, authConfig)
		}
	})

	Context("When auth is disabled", func() {
		It("should set Ready condition to True with Disabled reason", func() {
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigName,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: false,
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: authConfigName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			// Verify status
			updated := &kubeworkspacesiov1alpha1.AuthConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: authConfigName}, updated)).To(Succeed())
			Expect(updated.Status.Enabled).To(BeFalse())
			Expect(updated.Status.Conditions).To(HaveLen(1))
			Expect(updated.Status.Conditions[0].Type).To(Equal("Ready"))
			Expect(updated.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
			Expect(updated.Status.Conditions[0].Reason).To(Equal("Disabled"))
		})
	})

	Context("When auth is enabled but no authentication method is configured", func() {
		It("should set Ready condition to False with MissingAuthMethod reason", func() {
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigName,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: true,
					// No OIDC config, no localAuth
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: authConfigName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			// Verify status
			updated := &kubeworkspacesiov1alpha1.AuthConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: authConfigName}, updated)).To(Succeed())
			Expect(updated.Status.Enabled).To(BeTrue())
			Expect(updated.Status.Conditions).To(HaveLen(1))
			Expect(updated.Status.Conditions[0].Type).To(Equal("Ready"))
			Expect(updated.Status.Conditions[0].Status).To(Equal(metav1.ConditionFalse))
			Expect(updated.Status.Conditions[0].Reason).To(Equal("MissingAuthMethod"))
		})

		It("should detect empty issuer URL as missing config", func() {
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigName,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: true,
					OIDC: &kubeworkspacesiov1alpha1.OIDCConfig{
						IssuerURL: "", // empty
						ClientID:  "my-client",
						ClientSecret: kubeworkspacesiov1alpha1.SecretKeyRef{
							Name: "oidc-secret",
							Key:  "client-secret",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: authConfigName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			updated := &kubeworkspacesiov1alpha1.AuthConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: authConfigName}, updated)).To(Succeed())
			Expect(updated.Status.Conditions[0].Reason).To(Equal("MissingAuthMethod"))
		})
	})

	Context("When auth is enabled with only localAuth configured (no OIDC)", func() {
		It("should set Ready condition to True with LocalAuthOnly reason and create the bootstrap admin", func() {
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigName,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: true,
					LocalAuth: &kubeworkspacesiov1alpha1.LocalAuthConfig{
						Enabled: true,
					},
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: authConfigName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

			updated := &kubeworkspacesiov1alpha1.AuthConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: authConfigName}, updated)).To(Succeed())
			Expect(updated.Status.Conditions[0].Type).To(Equal("Ready"))
			Expect(updated.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
			Expect(updated.Status.Conditions[0].Reason).To(Equal("LocalAuthOnly"))

			// Bootstrap admin User should have been created
			bootstrapUser := &kubeworkspacesiov1alpha1.User{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "admin-at-local"}, bootstrapUser)).To(Succeed())
			Expect(bootstrapUser.Spec.Email).To(Equal(DefaultBootstrapAdminEmail))
			Expect(bootstrapUser.Spec.Role).To(Equal(kubeworkspacesiov1alpha1.UserRoleAdmin))
			Expect(bootstrapUser.Spec.LocalAuth).NotTo(BeNil())
			Expect(bootstrapUser.Spec.LocalAuth.Enabled).To(BeTrue())
			Expect(bootstrapUser.Spec.LocalAuth.MustChangePassword).To(BeTrue())

			// Password secret should exist with a bcrypt hash and plaintext password
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "kw-user-admin-at-local-local-auth",
				Namespace: LocalAuthSystemNamespace,
			}, secret)).To(Succeed())
			Expect(secret.Data).To(HaveKey(PasswordSecretHashKey))
			Expect(secret.Data).To(HaveKey(PasswordSecretPlaintextKey))

			// Clean up created resources
			_ = k8sClient.Delete(ctx, bootstrapUser)
			_ = k8sClient.Delete(ctx, secret)
		})

		It("should not overwrite an existing bootstrap admin on repeated reconciles", func() {
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigName,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: true,
					LocalAuth: &kubeworkspacesiov1alpha1.LocalAuthConfig{
						Enabled: true,
					},
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: authConfigName}})
			Expect(err).NotTo(HaveOccurred())

			secretBefore := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "kw-user-admin-at-local-local-auth",
				Namespace: LocalAuthSystemNamespace,
			}, secretBefore)).To(Succeed())
			hashBefore := string(secretBefore.Data[PasswordSecretHashKey])

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: authConfigName}})
			Expect(err).NotTo(HaveOccurred())

			secretAfter := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "kw-user-admin-at-local-local-auth",
				Namespace: LocalAuthSystemNamespace,
			}, secretAfter)).To(Succeed())
			Expect(string(secretAfter.Data[PasswordSecretHashKey])).To(Equal(hashBefore))

			bootstrapUser := &kubeworkspacesiov1alpha1.User{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "admin-at-local"}, bootstrapUser)).To(Succeed())

			_ = k8sClient.Delete(ctx, bootstrapUser)
			_ = k8sClient.Delete(ctx, secretAfter)
		})

		It("should skip bootstrap admin creation when Skip is set", func() {
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigName,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: true,
					LocalAuth: &kubeworkspacesiov1alpha1.LocalAuthConfig{
						Enabled: true,
						BootstrapAdmin: &kubeworkspacesiov1alpha1.BootstrapAdminConfig{
							Skip: true,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: authConfigName}})
			Expect(err).NotTo(HaveOccurred())

			bootstrapUser := &kubeworkspacesiov1alpha1.User{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "admin-at-local"}, bootstrapUser)
			Expect(err).To(HaveOccurred())
		})
	})

	Context("When auth is enabled with reachable OIDC issuer", func() {
		var oidcServer *httptest.Server

		BeforeEach(func() {
			// Start a mock OIDC server
			oidcServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/.well-known/openid-configuration" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"issuer": "%s", "authorization_endpoint": "%s/auth", "token_endpoint": "%s/token"}`,
						oidcServer.URL, oidcServer.URL, oidcServer.URL)
					return
				}
				http.NotFound(w, r)
			}))
		})

		AfterEach(func() {
			oidcServer.Close()
		})

		It("should verify issuer and set Ready condition to True", func() {
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigName,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: true,
					OIDC: &kubeworkspacesiov1alpha1.OIDCConfig{
						IssuerURL: oidcServer.URL,
						ClientID:  "test-client",
						ClientSecret: kubeworkspacesiov1alpha1.SecretKeyRef{
							Name: "oidc-secret",
							Key:  "client-secret",
						},
					},
					Session: &kubeworkspacesiov1alpha1.SessionConfig{
						SigningKey: kubeworkspacesiov1alpha1.SecretKeyRef{
							Name: "session-secret",
							Key:  "signing-key",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: authConfigName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

			// Verify status
			updated := &kubeworkspacesiov1alpha1.AuthConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: authConfigName}, updated)).To(Succeed())
			Expect(updated.Status.Enabled).To(BeTrue())
			Expect(updated.Status.IssuerReachable).To(BeTrue())
			Expect(updated.Status.LastVerified).NotTo(BeNil())
			Expect(updated.Status.Conditions).To(HaveLen(1))
			Expect(updated.Status.Conditions[0].Type).To(Equal("Ready"))
			Expect(updated.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
			Expect(updated.Status.Conditions[0].Reason).To(Equal("IssuerVerified"))
		})
	})

	Context("When auth is enabled with unreachable OIDC issuer", func() {
		It("should set IssuerReachable to false and Ready to False", func() {
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigName,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: true,
					OIDC: &kubeworkspacesiov1alpha1.OIDCConfig{
						IssuerURL: "http://localhost:19999", // unlikely to be listening
						ClientID:  "test-client",
						ClientSecret: kubeworkspacesiov1alpha1.SecretKeyRef{
							Name: "oidc-secret",
							Key:  "client-secret",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: authConfigName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

			// Verify status
			updated := &kubeworkspacesiov1alpha1.AuthConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: authConfigName}, updated)).To(Succeed())
			Expect(updated.Status.IssuerReachable).To(BeFalse())
			Expect(updated.Status.Conditions[0].Status).To(Equal(metav1.ConditionFalse))
			Expect(updated.Status.Conditions[0].Reason).To(Equal("IssuerUnreachable"))
		})

		It("should handle issuer returning non-200 status", func() {
			// Server that returns 500
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()

			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigName,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: true,
					OIDC: &kubeworkspacesiov1alpha1.OIDCConfig{
						IssuerURL: server.URL,
						ClientID:  "test-client",
						ClientSecret: kubeworkspacesiov1alpha1.SecretKeyRef{
							Name: "oidc-secret",
							Key:  "client-secret",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: authConfigName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

			updated := &kubeworkspacesiov1alpha1.AuthConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: authConfigName}, updated)).To(Succeed())
			Expect(updated.Status.IssuerReachable).To(BeFalse())
			Expect(updated.Status.Conditions[0].Reason).To(Equal("IssuerUnreachable"))
		})
	})

	Context("When reconciling non-default AuthConfig", func() {
		It("should ignore AuthConfig not named 'default'", func() {
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "not-default",
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: true,
				},
			}
			Expect(k8sClient.Create(ctx, authConfig)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "not-default"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			// Clean up
			_ = k8sClient.Delete(ctx, authConfig)
		})
	})

	Context("When AuthConfig does not exist", func() {
		It("should return without error for non-existent AuthConfig", func() {
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: authConfigName},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})
	})
})
