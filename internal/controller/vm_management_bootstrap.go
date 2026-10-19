package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const managementBootstrapLabel = "kubeworkspaces.io/management-bootstrap"
const managementBootstrapLifetime = 15 * time.Minute

// This is a versioned management-only credential. It is never placed in the
// tenant bootstrap Secret, a cloud-init body, or the existing desktop token file.
type managementBootstrapDocument struct {
	Version         int       `json:"version"`
	Token           string    `json:"token"`
	Namespace       string    `json:"namespace"`
	WorkspaceName   string    `json:"workspace_name"`
	WorkspaceUID    string    `json:"workspace_uid"`
	Generation      string    `json:"generation"`
	RevocationEpoch uint64    `json:"revocation_epoch"`
	ExpiresAt       time.Time `json:"expires_at"`
}

func managementBootstrapSecretName(workspaceUID, vmUID string, epoch ...int64) string {
	identity := workspaceUID + "/" + vmUID
	if len(epoch) > 0 && epoch[0] != 0 {
		identity += "/" + strconv.FormatInt(epoch[0], 10)
	}
	sum := sha256.Sum256([]byte(identity))
	return "kw-mgmt-" + hex.EncodeToString(sum[:16])
}

func managementLifecyclePending(ws *workspacev1.Workspace) bool {
	for _, key := range []string{AnnotationReset, AnnotationStopped, windowsRebootAnnotation, windowsDetachingGeneration} {
		if _, present := ws.Annotations[key]; present {
			return true
		}
	}
	return false
}

func managementVMCurrent(ws *workspacev1.Workspace, vm, observedVM *unstructured.Unstructured) bool {
	running, found, err := unstructured.NestedBool(vm.Object, "spec", "running")
	owner := metav1.GetControllerOf(vm)
	if err != nil || !found || !running || vm.GetUID() == "" || vm.GetUID() != observedVM.GetUID() || !vm.GetDeletionTimestamp().IsZero() || owner == nil || owner.UID != ws.UID || owner.Name != ws.Name || owner.Kind != "Workspace" || owner.APIVersion != workspacev1.GroupVersion.String() {
		return false
	}
	return !windowsWorkspace(ws) || (ws.Spec.VMProfile.Generation != "" && ws.Annotations[windowsProvisionedGeneration] == ws.Spec.VMProfile.Generation && vm.GetAnnotations()["kubeworkspaces.io/windows-generation"] == ws.Spec.VMProfile.Generation)
}

func managementBootstrapMatches(secret *corev1.Secret, ws *workspacev1.Workspace, vm *unstructured.Unstructured, now time.Time) bool {
	if secret.Type != corev1.SecretTypeOpaque || secret.Immutable == nil || !*secret.Immutable || len(secret.OwnerReferences) != 0 || !secret.DeletionTimestamp.IsZero() || len(secret.Data) != 1 {
		return false
	}
	if secret.Labels[managementBootstrapLabel] != "v1" || secret.Labels["kubeworkspaces.io/workspace-uid"] != string(ws.UID) || secret.Labels["kubeworkspaces.io/virtualmachine-uid"] != string(vm.GetUID()) {
		return false
	}
	if secret.Annotations["kubeworkspaces.io/workspace-namespace"] != ws.Namespace || secret.Annotations["kubeworkspaces.io/workspace-name"] != ws.Name {
		return false
	}
	raw := secret.Data["bootstrap.json"]
	if len(raw) == 0 || len(raw) > 4096 {
		return false
	}
	var doc managementBootstrapDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&doc) != nil || decoder.Decode(new(interface{})) != io.EOF {
		return false
	}
	nonce, err := base64.RawURLEncoding.DecodeString(doc.Token)
	state := ws.Status.ManagementExecution
	return state != nil && state.VMUID == string(vm.GetUID()) && state.RevocationEpoch >= 0 && err == nil && len(nonce) == 32 && base64.RawURLEncoding.EncodeToString(nonce) == doc.Token && doc.Version == 1 && doc.Namespace == ws.Namespace && doc.WorkspaceName == ws.Name && doc.WorkspaceUID == string(ws.UID) && doc.Generation == string(vm.GetUID()) && doc.RevocationEpoch == uint64(state.RevocationEpoch) && !doc.ExpiresAt.IsZero() && !doc.ExpiresAt.After(now.Add(managementBootstrapLifetime))
}

// reconcileManagementBootstrap is opt-in. The namespace must be inaccessible to
// tenant identities and different from the workspace namespace. There is no
// cross-namespace owner reference: cleanup/rotation is an explicit control-plane
// responsibility. Existing expired/consumed material is never auto-renewed.
func (r *WorkspaceReconciler) reconcileManagementBootstrap(ctx context.Context, instance *workspacev1.Workspace, observedVM *unstructured.Unstructured) error {
	if r.ManagementBootstrapNamespace == "" {
		return nil
	}
	if len(validation.IsDNS1123Label(r.ManagementBootstrapNamespace)) != 0 || r.ManagementBootstrapNamespace == instance.Namespace {
		return fmt.Errorf("management bootstrap requires a separate protected namespace")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	ws := &workspacev1.Workspace{}
	key := types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}
	if err := reader.Get(ctx, key, ws); err != nil {
		return fmt.Errorf("management bootstrap workspace lookup failed")
	}
	if ws.UID == "" || ws.UID != instance.UID || !ws.DeletionTimestamp.IsZero() || workspaceTypeOf(ws) != WorkspaceTypeVM || managementLifecyclePending(ws) {
		return fmt.Errorf("management bootstrap workspace identity is not current")
	}
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(kubeVirtVirtualMachineGVK)
	if err := reader.Get(ctx, key, vm); err != nil {
		return fmt.Errorf("management bootstrap VM lookup failed")
	}
	if !managementVMCurrent(ws, vm, observedVM) {
		return fmt.Errorf("management bootstrap VM identity is not current")
	}
	state := ws.Status.ManagementExecution
	requested, err := requestedExecutionEpoch(ws)
	if err != nil || state == nil || state.VMUID != string(vm.GetUID()) || state.RevocationEpoch < 0 || requested > state.RevocationEpoch {
		return fmt.Errorf("management bootstrap execution epoch is not current")
	}
	name := managementBootstrapSecretName(string(ws.UID), string(vm.GetUID()), state.RevocationEpoch)
	secret := &corev1.Secret{}
	err = reader.Get(ctx, types.NamespacedName{Namespace: r.ManagementBootstrapNamespace, Name: name}, secret)
	if err == nil {
		if !managementBootstrapMatches(secret, ws, vm, time.Now()) {
			return fmt.Errorf("management bootstrap Secret identity conflicts")
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("management bootstrap Secret lookup failed")
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("management bootstrap entropy unavailable")
	}
	doc := managementBootstrapDocument{Version: 1, Token: base64.RawURLEncoding.EncodeToString(nonce[:]), Namespace: ws.Namespace, WorkspaceName: ws.Name, WorkspaceUID: string(ws.UID), Generation: string(vm.GetUID()), RevocationEpoch: uint64(state.RevocationEpoch), ExpiresAt: time.Now().UTC().Truncate(time.Microsecond).Add(managementBootstrapLifetime)}
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("management bootstrap encoding failed")
	}
	immutable := true
	secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.ManagementBootstrapNamespace, Labels: map[string]string{managementBootstrapLabel: "v1", "kubeworkspaces.io/workspace-uid": string(ws.UID), "kubeworkspaces.io/virtualmachine-uid": string(vm.GetUID())}, Annotations: map[string]string{"kubeworkspaces.io/workspace-namespace": ws.Namespace, "kubeworkspaces.io/workspace-name": ws.Name}}, Type: corev1.SecretTypeOpaque, Immutable: &immutable, Data: map[string][]byte{"bootstrap.json": raw}}
	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("management bootstrap Secret creation failed")
	}
	return nil
}
