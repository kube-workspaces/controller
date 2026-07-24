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

	Context("When auth is enabled but OIDC config is missing", func() {
		It("should set Ready condition to False with MissingOIDCConfig reason", func() {
			authConfig := &kubeworkspacesiov1alpha1.AuthConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: authConfigName,
				},
				Spec: kubeworkspacesiov1alpha1.AuthConfigSpec{
					Enabled: true,
					// No OIDC config
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
			Expect(updated.Status.Conditions[0].Reason).To(Equal("MissingOIDCConfig"))
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
			Expect(updated.Status.Conditions[0].Reason).To(Equal("MissingOIDCConfig"))
		})
	})

	Context("When auth is enabled with reachable OIDC issuer", func() {
		var oidcServer *httptest.Server

		BeforeEach(func() {
			// Start a mock OIDC server
			oidcServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/.well-known/openid-configuration" {
					w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"issuer": "%s", "authorization_endpoint": "%s/auth", "token_endpoint": "%s/token"}`, //nolint:errcheck
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
