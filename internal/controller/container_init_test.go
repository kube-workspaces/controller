package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	workspacev1 "github.com/kube-workspaces/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func containerInitFixture(t *testing.T, objects ...client.Object) (*WorkspaceReconciler, *workspacev1.Workspace) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := workspacev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ws := &workspacev1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "test", UID: "workspace-uid"}, Spec: workspacev1.WorkspaceSpec{
		Type: "container", ContainerInit: &workspacev1.ContainerInitSpec{Mode: "injected", UserData: "#cloud-config\nruncmd: []\n"},
		Template: workspacev1.WorkspaceTemplateSpec{Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "main", Image: "alpine:3.22", Command: []string{"app"}, Args: []string{"a b", "", "$HOME"}, VolumeMounts: []corev1.VolumeMount{{Name: "existing", MountPath: "/data"}}}},
			Volumes:        []corev1.Volume{{Name: "existing", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			InitContainers: []corev1.Container{{Name: "existing-init", Image: "busybox"}},
		}},
	}}
	r := &WorkspaceReconciler{Scheme: scheme, Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), ContainerInitImage: "container-init:test"}
	return r, ws
}

func TestContainerInitBothWorkloadsPreserveOriginalAndSecretOwnership(t *testing.T) {
	for _, kind := range []string{"container", "scratch"} {
		t.Run(kind, func(t *testing.T) {
			r, ws := containerInitFixture(t)
			group := int64(1234)
			ws.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{FSGroup: &group}
			ws.Spec.Type = kind
			original := ws.DeepCopy()
			var first *corev1.PodTemplateSpec
			for i := 0; i < 2; i++ {
				var pod *corev1.PodTemplateSpec
				if kind == "container" {
					pod = &generateStatefulSet(ws).Spec.Template
				} else {
					pod = &generateDeployment(ws).Spec.Template
				}
				if err := r.prepareContainerInit(context.Background(), ws, pod); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(pod.Spec.SecurityContext, ws.Spec.Template.Spec.SecurityContext) {
					t.Fatal("pod security context/fsGroup changed during injection")
				}
				if first != nil && !reflect.DeepEqual(first, pod) {
					t.Fatal("reconciliation changed the generated template or accumulated injection")
				}
				first = pod
				main := pod.Spec.Containers[0]
				want := []string{"--user-data", "/run/container-init/user-data", "--termination-message", "/dev/termination-log", "--", "app", "a b", "", "$HOME"}
				if !reflect.DeepEqual(main.Args, want) || !reflect.DeepEqual(main.Command, []string{"/opt/container-init/container-init"}) {
					t.Fatalf("application argv not preserved: %#v", main)
				}
				if len(pod.Spec.InitContainers) != 2 || pod.Spec.InitContainers[0].Name != "existing-init" || len(pod.Spec.Volumes) != 3 || len(main.VolumeMounts) != 3 {
					t.Fatal("existing pod configuration was lost")
				}
				if !main.VolumeMounts[1].ReadOnly || !main.VolumeMounts[2].ReadOnly {
					t.Fatal("injected mounts must be read-only in the main container")
				}
			}
			if !reflect.DeepEqual(original, ws) {
				t.Fatal("original Workspace was mutated")
			}
			secret := &corev1.Secret{}
			if err := r.Get(context.Background(), types.NamespacedName{Namespace: ws.Namespace, Name: containerInitSecretName(ws.Name)}, secret); err != nil {
				t.Fatal(err)
			}
			if !metav1.IsControlledBy(secret, ws) || string(secret.Data[containerInitDataKey]) != ws.Spec.ContainerInit.UserData {
				t.Fatal("user-data ownership or contents incorrect")
			}
			clone := ws.DeepCopy()
			clone.Name, clone.UID = "clone", "clone-uid"
			pod := &generateDeployment(clone).Spec.Template
			if err := r.prepareContainerInit(context.Background(), clone, pod); err != nil {
				t.Fatal(err)
			}
			if pod.Spec.Volumes[1].Secret.SecretName == secret.Name {
				t.Fatal("clone reused the source's generated Secret")
			}
		})
	}
}

func TestContainerInitIdentityArguments(t *testing.T) {
	r, ws := containerInitFixture(t)
	ws.Spec.ContainerInit.DefaultUser = "developer"
	ws.Spec.ContainerInit.RunAsUser = "app"
	pod := &generateStatefulSet(ws).Spec.Template
	if err := r.prepareContainerInit(context.Background(), ws, pod); err != nil {
		t.Fatal(err)
	}
	want := []string{"--user-data", "/run/container-init/user-data", "--termination-message", "/dev/termination-log", "--default-user", "developer", "--run-as", "app", "--"}
	if len(pod.Spec.Containers[0].Args) < len(want) || !reflect.DeepEqual(pod.Spec.Containers[0].Args[:len(want)], want) {
		t.Fatalf("identity flags lost: %v", pod.Spec.Containers[0].Args)
	}
	ws.Spec.ContainerInit.RunAsUser = "root"
	if err := r.prepareContainerInit(context.Background(), ws, &generateStatefulSet(ws).Spec.Template); err == nil {
		t.Fatal("root privilege-drop request accepted")
	}
}

func TestContainerInitLatestPullPolicy(t *testing.T) {
	for _, image := range []string{"ghcr.io/example/installer:latest", "registry:5000/installer", "ghcr.io/example/installer:sha-123", "ghcr.io/example/installer@sha256:123"} {
		r, ws := containerInitFixture(t)
		r.ContainerInitImage = image
		pod := &generateStatefulSet(ws).Spec.Template
		if err := r.prepareContainerInit(context.Background(), ws, pod); err != nil {
			t.Fatal(err)
		}
		policy := pod.Spec.InitContainers[len(pod.Spec.InitContainers)-1].ImagePullPolicy
		want := corev1.PullIfNotPresent
		if strings.HasSuffix(image, ":latest") || image == "registry:5000/installer" {
			want = corev1.PullAlways
		}
		if policy != want {
			t.Fatalf("%s: got %s want %s", image, policy, want)
		}
	}
}

func TestContainerInitPreservesTerminationMessageSettings(t *testing.T) {
	const path = "/run/custom-termination"
	r, ws := containerInitFixture(t)
	ws.Spec.Template.Spec.Containers[0].TerminationMessagePath = path
	ws.Spec.Template.Spec.Containers[0].TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	pod := &generateStatefulSet(ws).Spec.Template
	if err := r.prepareContainerInit(context.Background(), ws, pod); err != nil {
		t.Fatal(err)
	}
	main := pod.Spec.Containers[0]
	if main.Args[3] != path || main.TerminationMessagePath != path || main.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Fatal("explicit termination message settings overwritten")
	}
}

func TestContainerInitFailureConditionRejectsPayloadsAndStaleFailure(t *testing.T) {
	_, ws := containerInitFixture(t)
	for _, message := range []string{`{"source":"container-init","version":1,"state":"failed","step":"runcmd[0]"}`, `{"source":"container-init","version":1,"state":"failed","step":"do-not-print-me"}`, "arbitrary tool output"} {
		pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: message}}}}}}
		condition := containerInitFailureCondition(ws, pod)
		if strings.Contains(message, "runcmd[0]") {
			if condition == nil || condition.Reason != "ContainerInitFailed" || !strings.Contains(condition.Message, "runcmd[0]") {
				t.Fatal("missing failure condition")
			}
		} else if condition != nil {
			t.Fatal("untrusted message accepted as failure phase")
		}
		pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		if containerInitFailureCondition(ws, pod) != nil {
			t.Fatal("previous attempt failure leaked into running retry")
		}
	}
}

func TestContainerInitBakedInAndDisabled(t *testing.T) {
	r, ws := containerInitFixture(t)
	r.ContainerInitImage = ""
	ws.Spec.ContainerInit.Mode = "baked-in"
	pod := &generateStatefulSet(ws).Spec.Template
	if err := r.prepareContainerInit(context.Background(), ws, pod); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pod.Spec.Containers[0].Command, ws.Spec.Template.Spec.Containers[0].Command) || len(pod.Spec.InitContainers) != 1 || len(pod.Spec.Volumes) != 2 {
		t.Fatal("baked-in mode changed application startup")
	}
	ws.Spec.ContainerInit.Mode = "disabled"
	pod = &generateStatefulSet(ws).Spec.Template
	if err := r.prepareContainerInit(context.Background(), ws, pod); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.Volumes) != 1 || len(pod.Spec.InitContainers) != 1 {
		t.Fatal("disabled mode injected pod configuration")
	}
	list := &corev1.SecretList{}
	if err := r.List(context.Background(), list); err != nil || len(list.Items) != 0 {
		t.Fatalf("owned Secret not cleaned up: %v", err)
	}
}

func TestContainerInitCollisionAndPreflightFailures(t *testing.T) {
	for _, failure := range []string{"image", "command", "volume", "mount", "init-name", "mode", "empty-data", "two-sources", "vm"} {
		t.Run(failure, func(t *testing.T) {
			r, ws := containerInitFixture(t)
			switch failure {
			case "image":
				r.ContainerInitImage = ""
			case "command":
				ws.Spec.Template.Spec.Containers[0].Command = nil
			case "volume":
				ws.Spec.Template.Spec.Volumes[0].Name = containerInitBinVolume
			case "mount":
				ws.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath = containerInitDataPath
			case "init-name":
				ws.Spec.Template.Spec.InitContainers[0].Name = containerInitInstaller
			case "mode":
				ws.Spec.ContainerInit.Mode = "unsupported"
			case "empty-data":
				ws.Spec.ContainerInit.UserData = ""
			case "two-sources":
				ws.Spec.ContainerInit.UserDataSecretRef = &corev1.LocalObjectReference{Name: "private"}
			case "vm":
				ws.Spec.Type = "vm"
			}
			pod := &generateStatefulSet(ws).Spec.Template
			if err := r.prepareContainerInit(context.Background(), ws, pod); err == nil {
				t.Fatal("invalid initialization accepted")
			}
			list := &corev1.SecretList{}
			if err := r.List(context.Background(), list); err != nil || len(list.Items) > 0 {
				t.Fatal("preflight failure created a Secret")
			}
		})
	}
}

func TestContainerInitNeverAdoptsForeignSecret(t *testing.T) {
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-container-init", Namespace: "test"}, Data: map[string][]byte{containerInitDataKey: []byte("foreign")}}
	r, ws := containerInitFixture(t, foreign)
	if err := r.prepareContainerInit(context.Background(), ws, &generateStatefulSet(ws).Spec.Template); err == nil {
		t.Fatal("foreign Secret adopted")
	}
	ws.Spec.ContainerInit = nil
	if err := r.prepareContainerInit(context.Background(), ws, &generateStatefulSet(ws).Spec.Template); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(foreign), got); err != nil || string(got.Data[containerInitDataKey]) != "foreign" || len(got.OwnerReferences) != 0 {
		t.Fatal("foreign Secret changed or deleted")
	}
}

func TestContainerInitReferencedSecretRolloutAndWatch(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "test"}, Data: map[string][]byte{containerInitDataKey: []byte("#cloud-config\nruncmd: []\n")}}
	r, ws := containerInitFixture(t, secret)
	ws.Spec.ContainerInit.UserData = ""
	ws.Spec.ContainerInit.UserDataSecretRef = &corev1.LocalObjectReference{Name: secret.Name}
	if err := r.Create(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	pod := &generateStatefulSet(ws).Spec.Template
	if err := r.prepareContainerInit(context.Background(), ws, pod); err != nil {
		t.Fatal(err)
	}
	first := pod.Annotations[containerInitHash]
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(secret), secret); err != nil {
		t.Fatal(err)
	}
	secret.Data[containerInitDataKey] = []byte("#cloud-config\nruncmd: [[true]]\n")
	if err := r.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	pod = &generateStatefulSet(ws).Spec.Template
	if err := r.prepareContainerInit(context.Background(), ws, pod); err != nil {
		t.Fatal(err)
	}
	if first == pod.Annotations[containerInitHash] || len(secret.OwnerReferences) != 0 {
		t.Fatal("referenced data change did not change rollout identity, or Secret was adopted")
	}
	requests := r.workspacesForContainerInitSecret(context.Background(), secret)
	if len(requests) != 1 || requests[0].Name != ws.Name {
		t.Fatalf("Secret watch: %#v", requests)
	}
	if name := containerInitSecretName(strings.Repeat("a", 253)); len(name) > 253 {
		t.Fatal("generated Secret name exceeds Kubernetes limit")
	}
}
