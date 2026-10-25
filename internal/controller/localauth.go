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
	"crypto/sha256"
	"fmt"
	"math/big"
	"strings"
	"time"

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

	bootstrapOwnerAnnotation = "kubeworkspaces.io/bootstrap-authconfig-uid"

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

// localAuthSecretName is collision-resistant even for long/colliding emails.
func localAuthSecretName(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return fmt.Sprintf("kw-password-%x", sum[:24])
}

// createLocalAuthSecret creates the password Secret for a local user, containing
// both the bcrypt hash and (transiently) the plaintext password, and returns a
// SecretKeyRef pointing at the hash key. It is a no-op (returns the existing ref)
// only if an existing Secret is verifiably owned by that same persisted UID.
func (r *AuthConfigReconciler) createLocalAuthSecret(ctx context.Context, user *kubeworkspacesiov1alpha1.User, plaintextPassword string, mustChange bool) (kubeworkspacesiov1alpha1.SecretKeyRef, error) {
	name := localAuthSecretName(string(user.UID))
	ref := kubeworkspacesiov1alpha1.SecretKeyRef{Name: name, Key: PasswordSecretHashKey}
	if user.UID == "" {
		return ref, fmt.Errorf("persisted User UID is required for bootstrap credentials")
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: LocalAuthSystemNamespace}, existing)
	if err == nil {
		owned := existing.Annotations[AnnotationUserUID] == string(user.UID) && metav1.IsControlledBy(existing, user)
		_, hashErr := bcrypt.Cost(existing.Data[PasswordSecretHashKey])
		if !owned || !existing.DeletionTimestamp.IsZero() || hashErr != nil {
			return ref, fmt.Errorf("bootstrap password Secret %s ownership or credential mismatch; refusing adoption", name)
		}
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
				LabelUserName:      user.Name,
			},
			Annotations:     map[string]string{AnnotationUserUID: string(user.UID)},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(user, kubeworkspacesiov1alpha1.GroupVersion.WithKind("User"))},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			PasswordSecretHashKey:      hash,
			PasswordSecretPlaintextKey: plaintextPassword,
			"mustChangePassword":       fmt.Sprint(mustChange),
		},
	}
	if err := r.Create(ctx, secret); err != nil {
		return ref, fmt.Errorf("failed to create local-auth secret %s/%s: %w", LocalAuthSystemNamespace, name, err)
	}
	return ref, nil
}

// reconcileBootstrapAdmin ensures a default local admin User exists when local
// auth is enabled, unless explicitly skipped or a usable local admin exists.
// It is idempotent: it never overwrites an existing User or password Secret.
func (r *AuthConfigReconciler) reconcileBootstrapAdmin(ctx context.Context, authConfig *kubeworkspacesiov1alpha1.AuthConfig) error {
	localAuth := authConfig.Spec.LocalAuth
	if localAuth == nil || !localAuth.Enabled {
		return nil
	}

	email := DefaultBootstrapAdminEmail
	mustChangePassword := true
	if localAuth.BootstrapAdmin != nil && localAuth.BootstrapAdmin.Email != "" {
		email = localAuth.BootstrapAdmin.Email
	}
	if localAuth.BootstrapAdmin != nil && localAuth.BootstrapAdmin.MustChangePassword != nil {
		mustChangePassword = *localAuth.BootstrapAdmin.MustChangePassword
	}

	// A disabled account, editor, missing hash or locked account cannot serve as
	// the installation's independent local recovery administrator.
	var userList kubeworkspacesiov1alpha1.UserList
	if err := r.List(ctx, &userList); err != nil {
		return fmt.Errorf("failed to list users: %w", err)
	}
	for _, u := range userList.Items {
		if r.usableLocalAdmin(ctx, &u, authConfig) {
			return nil
		}
	}
	if localAuth.BootstrapAdmin != nil && localAuth.BootstrapAdmin.Skip {
		return fmt.Errorf("bootstrap is skipped and no usable local administrator exists")
	}

	slug := slugifyEmail(email)

	// Resume only this AuthConfig's incomplete bootstrap transaction. A preexisting
	// account is never promoted or given credentials merely by matching its name.
	existing := &kubeworkspacesiov1alpha1.User{}
	err := r.Get(ctx, client.ObjectKey{Name: slug}, existing)
	if err == nil {
		if authConfig.UID == "" || existing.Annotations[bootstrapOwnerAnnotation] != string(authConfig.UID) || existing.Spec.Email != email || existing.Spec.Role != kubeworkspacesiov1alpha1.UserRoleAdmin || existing.Spec.Disabled || !existing.DeletionTimestamp.IsZero() || existing.Spec.LocalAuth != nil {
			return fmt.Errorf("bootstrap User %s already exists but is not a usable local administrator; recover the existing account in place", slug)
		}
	}
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	user := &kubeworkspacesiov1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        slug,
			Annotations: map[string]string{bootstrapOwnerAnnotation: string(authConfig.UID)},
			Labels: map[string]string{
				"kubeworkspaces.io/role": string(kubeworkspacesiov1alpha1.UserRoleAdmin),
			},
		},
		Spec: kubeworkspacesiov1alpha1.UserSpec{
			Email:       email,
			DisplayName: "Administrator",
			Role:        kubeworkspacesiov1alpha1.UserRoleAdmin,
		},
	}
	if errors.IsNotFound(err) {
		// Retained legacy password material requires explicit recovery, never
		// adoption by a replacement bootstrap User with a new UID.
		legacy := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Name: fmt.Sprintf("kw-user-%s-local-auth", slug), Namespace: LocalAuthSystemNamespace}, legacy); err == nil {
			return fmt.Errorf("retained legacy bootstrap credential exists; recover the original account in place")
		} else if !errors.IsNotFound(err) {
			return err
		}
		if err := r.Create(ctx, user); err != nil {
			return fmt.Errorf("failed to create bootstrap admin user: %w", err)
		}
	} else {
		user = existing
	}
	password, err := generatePassword()
	if err != nil {
		return err
	}
	ref, err := r.createLocalAuthSecret(ctx, user, password, mustChangePassword)
	if err != nil {
		return err
	}
	user.Spec.LocalAuth = &kubeworkspacesiov1alpha1.LocalAuthSpec{Enabled: true, PasswordSecretRef: ref, MustChangePassword: mustChangePassword}
	delete(user.Annotations, bootstrapOwnerAnnotation)
	if err := r.Update(ctx, user); err != nil {
		return fmt.Errorf("failed to enable bootstrap administrator: %w", err)
	}

	if r.EventRecorder != nil {
		r.EventRecorder.Event(authConfig, corev1.EventTypeNormal, "BootstrapAdminCreated",
			fmt.Sprintf("Created default local admin user %q; retrieve the generated password from secret %s/%s",
				email, LocalAuthSystemNamespace, ref.Name))
	}

	return nil
}

func (r *AuthConfigReconciler) usableLocalAdmin(ctx context.Context, user *kubeworkspacesiov1alpha1.User, cfg *kubeworkspacesiov1alpha1.AuthConfig) bool {
	if user.Spec.Disabled || !user.DeletionTimestamp.IsZero() || user.Spec.LocalAuth == nil || !user.Spec.LocalAuth.Enabled {
		return false
	}
	admin := user.Spec.Role == kubeworkspacesiov1alpha1.UserRoleAdmin
	for _, email := range cfg.Spec.AdminEmails {
		admin = admin || strings.EqualFold(email, user.Spec.Email)
	}
	if !admin || (user.Status.LockedUntil != nil && user.Status.LockedUntil.After(time.Now())) {
		return false
	}
	ref := user.Spec.LocalAuth.PasswordSecretRef
	secret := &corev1.Secret{}
	if ref.Name == "" || r.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: LocalAuthSystemNamespace}, secret) != nil || !secret.DeletionTimestamp.IsZero() {
		return false
	}
	if uid := secret.Annotations[AnnotationUserUID]; uid != "" && uid != string(user.UID) {
		return false
	}
	for _, owner := range secret.OwnerReferences {
		if owner.Kind == "User" && (owner.UID != user.UID || owner.Name != user.Name || owner.APIVersion != kubeworkspacesiov1alpha1.GroupVersion.String()) {
			return false
		}
	}
	key := ref.Key
	if key == "" {
		key = PasswordSecretHashKey
	}
	_, err := bcrypt.Cost(secret.Data[key])
	return err == nil
}
