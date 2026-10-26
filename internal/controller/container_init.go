package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	containerInitDisabled   = "disabled"
	containerInitInjected   = "injected"
	containerInitBakedIn    = "baked-in"
	containerInitBinVolume  = "kw-container-init-bin"
	containerInitDataVolume = "kw-container-init-data"
	containerInitInstaller  = "container-init-install"
	containerInitBinPath    = "/opt/container-init"
	containerInitDataPath   = "/run/container-init"
	containerInitDataKey    = "user-data"
	containerInitMaxData    = 262144
	containerInitHash       = "kubeworkspaces.io/container-init-data-hash"
)

var containerInitAccountName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func validateContainerInitIdentity(cfg *workspacev1.ContainerInitSpec) error {
	if (cfg.DefaultUser != "" || cfg.RunAsUser != "") && cfg.Mode != containerInitInjected {
		return fmt.Errorf("container initialization identity options require injected mode")
	}
	for _, name := range []string{cfg.DefaultUser, cfg.RunAsUser} {
		if name != "" && !containerInitAccountName.MatchString(name) {
			return fmt.Errorf("container initialization identity must be a valid account name")
		}
	}
	if cfg.RunAsUser == "root" {
		return fmt.Errorf("container initialization runAsUser must be non-root")
	}
	return nil
}

func containerInitPullPolicy(image string) corev1.PullPolicy {
	name := image[strings.LastIndex(image, "/")+1:]
	if !strings.Contains(image, "@") && (strings.HasSuffix(name, ":latest") || !strings.Contains(name, ":")) {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

func containerInitSecretName(name string) string {
	if len(name) > 230 {
		sum := sha256.Sum256([]byte(name))
		name = strings.TrimRight(name[:210], "-.") + fmt.Sprintf("-%x", sum[:8])
	}
	return name + "-container-init"
}

// prepareContainerInit only transforms the generated workload template. The
// Workspace's original argv/template stays intact, so repeated reconciles and
// clones never accumulate wrappers. Secret bytes are never placed in status.
func (r *WorkspaceReconciler) prepareContainerInit(ctx context.Context, ws *workspacev1.Workspace, pod *corev1.PodTemplateSpec) error {
	cfg := ws.Spec.ContainerInit
	if cfg == nil || cfg.Mode == containerInitDisabled {
		return r.removeContainerInitSecret(ctx, ws)
	}
	if cfg.Mode != containerInitInjected && cfg.Mode != containerInitBakedIn {
		return fmt.Errorf("unsupported container initialization mode")
	}
	if err := validateContainerInitIdentity(cfg); err != nil {
		return err
	}
	if workspaceTypeOf(ws) == WorkspaceTypeVM {
		return fmt.Errorf("container initialization cannot be used for a VM")
	}
	if len(pod.Spec.Containers) == 0 {
		return fmt.Errorf("container initialization requires a main container")
	}
	if cfg.Mode == containerInitInjected {
		if strings.TrimSpace(r.ContainerInitImage) == "" {
			return fmt.Errorf("injected initialization requires the operator's container-init-image setting")
		}
		if len(pod.Spec.Containers[0].Command) == 0 || pod.Spec.Containers[0].Command[0] == "" {
			return fmt.Errorf("injected initialization requires an explicit application command")
		}
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == containerInitBinVolume || volume.Name == containerInitDataVolume {
			return fmt.Errorf("workspace volume conflicts with a reserved container-init volume name")
		}
	}
	for _, init := range pod.Spec.InitContainers {
		if init.Name == containerInitInstaller {
			return fmt.Errorf("workspace init container conflicts with the container-init installer name")
		}
	}
	main := &pod.Spec.Containers[0]
	for _, mount := range main.VolumeMounts {
		if mount.MountPath == containerInitBinPath || mount.MountPath == containerInitDataPath ||
			strings.HasPrefix(mount.MountPath, containerInitBinPath+"/") || strings.HasPrefix(mount.MountPath, containerInitDataPath+"/") {
			return fmt.Errorf("workspace mount conflicts with a reserved container-init path")
		}
	}
	name, data, err := r.containerInitData(ctx, ws)
	if err != nil {
		return err
	}
	mode := int32(0444)
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: containerInitDataVolume,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: name, DefaultMode: &mode,
			Items: []corev1.KeyToPath{{Key: containerInitDataKey, Path: containerInitDataKey}},
		}},
	})
	main.VolumeMounts = append(main.VolumeMounts, corev1.VolumeMount{Name: containerInitDataVolume, MountPath: containerInitDataPath, ReadOnly: true})
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[containerInitHash] = fmt.Sprintf("%x", sha256.Sum256(data))
	if cfg.Mode == containerInitBakedIn {
		return nil
	}
	argv := append(append([]string{}, main.Command...), main.Args...)
	main.Command = []string{containerInitBinPath + "/container-init"}
	if main.TerminationMessagePath == "" {
		main.TerminationMessagePath = "/dev/termination-log"
	}
	if main.TerminationMessagePolicy == "" {
		main.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	main.Args = []string{"--user-data", containerInitDataPath + "/" + containerInitDataKey, "--termination-message", main.TerminationMessagePath}
	if cfg.DefaultUser != "" {
		main.Args = append(main.Args, "--default-user", cfg.DefaultUser)
	}
	if cfg.RunAsUser != "" {
		main.Args = append(main.Args, "--run-as", cfg.RunAsUser)
	}
	main.Args = append(append(main.Args, "--"), argv...)
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: containerInitBinVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	main.VolumeMounts = append(main.VolumeMounts, corev1.VolumeMount{Name: containerInitBinVolume, MountPath: containerInitBinPath, ReadOnly: true})
	uid := int64(0)
	readOnly := true
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
		Name: containerInitInstaller, Image: r.ContainerInitImage, ImagePullPolicy: containerInitPullPolicy(r.ContainerInitImage),
		Command:         []string{"/bin/sh", "-c", "cp /usr/local/bin/container-init /install/container-init && chmod 0755 /install/container-init"},
		VolumeMounts:    []corev1.VolumeMount{{Name: containerInitBinVolume, MountPath: "/install"}},
		SecurityContext: &corev1.SecurityContext{RunAsUser: &uid, ReadOnlyRootFilesystem: &readOnly},
	})
	return nil
}

var containerInitFailureStep = regexp.MustCompile(`^(preflight|handoff|package_update|package_upgrade|packages|ssh_authorized_keys|ca_certs|timezone|(bootcmd|write_files|runcmd|groups|users|chpasswd\.users)\[[0-9]+\]|users\[[0-9]+\]\.lock_passwd)$`)

// Only recognize the runner's structured, payload-free failure contract. Never
// copy arbitrary application/tool output into an initialization condition.
func containerInitFailureCondition(ws *workspacev1.Workspace, pod *corev1.Pod) *workspacev1.WorkspaceCondition {
	if ws.Spec.ContainerInit == nil || ws.Spec.ContainerInit.Mode != containerInitInjected || len(pod.Status.ContainerStatuses) == 0 {
		return nil
	}
	s := pod.Status.ContainerStatuses[0]
	terminated := s.State.Terminated
	if s.State.Waiting != nil {
		terminated = s.LastTerminationState.Terminated
	}
	if terminated == nil || terminated.ExitCode == 0 {
		return nil
	}
	var diagnostic struct {
		Source  string `json:"source"`
		Version int    `json:"version"`
		State   string `json:"state"`
		Step    string `json:"step"`
	}
	if len(terminated.Message) > 1024 || json.Unmarshal([]byte(terminated.Message), &diagnostic) != nil || diagnostic.Source != "container-init" || diagnostic.Version != 1 || diagnostic.State != "failed" || !containerInitFailureStep.MatchString(diagnostic.Step) {
		return nil
	}
	return &workspacev1.WorkspaceCondition{Type: "ContainerInitialized", Status: "False", Reason: "ContainerInitFailed", Message: "Container initialization failed during " + diagnostic.Step + "; inspect the main container logs.", LastTransitionTime: terminated.FinishedAt}
}

func (r *WorkspaceReconciler) containerInitData(ctx context.Context, ws *workspacev1.Workspace) (string, []byte, error) {
	cfg := ws.Spec.ContainerInit
	if cfg.UserDataSecretRef != nil {
		if cfg.UserData != "" || cfg.UserDataSecretRef.Name == "" || cfg.UserDataSecretRef.Name == containerInitSecretName(ws.Name) {
			return "", nil, fmt.Errorf("choose inline userData or an independently managed named userDataSecretRef")
		}
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: cfg.UserDataSecretRef.Name}, secret); err != nil {
			return "", nil, fmt.Errorf("cannot read container initialization Secret: %w", err)
		}
		data := secret.Data[containerInitDataKey]
		if len(data) == 0 || len(data) > containerInitMaxData {
			return "", nil, fmt.Errorf("container initialization Secret needs a nonempty user-data key of at most 256 KiB")
		}
		if err := r.removeContainerInitSecret(ctx, ws); err != nil {
			return "", nil, err
		}
		return secret.Name, data, nil
	}
	data := []byte(cfg.UserData)
	if len(data) == 0 || len(data) > containerInitMaxData {
		return "", nil, fmt.Errorf("container initialization userData must contain 1 to 262144 bytes")
	}
	name := containerInitSecretName(ws.Name)
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: name}, secret)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", nil, err
	}
	if apierrors.IsNotFound(err) {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ws.Namespace}, Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{containerInitDataKey: data}}
		if err := ctrl.SetControllerReference(ws, secret, r.Scheme); err != nil {
			return "", nil, err
		}
		if err := r.Create(ctx, secret); err != nil {
			return "", nil, err
		}
	} else {
		if !metav1.IsControlledBy(secret, ws) {
			return "", nil, fmt.Errorf("container initialization Secret name is already in use by another owner")
		}
		wanted := map[string][]byte{containerInitDataKey: data}
		if !reflect.DeepEqual(secret.Data, wanted) {
			secret.Data = wanted
			if err := r.Update(ctx, secret); err != nil {
				return "", nil, err
			}
		}
	}
	return name, data, nil
}

func (r *WorkspaceReconciler) removeContainerInitSecret(ctx context.Context, ws *workspacev1.Workspace) error {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: containerInitSecretName(ws.Name)}, secret)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(secret, ws) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, secret))
}

func (r *WorkspaceReconciler) workspacesForContainerInitSecret(ctx context.Context, obj client.Object) []ctrl.Request {
	list := &workspacev1.WorkspaceList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var requests []ctrl.Request
	for _, ws := range list.Items {
		cfg := ws.Spec.ContainerInit
		if cfg != nil && cfg.Mode != containerInitDisabled && cfg.UserDataSecretRef != nil && cfg.UserDataSecretRef.Name == obj.GetName() {
			requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Name: ws.Name, Namespace: ws.Namespace}})
		}
	}
	return requests
}
