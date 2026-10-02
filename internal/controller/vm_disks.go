package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path"
	"sort"
	"strings"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const vmDiskTypeLabel = "kubeworkspaces.io/volume-type"
const vmDiskOwnerAnnotation = "kubeworkspaces.io/disk-workspace"

var vmDataVolumeGVK = schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume"}

func vmDiskSerial(name string) string {
	hash := sha256.Sum256([]byte(name))
	return fmt.Sprintf("kw%x", hash[:9]) // virtio serial: at most 20 characters.
}

func validGuestMountPath(value string) bool {
	if !path.IsAbs(value) || path.Clean(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, protected := range []string{"/", "/boot", "/dev", "/proc", "/sys", "/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/run"} {
		if value == protected || (protected != "/" && strings.HasPrefix(value, protected+"/")) {
			return false
		}
	}
	return true
}

// claimVMDisks serializes writable ownership with resourceVersion updates.
// Claims survive stop/reset, but a deleted Workspace's claim can be reclaimed
// once its VMI is gone. The DataVolume has no Workspace/VM owner reference.
func (r *WorkspaceReconciler) claimVMDisks(ctx context.Context, ws *kubeworkspacesiov1alpha1.Workspace) error {
	if len(ws.Spec.Template.Spec.Containers) == 0 {
		return nil
	}
	mounts := []string{}
	for _, mount := range ws.Spec.Template.Spec.Containers[0].VolumeMounts {
		mounts = append(mounts, mount.Name)
	}
	sort.Strings(mounts)
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	for _, name := range mounts {
		if err := r.claimVMDataVolume(ctx, reader, ws, name); err != nil {
			return err
		}
	}
	return nil
}

func (r *WorkspaceReconciler) claimVMDataVolume(ctx context.Context, reader client.Reader, ws *kubeworkspacesiov1alpha1.Workspace, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		disk := &unstructured.Unstructured{}
		disk.SetGroupVersionKind(vmDataVolumeGVK)
		if err := reader.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: name}, disk); err != nil {
			return fmt.Errorf("cannot get VM disk %s: %w", name, err)
		}
		_, blank, _ := unstructured.NestedMap(disk.Object, "spec", "source", "blank")
		if disk.GetLabels()[vmDiskTypeLabel] != "vm-disk" || !blank || !disk.GetDeletionTimestamp().IsZero() {
			return fmt.Errorf("%s must be a non-terminating platform-created blank CDI disk", name)
		}
		annotations := disk.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		owner := ws.Name + "/" + string(ws.UID)
		if current := annotations[vmDiskOwnerAnnotation]; current != "" && current != owner {
			parts := strings.SplitN(current, "/", 2)
			other := &kubeworkspacesiov1alpha1.Workspace{}
			err := reader.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: parts[0]}, other)
			if err == nil && (len(parts) != 2 || string(other.UID) == parts[1]) {
				return fmt.Errorf("VM disk %s is owned by workspace %s", name, parts[0])
			}
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			vmi := &unstructured.Unstructured{}
			vmi.SetGroupVersionKind(kubeVirtVirtualMachineInstanceGVK)
			if err := reader.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: parts[0]}, vmi); err == nil {
				return fmt.Errorf("VM disk %s is still used by VMI %s", name, parts[0])
			} else if !apierrors.IsNotFound(err) {
				return err
			}
		}
		if annotations[vmDiskOwnerAnnotation] == owner {
			return nil
		}
		annotations[vmDiskOwnerAnnotation] = owner
		disk.SetAnnotations(annotations)
		return r.Update(ctx, disk)
	})
}

// vmCloudInitUserData mounts only platform-created blank disks. blkid's
// no-signature exit code (2) is the only condition permitting mkfs. Existing
// ext4 data is mounted as-is; partitioned/other filesystem disks are rejected.
func vmCloudInitUserData(ws *kubeworkspacesiov1alpha1.Workspace, img *kubeworkspacesiov1alpha1.Image, keys []string) (string, error) {
	data := cloudInitUserData(img, keys)
	if len(ws.Spec.Template.Spec.Containers) == 0 || len(ws.Spec.Template.Spec.Containers[0].VolumeMounts) == 0 {
		return data, nil
	}
	if img == nil || (!img.Spec.DefaultCloudInit && img.Spec.DefaultUserData == "") {
		return "", fmt.Errorf("VM disk mounting requires a cloud-init capable Image")
	}
	body := map[string]interface{}{}
	if data != "" {
		if !strings.HasPrefix(strings.TrimSpace(data), "#cloud-config") {
			return "", fmt.Errorf("VM disk mounting requires cloud-config user-data")
		}
		if err := yaml.Unmarshal([]byte(data), &body); err != nil {
			return "", err
		}
	}
	commands, _ := body["bootcmd"].([]interface{})
	names, paths := map[string]bool{}, map[string]bool{}
	for _, mount := range ws.Spec.Template.Spec.Containers[0].VolumeMounts {
		if len(validation.IsDNS1123Label(mount.Name)) != 0 || names[mount.Name] || !validGuestMountPath(mount.MountPath) || paths[mount.MountPath] {
			return "", fmt.Errorf("VM disk names and clean guest mount paths must be valid and unique")
		}
		names[mount.Name], paths[mount.MountPath] = true, true
		device := "/dev/disk/by-id/virtio-" + vmDiskSerial(mount.Name)
		ownership := ""
		if img.Spec.DefaultUser != "" {
			ownership = fmt.Sprintf("if [ \"$formatted\" = true ] && id %s >/dev/null 2>&1; then chown %s \"$target\"; fi\n", shellQuote(img.Spec.DefaultUser), shellQuote(img.Spec.DefaultUser))
		}
		commands = append(commands, fmt.Sprintf(`set -eu
device=%s
target=%s
formatted=false
udevadm settle
test -b "$device"
if blkid -p "$device" >/dev/null 2>&1; then
  test "$(blkid -p -s TYPE -o value "$device")" = ext4
else
  status=$?
  test "$status" = 2
  mkfs.ext4 -F "$device"
  formatted=true
fi
mkdir -p "$target"
if ! mountpoint -q "$target"; then
  mount -t ext4 "$device" "$target"
fi
%s`, shellQuote(device), shellQuote(mount.MountPath), ownership))
	}
	body["bootcmd"] = commands
	encoded, err := yaml.Marshal(body)
	return "#cloud-config\n" + string(encoded), err
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
