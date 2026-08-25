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
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

const (
	// LocalAuthSystemNamespace is where local-auth password Secrets are stored,
	// regardless of the user's personal namespace.
	LocalAuthSystemNamespace = "kube-workspaces-system"

	// PasswordSecretHashKey is the Secret key holding the bcrypt password hash.
	PasswordSecretHashKey = "passwordHash"
	// PasswordSecretPlaintextKey is the Secret key holding the plaintext password.
	// Only present until the user changes their password for the first time.
	PasswordSecretPlaintextKey = "password"

	// DefaultBootstrapAdminEmail is used when AuthConfig.spec.localAuth.bootstrapAdmin.email is unset.
	DefaultBootstrapAdminEmail = "admin@local"

	bcryptCost = 12

	generatedPasswordLength = 20
	passwordCharset         = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// generatePassword returns a cryptographically random password of
// generatedPasswordLength characters drawn from passwordCharset.
func generatePassword() (string, error) {
	b := make([]byte, generatedPasswordLength)
	max := big.NewInt(int64(len(passwordCharset)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("failed to generate random password: %w", err)
		}
		b[i] = passwordCharset[n.Int64()]
	}
	return string(b), nil
}

// hashPassword bcrypt-hashes the given plaintext password.
func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash password: %w", err)
	}
	return string(hash), nil
}

// slugifyEmail converts an email address into a valid Kubernetes resource name
// fragment (mirrors the slugification used by the API's user auto-provisioning).
func slugifyEmail(email string) string {
	s := strings.ToLower(email)
	s = strings.ReplaceAll(s, "@", "-at-")
	s = strings.ReplaceAll(s, ".", "-")
	s = strings.ReplaceAll(s, "_", "-")
	var result []byte
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, c)
		}
	}
	s = strings.Trim(string(result), "-")
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

// localAuthSecretName returns the conventional Secret name for a local user's
// password material, given their slugified identifier.
func localAuthSecretName(slug string) string {
	return fmt.Sprintf("kw-user-%s-local-auth", slug)
}

// createLocalAuthSecret creates the password Secret for a local user, containing
// both the bcrypt hash and (transiently) the plaintext password, and returns a
// SecretKeyRef pointing at the hash key. It is a no-op (returns the existing ref)
// if the Secret already exists, so it is safe to call repeatedly.
func (r *AuthConfigReconciler) createLocalAuthSecret(ctx context.Context, slug, plaintextPassword string) (kubeworkspacesiov1alpha1.SecretKeyRef, error) {
	name := localAuthSecretName(slug)
	ref := kubeworkspacesiov1alpha1.SecretKeyRef{Name: name, Key: PasswordSecretHashKey}

	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: LocalAuthSystemNamespace}, existing)
	if err == nil {
		// Secret already exists; never overwrite a user's password here.
		return ref, nil
	}
	if !errors.IsNotFound(err) {
		return ref, err
	}

	hash, err := hashPassword(plaintextPassword)
	if err != nil {
		return ref, err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: LocalAuthSystemNamespace,
			Labels: map[string]string{
				LabelManagedByUser: ManagedByValue,
				LabelUserName:      slug,
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			PasswordSecretHashKey:      hash,
			PasswordSecretPlaintextKey: plaintextPassword,
		},
	}
	if err := r.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
		return ref, fmt.Errorf("failed to create local-auth secret %s/%s: %w", LocalAuthSystemNamespace, name, err)
	}
	return ref, nil
}

// reconcileBootstrapAdmin ensures a default local admin User exists when local
// auth is enabled, unless explicitly skipped or a local user already exists.
// It is idempotent: it never overwrites an existing User or password Secret.
func (r *AuthConfigReconciler) reconcileBootstrapAdmin(ctx context.Context, authConfig *kubeworkspacesiov1alpha1.AuthConfig) error {
	localAuth := authConfig.Spec.LocalAuth
	if localAuth == nil || !localAuth.Enabled {
		return nil
	}
	if localAuth.BootstrapAdmin != nil && localAuth.BootstrapAdmin.Skip {
		return nil
	}

	email := DefaultBootstrapAdminEmail
	if localAuth.BootstrapAdmin != nil && localAuth.BootstrapAdmin.Email != "" {
		email = localAuth.BootstrapAdmin.Email
	}

	// Skip if any local-auth-enabled user already exists (covers the case where
	// an admin has already manually provisioned a local user).
	var userList kubeworkspacesiov1alpha1.UserList
	if err := r.List(ctx, &userList); err != nil {
		return fmt.Errorf("failed to list users: %w", err)
	}
	for _, u := range userList.Items {
		if u.Spec.LocalAuth != nil && u.Spec.LocalAuth.Enabled {
			return nil
		}
	}

	slug := slugifyEmail(email)

	// Also skip if a User with this exact name already exists (e.g. created but
	// not yet local-auth enabled), to avoid clobbering it.
	existing := &kubeworkspacesiov1alpha1.User{}
	err := r.Get(ctx, client.ObjectKey{Name: slug}, existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	password, err := generatePassword()
	if err != nil {
		return err
	}

	ref, err := r.createLocalAuthSecret(ctx, slug, password)
	if err != nil {
		return err
	}

	user := &kubeworkspacesiov1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name: slug,
			Labels: map[string]string{
				"kubeworkspaces.io/role": string(kubeworkspacesiov1alpha1.UserRoleAdmin),
			},
		},
		Spec: kubeworkspacesiov1alpha1.UserSpec{
			Email:       email,
			DisplayName: "Administrator",
			Role:        kubeworkspacesiov1alpha1.UserRoleAdmin,
			LocalAuth: &kubeworkspacesiov1alpha1.LocalAuthSpec{
				Enabled:            true,
				PasswordSecretRef:  ref,
				MustChangePassword: true,
			},
		},
	}

	if err := r.Create(ctx, user); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create bootstrap admin user: %w", err)
	}

	if r.EventRecorder != nil {
		r.EventRecorder.Event(authConfig, corev1.EventTypeNormal, "BootstrapAdminCreated",
			fmt.Sprintf("Created default local admin user %q; retrieve the generated password from secret %s/%s",
				email, LocalAuthSystemNamespace, localAuthSecretName(slug)))
	}

	return nil
}
