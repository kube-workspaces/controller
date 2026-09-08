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
	"fmt"
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

const (
	// DefaultContainerPort is the default port for workspace containers
	DefaultContainerPort = 8080
	// DefaultServingPort is the port exposed by the Service
	DefaultServingPort = 80
	// AnnotationStopped is the annotation that indicates a workspace is stopped
	AnnotationStopped = "kubeworkspaces.io/stopped"
	// AnnotationReset is the annotation that requests a reset (re-provisioning
	// the workspace from its image, giving it a fresh root volume). The API
	// stamps it with a timestamp so every reset is unique.
	AnnotationReset = "kubeworkspaces.io/reset"
	// LabelWorkspaceName is the label applied to pods to identify the workspace
	LabelWorkspaceName = "workspace-name"
	// LabelKubevirtVM is set by KubeVirt on virt-launcher pods, naming the VMI
	LabelKubevirtVM = "vm.kubevirt.io/name"
)

// Workspace workload types (spec.type).
const (
	WorkspaceTypeContainer = "container"
	WorkspaceTypeVM        = "vm"
	WorkspaceTypeScratch   = "scratch"
)

// kubeVirtVirtualMachineGVK identifies KubeVirt VirtualMachine objects without
// importing the KubeVirt API module (the controller manages them unstructured).
var kubeVirtVirtualMachineGVK = schema.GroupVersionKind{
	Group:   "kubevirt.io",
	Version: "v1",
	Kind:    "VirtualMachine",
}

// kubeVirtVirtualMachineInstanceGVK identifies KubeVirt VirtualMachineInstance
// objects without importing the KubeVirt API module.
var kubeVirtVirtualMachineInstanceGVK = schema.GroupVersionKind{
	Group:   "kubevirt.io",
	Version: "v1",
	Kind:    "VirtualMachineInstance",
}

// WorkspaceReconciler reconciles a Workspace object
type WorkspaceReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	EventRecorder record.EventRecorder
	// APIReader bypasses the informer cache for resources the controller does
	// not watch (e.g. Image CRs read for cloud-init seeding).
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=images,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=workspaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=workspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachineinstances/console,verbs=get
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachineinstances/vnc,verbs=get

// Reconcile is the main reconciliation loop for Workspace resources.
// It ensures the workload matching the workspace type (StatefulSet, Deployment
// or KubeVirt VirtualMachine) plus a Service exist for each Workspace CR and
// keeps the status up to date.
//
//nolint:gocyclo
func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reconcileStart := time.Now()
	log := logf.FromContext(ctx)
	log.Info("Reconciliation loop started")

	// Fetch the Workspace instance
	instance := &kubeworkspacesiov1alpha1.Workspace{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		if apierrs.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch Workspace")
		ReconcileTotal.WithLabelValues("error").Inc()
		ReconcileDuration.Observe(time.Since(reconcileStart).Seconds())
		return ctrl.Result{}, err
	}

	// If workspace is being deleted, do nothing (let garbage collection handle owned resources)
	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	wsType := workspaceTypeOf(instance)

	// VM workspaces need the KubeVirt CRDs installed; degrade gracefully if absent.
	if wsType == WorkspaceTypeVM {
		installed, err := r.kubeVirtInstalled(ctx)
		if err != nil {
			log.Error(err, "unable to check KubeVirt availability")
			return ctrl.Result{}, err
		}
		if !installed {
			log.Info("KubeVirt CRDs not installed; cannot run vm workspace", "name", instance.Name)
			r.setCondition(ctx, instance, "Ready", "False", "KubeVirtNotInstalled",
				"KubeVirt is not installed in this cluster")
			return ctrl.Result{}, nil
		}

		// A reset request re-provisions the VM back to pristine: delete the
		// VirtualMachine so the owned root DataVolume/PVC is garbage collected,
		// then recreate it fresh from the image on a later reconcile. Handled
		// here (before the workload switch) so the reset path can requeue.
		res, resetHandled, err := r.handleReset(ctx, instance)
		if err != nil {
			ReconcileTotal.WithLabelValues("error").Inc()
			ReconcileDuration.Observe(time.Since(reconcileStart).Seconds())
			return ctrl.Result{}, err
		}
		if resetHandled {
			return res, nil
		}
	}

	// Reconcile the workload
	var readyReplicas int32
	var pod *corev1.Pod

	switch wsType {
	case WorkspaceTypeVM:
		rr, p, err := r.reconcileVirtualMachine(ctx, instance)
		if err != nil {
			ReconcileTotal.WithLabelValues("error").Inc()
			ReconcileDuration.Observe(time.Since(reconcileStart).Seconds())
			return ctrl.Result{}, err
		}
		readyReplicas, pod = rr, p
	case WorkspaceTypeScratch:
		rr, p, err := r.reconcileDeployment(ctx, instance)
		if err != nil {
			ReconcileTotal.WithLabelValues("error").Inc()
			ReconcileDuration.Observe(time.Since(reconcileStart).Seconds())
			return ctrl.Result{}, err
		}
		readyReplicas, pod = rr, p
	default:
		rr, p, err := r.reconcileStatefulSet(ctx, instance)
		if err != nil {
			ReconcileTotal.WithLabelValues("error").Inc()
			ReconcileDuration.Observe(time.Since(reconcileStart).Seconds())
			return ctrl.Result{}, err
		}
		readyReplicas, pod = rr, p
	}

	// Reconcile Service
	if err := r.reconcileService(ctx, instance, wsType); err != nil {
		ReconcileTotal.WithLabelValues("error").Inc()
		ReconcileDuration.Observe(time.Since(reconcileStart).Seconds())
		return ctrl.Result{}, err
	}

	// Update Workspace CR status
	if err := r.updateWorkspaceStatus(ctx, instance, readyReplicas, pod); err != nil {
		ReconcileTotal.WithLabelValues("error").Inc()
		ReconcileDuration.Observe(time.Since(reconcileStart).Seconds())
		return ctrl.Result{}, err
	}

	// Record metrics
	ReconcileTotal.WithLabelValues("success").Inc()
	ReconcileDuration.Observe(time.Since(reconcileStart).Seconds())

	// Track workspace status for gauge
	_, stopped := instance.Annotations[AnnotationStopped]
	if stopped {
		WorkspacesTotal.WithLabelValues(instance.Namespace, "stopped").Set(1)
	} else if readyReplicas > 0 {
		WorkspacesTotal.WithLabelValues(instance.Namespace, "running").Set(1)
	} else {
		WorkspacesTotal.WithLabelValues(instance.Namespace, "starting").Set(1)
	}

	// Record ready time (time from creation to first ready state)
	if readyReplicas > 0 && !instance.CreationTimestamp.IsZero() {
		readyTime := time.Since(instance.CreationTimestamp.Time).Seconds()
		// Only record if this is a reasonable startup time (< 30 min, avoids re-recording on every reconcile)
		if readyTime < 1800 && readyTime > 0 {
			// Check if we haven't already recorded this (use annotation as marker)
			if _, recorded := instance.Annotations["kubeworkspaces.io/ready-time-recorded"]; !recorded {
				WorkspaceReadyTime.Observe(readyTime)
				// Mark as recorded to avoid duplicate observations
				annotations := instance.Annotations
				if annotations == nil {
					annotations = make(map[string]string)
				}
				annotations["kubeworkspaces.io/ready-time-recorded"] = "true"
				instance.Annotations = annotations
				if err := r.Update(ctx, instance); err != nil {
					log.Error(err, "unable to set ready-time-recorded annotation")
				}
			}
		}
	}

	return ctrl.Result{}, nil
}

// workspaceTypeOf returns the workspace's spec.type, defaulting to container.
func workspaceTypeOf(ws *kubeworkspacesiov1alpha1.Workspace) string {
	if ws.Spec.Type == "" {
		return WorkspaceTypeContainer
	}
	return ws.Spec.Type
}

// kubeVirtInstalled reports whether the KubeVirt VirtualMachine CRD exists.
func (r *WorkspaceReconciler) kubeVirtInstalled(ctx context.Context) (bool, error) {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(kubeVirtVirtualMachineGVK)
	err := r.Get(ctx, types.NamespacedName{Namespace: metav1.NamespaceSystem, Name: "kubevirt-probe"}, vm)
	if err == nil {
		return true, nil
	}
	if apierrs.IsNotFound(err) {
		// Object not found means the CRD (and API) exists — the probe object never does.
		return true, nil
	}
	if isNoMatchError(err) {
		return false, nil
	}
	return false, err
}

// isNoMatchError detects "no kind is registered for the type" style errors
// returned when a CRD's API group is not served by the cluster.
func isNoMatchError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no matches for kind") ||
		strings.Contains(msg, "the server could not find the requested resource") ||
		strings.Contains(msg, "no kind is registered")
}

// setCondition writes a single status condition, replacing any of the same type.
func (r *WorkspaceReconciler) setCondition(ctx context.Context, ws *kubeworkspacesiov1alpha1.Workspace,
	condType, status, reason, message string) {

	var conditions []kubeworkspacesiov1alpha1.WorkspaceCondition
	for _, c := range ws.Status.Conditions {
		if c.Type != condType {
			conditions = append(conditions, c)
		}
	}
	conditions = append(conditions, kubeworkspacesiov1alpha1.WorkspaceCondition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
	if reflect.DeepEqual(ws.Status.Conditions, conditions) {
		return
	}
	ws.Status.Conditions = conditions
	if err := r.Status().Update(ctx, ws); err != nil {
		logf.FromContext(ctx).Error(err, "unable to set workspace condition")
	}
}

// reconcileStatefulSet handles container-type workspaces (today's behaviour).
// Returns ready replicas and the workspace pod ({name}-0).
func (r *WorkspaceReconciler) reconcileStatefulSet(ctx context.Context, instance *kubeworkspacesiov1alpha1.Workspace) (int32, *corev1.Pod, error) {
	log := logf.FromContext(ctx)

	ss := generateStatefulSet(instance)
	if err := ctrl.SetControllerReference(instance, ss, r.Scheme); err != nil {
		return 0, nil, err
	}

	foundStateful := &appsv1.StatefulSet{}
	justCreated := false
	err := r.Get(ctx, types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}, foundStateful)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating StatefulSet", "namespace", ss.Namespace, "name", ss.Name)
		err = r.Create(ctx, ss)
		justCreated = true
		if err != nil {
			log.Error(err, "unable to create StatefulSet")
			return 0, nil, err
		}
	} else if err != nil {
		log.Error(err, "error getting StatefulSet")
		return 0, nil, err
	}

	// Update the StatefulSet if needed
	if !justCreated && statefulSetNeedsUpdate(ss, foundStateful) {
		log.Info("Updating StatefulSet", "namespace", ss.Namespace, "name", ss.Name)
		foundStateful.Spec.Replicas = ss.Spec.Replicas
		// Assign the deep-copied PodSpec wholesale. A shallow copy of only the
		// containers slice drops volumes/initContainers added by the API (e.g.
		// the dshm shared-memory volume), leaving volumeMounts pointing at
		// volumes the StatefulSet no longer has.
		foundStateful.Spec.Template.Spec = ss.Spec.Template.Spec
		if err := r.Update(ctx, foundStateful); err != nil {
			log.Error(err, "unable to update StatefulSet")
			return 0, nil, err
		}
	}

	pod, err := r.getPod(ctx, instance.Namespace, ss.Name+"-0")
	if err != nil {
		return foundStateful.Status.ReadyReplicas, nil, err
	}
	return foundStateful.Status.ReadyReplicas, pod, nil
}

// reconcileDeployment handles scratch-type workspaces: a plain Deployment with
// replicas 0/1 driven by the stopped annotation. Pod names are generated, so
// the pod is resolved via the workspace-name label.
func (r *WorkspaceReconciler) reconcileDeployment(ctx context.Context, instance *kubeworkspacesiov1alpha1.Workspace) (int32, *corev1.Pod, error) {
	log := logf.FromContext(ctx)

	dep := generateDeployment(instance)
	if err := ctrl.SetControllerReference(instance, dep, r.Scheme); err != nil {
		return 0, nil, err
	}

	found := &appsv1.Deployment{}
	justCreated := false
	err := r.Get(ctx, types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, found)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating Deployment", "namespace", dep.Namespace, "name", dep.Name)
		err = r.Create(ctx, dep)
		justCreated = true
		if err != nil {
			log.Error(err, "unable to create Deployment")
			return 0, nil, err
		}
	} else if err != nil {
		log.Error(err, "error getting Deployment")
		return 0, nil, err
	}

	if !justCreated && deploymentNeedsUpdate(dep, found) {
		log.Info("Updating Deployment", "namespace", dep.Namespace, "name", dep.Name)
		found.Spec.Replicas = dep.Spec.Replicas
		found.Spec.Template.Spec.Containers = dep.Spec.Template.Spec.Containers
		found.Spec.Template.Spec.InitContainers = dep.Spec.Template.Spec.InitContainers
		found.Spec.Template.Spec.Volumes = dep.Spec.Template.Spec.Volumes
		if err := r.Update(ctx, found); err != nil {
			log.Error(err, "unable to update Deployment")
			return 0, nil, err
		}
	}

	pod, err := r.podByLabel(ctx, instance.Namespace, LabelWorkspaceName+"="+instance.Name)
	if err != nil {
		return found.Status.ReadyReplicas, nil, err
	}
	return found.Status.ReadyReplicas, pod, nil
}

// reconcileVirtualMachine handles vm-type workspaces: a KubeVirt VirtualMachine
// whose root disk is a containerDisk built from the workspace's main container
// image. Status is derived from the virt-launcher pod, which KubeVirt labels
// with vm.kubevirt.io/name=<vm name>; we also inject the workspace-name label
// into the VMI template so the existing pod watch maps it back to the Workspace.
func (r *WorkspaceReconciler) reconcileVirtualMachine(ctx context.Context, instance *kubeworkspacesiov1alpha1.Workspace) (int32, *corev1.Pod, error) {
	log := logf.FromContext(ctx)

	// Seed cloud-init user-data from the Image CR's declared defaults (when the
	// image opts in). The user-data is inlined in the VM's NoCloud datasource.
	// The Image CR also drives the persistent root-disk (DataVolume) and memory
	// overrides for desktop images.
	imageRef := ""
	if len(instance.Spec.Template.Spec.Containers) > 0 {
		imageRef = instance.Spec.Template.Spec.Containers[0].Image
	}
	img, err := imageByRef(ctx, r.APIReader, instance.Namespace, imageRef)
	if err != nil {
		log.Error(err, "unable to look up Image CR for cloud-init seeding")
		return 0, nil, err
	}

	desired := generateVirtualMachine(instance, img)
	if err := ctrl.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return 0, nil, err
	}

	found := &unstructured.Unstructured{}
	found.SetGroupVersionKind(kubeVirtVirtualMachineGVK)
	justCreated := false
	err = r.Get(ctx, types.NamespacedName{Name: desired.GetName(), Namespace: desired.GetNamespace()}, found)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating VirtualMachine", "namespace", desired.GetNamespace(), "name", desired.GetName())
		err = r.Create(ctx, desired)
		justCreated = true
		if err != nil {
			log.Error(err, "unable to create VirtualMachine")
			return 0, nil, err
		}
	} else if err != nil {
		log.Error(err, "error getting VirtualMachine")
		return 0, nil, err
	}

	if !justCreated && virtualMachineNeedsUpdate(desired, found) {
		log.Info("Updating VirtualMachine", "namespace", desired.GetNamespace(), "name", desired.GetName())
		found.Object["spec"] = desired.Object["spec"]
		if err := r.Update(ctx, found); err != nil {
			log.Error(err, "unable to update VirtualMachine")
			return 0, nil, err
		}
	}

	// The launcher pod only exists while the VM is running.
	pod, err := r.podByLabel(ctx, instance.Namespace, LabelKubevirtVM+"="+instance.Name)
	if err != nil {
		return 0, nil, err
	}

	// The VMI must exist and be in Running phase for the workspace to be
	// considered ready. Without this, a crashed/restarting VMI still reports
	// readyReplicas=1 because the virt-launcher pod stays in Ready state while
	// the guest is down.
	vmiReady := vmiIsRunning(ctx, r.Client, instance)

	var readyReplicas int32
	if pod != nil && podReady(pod) && vmiReady {
		readyReplicas = 1
	}
	return readyReplicas, pod, nil
}

// handleReset re-provisions a vm workspace when a reset has been requested.
// The API stamps the kubeworkspaces.io/reset annotation; handling it here
// deletes the VirtualMachine, whose owned DataVolume/PVC (the persistent root
// volume) is garbage collected along with it. The annotation is only cleared
// once the VirtualMachine is fully gone so the next reconcile recreates the
// workspace from its image with a fresh root disk. It returns (Result, true)
// for every reset path so the caller skips the normal workload reconcile.
func (r *WorkspaceReconciler) handleReset(ctx context.Context, instance *kubeworkspacesiov1alpha1.Workspace) (ctrl.Result, bool, error) {
	if _, ok := instance.Annotations[AnnotationReset]; !ok {
		return ctrl.Result{}, false, nil
	}
	log := logf.FromContext(ctx)
	log.Info("Reset requested; re-provisioning VirtualMachine from image", "namespace", instance.Namespace, "name", instance.Name)

	found := &unstructured.Unstructured{}
	found.SetGroupVersionKind(kubeVirtVirtualMachineGVK)
	err := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, found)
	if err == nil {
		if found.GetDeletionTimestamp().IsZero() {
			log.Info("Deleting VirtualMachine", "namespace", found.GetNamespace(), "name", found.GetName())
			// Foreground propagation so the owned root DataVolume/PVC is
			// garbage collected before the VM disappears, guaranteeing a clean
			// re-import of a fresh root volume.
			if err := r.Delete(ctx, found, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrs.IsNotFound(err) {
				log.Error(err, "unable to delete VirtualMachine for reset")
				return ctrl.Result{}, true, err
			}
		}
		// The VM still exists (or is mid-deletion): keep the reset annotation
		// and requeue until it and its root volume are gone.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, true, nil
	}
	if !apierrs.IsNotFound(err) {
		log.Error(err, "unable to get VirtualMachine for reset")
		return ctrl.Result{}, true, err
	}

	// The VirtualMachine (and its root volume) are gone; consume the reset
	// annotation so the next reconcile recreates the workspace fresh.
	log.Info("VirtualMachine deleted; clearing reset annotation", "namespace", instance.Namespace, "name", instance.Name)
	annotations := instance.GetAnnotations()
	delete(annotations, AnnotationReset)
	// A fresh re-provision is a fresh workspace: restart the ready-time clock.
	delete(annotations, "kubeworkspaces.io/ready-time-recorded")
	instance.SetAnnotations(annotations)
	if err := r.Update(ctx, instance); err != nil {
		log.Error(err, "unable to clear reset annotation")
		return ctrl.Result{}, true, err
	}
	// Give the old root volume's garbage collection a beat before the next
	// reconcile imports a new DataVolume under the same name.
	return ctrl.Result{RequeueAfter: 3 * time.Second}, true, nil
}

// vmiIsRunning reports whether the VirtualMachineInstance backing this
// workspace exists and is in the Running phase. A missing or non-Running VMI
// means the VM is stopped, crashing, or restarting.
func vmiIsRunning(ctx context.Context, reader client.Reader, ws *kubeworkspacesiov1alpha1.Workspace) bool {
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(kubeVirtVirtualMachineInstanceGVK)
	if err := reader.Get(ctx, types.NamespacedName{Name: ws.Name, Namespace: ws.Namespace}, vmi); err != nil {
		return false
	}
	phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
	return phase == "Running"
}

// podReady reports whether a pod's Ready condition is True.
func podReady(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// getPod fetches a pod by name, returning nil (not error) when it doesn't exist.
func (r *WorkspaceReconciler) getPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, pod)
	if err != nil {
		if apierrs.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return pod, nil
}

// podByLabel returns the first pod matching the selector, nil if none exist.
func (r *WorkspaceReconciler) podByLabel(ctx context.Context, namespace, selector string) (*corev1.Pod, error) {
	sel, err := labels.Parse(selector)
	if err != nil {
		return nil, err
	}
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabelsSelector{
		Selector: sel,
	}); err != nil {
		return nil, err
	}
	if len(podList.Items) == 0 {
		return nil, nil
	}
	return &podList.Items[0], nil
}

// reconcileService ensures the Service exists with the selector matching the
// workload type. VM and scratch workloads both expose pods labelled
// workspace-name; container workloads use the StatefulSet selector.
func (r *WorkspaceReconciler) reconcileService(ctx context.Context, instance *kubeworkspacesiov1alpha1.Workspace, wsType string) error {
	log := logf.FromContext(ctx)

	service := generateService(instance, wsType)
	if err := ctrl.SetControllerReference(instance, service, r.Scheme); err != nil {
		return err
	}

	foundService := &corev1.Service{}
	justCreated := false
	err := r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, foundService)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating Service", "namespace", service.Namespace, "name", service.Name)
		err = r.Create(ctx, service)
		justCreated = true
		if err != nil {
			log.Error(err, "unable to create Service")
			return err
		}
	} else if err != nil {
		log.Error(err, "error getting Service")
		return err
	}

	// Update the Service if needed
	if !justCreated && serviceNeedsUpdate(service, foundService) {
		log.Info("Updating Service", "namespace", service.Namespace, "name", service.Name)
		foundService.Spec.Ports = service.Spec.Ports
		foundService.Spec.Selector = service.Spec.Selector
		if err := r.Update(ctx, foundService); err != nil {
			log.Error(err, "unable to update Service")
			return err
		}
	}
	return nil
}

// updateWorkspaceStatus updates the Workspace CR status from the workload's
// ready-replica count and the serving pod's state.
func (r *WorkspaceReconciler) updateWorkspaceStatus(ctx context.Context,
	ws *kubeworkspacesiov1alpha1.Workspace, readyReplicas int32, pod *corev1.Pod) error {

	log := logf.FromContext(ctx)

	status := kubeworkspacesiov1alpha1.WorkspaceStatus{
		Conditions:     make([]kubeworkspacesiov1alpha1.WorkspaceCondition, 0),
		ReadyReplicas:  readyReplicas,
		ContainerState: corev1.ContainerState{},
	}

	// Update the status based on the Pod's status
	if pod != nil && !reflect.DeepEqual(pod.Status, corev1.PodStatus{}) {
		// Use the first container status (single-container workspaces)
		if len(pod.Status.ContainerStatuses) > 0 {
			status.ContainerState = pod.Status.ContainerStatuses[0].State
		}

		// Mirror pod conditions to workspace conditions
		for i := range pod.Status.Conditions {
			condition := podCondToWorkspaceCondition(pod.Status.Conditions[i])
			status.Conditions = append(status.Conditions, condition)
		}
	}

	// Only update if status has changed
	if !reflect.DeepEqual(ws.Status, status) {
		log.Info("Updating Workspace CR Status")
		ws.Status = status
		return r.Status().Update(ctx, ws)
	}

	return nil
}

// podCondToWorkspaceCondition converts a PodCondition to a WorkspaceCondition.
func podCondToWorkspaceCondition(podc corev1.PodCondition) kubeworkspacesiov1alpha1.WorkspaceCondition {
	condition := kubeworkspacesiov1alpha1.WorkspaceCondition{
		Type:   string(podc.Type),
		Status: string(podc.Status),
	}

	if len(podc.Message) > 0 {
		condition.Message = podc.Message
	}

	if len(podc.Reason) > 0 {
		condition.Reason = podc.Reason
	}

	if !podc.LastProbeTime.Time.Equal(time.Time{}) {
		condition.LastProbeTime = podc.LastProbeTime
	} else {
		condition.LastProbeTime = metav1.Now()
	}

	if !podc.LastTransitionTime.Time.Equal(time.Time{}) {
		condition.LastTransitionTime = podc.LastTransitionTime
	} else {
		condition.LastTransitionTime = metav1.Now()
	}

	return condition
}

// generateStatefulSet creates the desired StatefulSet for a Workspace.
func generateStatefulSet(instance *kubeworkspacesiov1alpha1.Workspace) *appsv1.StatefulSet {
	replicas := int32(1)
	if _, stopped := instance.Annotations[AnnotationStopped]; stopped {
		replicas = 0
	}

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"statefulset": instance.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"statefulset":      instance.Name,
						LabelWorkspaceName: instance.Name,
					},
					Annotations: map[string]string{},
				},
				Spec: *instance.Spec.Template.Spec.DeepCopy(),
			},
		},
	}

	// Copy workspace labels to the pod template
	for k, v := range instance.Labels {
		ss.Spec.Template.Labels[k] = v
	}

	// Copy relevant workspace annotations to the pod template
	for k, v := range instance.Annotations {
		if k != AnnotationStopped {
			ss.Spec.Template.Annotations[k] = v
		}
	}

	// Ensure the first container has a valid port defined
	podSpec := &ss.Spec.Template.Spec
	if len(podSpec.Containers) > 0 {
		container := &podSpec.Containers[0]
		if len(container.Ports) == 0 || container.Ports[0].ContainerPort == 0 {
			container.Ports = []corev1.ContainerPort{
				{
					ContainerPort: DefaultContainerPort,
					Name:          "workspace-port",
					Protocol:      "TCP",
				},
			}
		}
	}

	return ss
}

// generateDeployment creates the desired Deployment for a scratch workspace.
// Identical pod template handling to generateStatefulSet, but as a Deployment
// with generated pod names (no persistent identity).
func generateDeployment(instance *kubeworkspacesiov1alpha1.Workspace) *appsv1.Deployment {
	replicas := int32(1)
	if _, stopped := instance.Annotations[AnnotationStopped]; stopped {
		replicas = 0
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"deployment": instance.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"deployment":       instance.Name,
						LabelWorkspaceName: instance.Name,
					},
					Annotations: map[string]string{},
				},
				Spec: *instance.Spec.Template.Spec.DeepCopy(),
			},
		},
	}

	for k, v := range instance.Labels {
		dep.Spec.Template.Labels[k] = v
	}
	for k, v := range instance.Annotations {
		if k != AnnotationStopped {
			dep.Spec.Template.Annotations[k] = v
		}
	}

	podSpec := &dep.Spec.Template.Spec
	if len(podSpec.Containers) > 0 {
		container := &podSpec.Containers[0]
		if len(container.Ports) == 0 || container.Ports[0].ContainerPort == 0 {
			container.Ports = []corev1.ContainerPort{
				{
					ContainerPort: DefaultContainerPort,
					Name:          "workspace-port",
					Protocol:      "TCP",
				},
			}
		}
	}

	return dep
}

// generateVirtualMachine creates the desired KubeVirt VirtualMachine for a vm
// workspace. The main container image becomes a containerDisk root volume —
// containerDisk images are OCI images, so the same image reference works.
// Declared container ports are forwarded via masquerade interfaces. The VMI
// template carries the workspace-name label so the existing pod watch maps the
// virt-launcher pod back to this Workspace.
//
// When cloudUserData is non-empty, a NoCloud datasource with the user-data
// inlined is attached so cloud-init configures the guest at (re)boot.
// containerDisk roots are ephemeral, so cloud-init runs on every start.
func generateVirtualMachine(instance *kubeworkspacesiov1alpha1.Workspace, img *kubeworkspacesiov1alpha1.Image) *unstructured.Unstructured {
	stopped := false
	if _, ok := instance.Annotations[AnnotationStopped]; ok {
		stopped = true
	}

	podSpec := instance.Spec.Template.Spec

	// Domain resources from the main container's requests/limits.
	resources := map[string]interface{}{}
	if len(podSpec.Containers) > 0 {
		if req := podSpec.Containers[0].Resources.Requests; len(req) > 0 {
			requests := map[string]interface{}{}
			for k, v := range req {
				requests[k.String()] = v.String()
			}
			resources["requests"] = requests
		}
		if lim := podSpec.Containers[0].Resources.Limits; len(lim) > 0 {
			limits := map[string]interface{}{}
			for k, v := range lim {
				limits[k.String()] = v.String()
			}
			resources["limits"] = limits
		}
	}
	// Desktop images may need more memory than the container defaults. The Image
	// CR can raise the guest RAM via MemoryLimit (kept equal to the pod request
	// so QoS keeps the workload Guaranteed) and optionally decouple the pod
	// allocation with MemoryRequest so the virt-launcher pod is granted headroom
	// above the guest RAM for qemu overhead, without inflating guest memory.
	if img != nil && img.Spec.MemoryLimit != "" {
		limits, _, _ := unstructured.NestedMap(resources, "limits")
		if limits == nil {
			limits = map[string]interface{}{}
		}
		reqMap, _, _ := unstructured.NestedMap(resources, "requests")
		if reqMap == nil {
			reqMap = map[string]interface{}{}
		}
		podMem := img.Spec.MemoryLimit
		if img.Spec.MemoryRequest != "" {
			podMem = img.Spec.MemoryRequest
		}
		limits["memory"] = podMem
		resources["limits"] = limits
		reqMap["memory"] = podMem
		resources["requests"] = reqMap
	}

	// A single masquerade interface on the pod network, forwarding every
	// declared container port. KubeVirt rejects multiple interfaces bound to
	// the same pod network.
	interfaces := []interface{}{}
	networks := []interface{}{
		map[string]interface{}{"name": "default", "pod": map[string]interface{}{}},
	}
	if len(podSpec.Containers) > 0 {
		ports := podSpec.Containers[0].Ports
		if len(ports) == 0 {
			ports = []corev1.ContainerPort{{ContainerPort: DefaultContainerPort}}
		}
		fwd := make([]interface{}, 0, len(ports))
		for _, p := range ports {
			fwd = append(fwd, map[string]interface{}{"port": int64(p.ContainerPort), "protocol": "TCP"})
		}
		interfaces = append(interfaces, map[string]interface{}{
			"name":       "default",
			"masquerade": map[string]interface{}{},
			"ports":      fwd,
		})
	}

	// Root disk: an ephemeral containerDisk by default; a registry-imported
	// DataVolume (persistent PVC) when the image opts in via PersistentRootDisk.
	image := ""
	if len(podSpec.Containers) > 0 {
		image = podSpec.Containers[0].Image
	}
	persistentRoot := img != nil && img.Spec.PersistentRootDisk && image != ""
	var dataVolumeTemplates []interface{}
	disks := []interface{}{
		map[string]interface{}{
			"name": "rootdisk",
			"disk": map[string]interface{}{"bus": "virtio"},
		},
	}
	volumes := []interface{}{}
	if persistentRoot {
		size := img.Spec.PersistentRootDiskSize
		if size == "" {
			size = "10Gi"
		}
		dataVolumeTemplates = append(dataVolumeTemplates, map[string]interface{}{
			"metadata": map[string]interface{}{"name": "rootdisk"},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"registry": map[string]interface{}{"url": "docker://" + image},
				},
				"pvc": map[string]interface{}{
					"accessModes": []interface{}{"ReadWriteOnce"},
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{"storage": size},
					},
				},
			},
		})
		volumes = append(volumes, map[string]interface{}{
			"name":       "rootdisk",
			"dataVolume": map[string]interface{}{"name": "rootdisk"},
		})
	} else {
		volumes = append(volumes, map[string]interface{}{
			"name":          "rootdisk",
			"containerDisk": map[string]interface{}{"image": image},
		})
	}
	cloudUserData := cloudInitUserData(img)
	if cloudUserData != "" {
		disks = append(disks, map[string]interface{}{
			"name": "cloudinitdisk",
			"disk": map[string]interface{}{"bus": "virtio"},
		})
		volumes = append(volumes, map[string]interface{}{
			"name": "cloudinitdisk",
			"cloudInitNoCloud": map[string]interface{}{
				"userData": cloudUserData,
			},
		})
	}

	// VMI template labels: workspace-name drives the controller's pod watch;
	// KubeVirt also adds vm.kubevirt.io/name itself.
	vmLabels := map[string]interface{}{
		LabelWorkspaceName: instance.Name,
	}
	for k, v := range instance.Labels {
		vmLabels[k] = v
	}

	vmSpec := map[string]interface{}{
		"running": !stopped,
		"template": map[string]interface{}{
			"metadata": map[string]interface{}{"labels": vmLabels},
			"spec": map[string]interface{}{
				"domain": map[string]interface{}{
					"resources": resources,
					"devices": map[string]interface{}{
						"disks":      disks,
						"interfaces": interfaces,
					},
				},
				"networks": networks,
				"volumes":  volumes,
			},
		},
	}
	// When MemoryRequest decouples the pod allocation from the guest RAM, pin
	// the domain memory explicitly so KubeVirt does not re-derive the guest from
	// the inflated pod request. Also pin the domain CPU cores from the container
	// resource limits — without domain.cpu.cores KubeVirt ignores the CPU limit
	// and defaults to 1 vCPU.
	domain, _, _ := unstructured.NestedMap(vmSpec, "template", "spec", "domain")
	if domain != nil {
		if img != nil && img.Spec.MemoryLimit != "" && img.Spec.MemoryRequest != "" {
			domain["memory"] = map[string]interface{}{"guest": img.Spec.MemoryLimit}
		}
		if limits, ok, _ := unstructured.NestedMap(resources, "limits"); ok {
			if cpuStr, hasCPU := limits["cpu"].(string); hasCPU && cpuStr != "" {
				q, err := resource.ParseQuantity(cpuStr)
				if err == nil {
					cores := q.Value()
					if cores < 1 {
						cores = 1
					}
					domain["cpu"] = map[string]interface{}{"cores": cores}
				}
			}
		}
		_ = unstructured.SetNestedField(vmSpec["template"].(map[string]interface{})["spec"].(map[string]interface{}), domain, "domain")
	}
	if len(dataVolumeTemplates) > 0 {
		vmSpec["dataVolumeTemplates"] = dataVolumeTemplates
	}

	vm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": kubeVirtVirtualMachineGVK.GroupVersion().String(),
		"kind":       kubeVirtVirtualMachineGVK.Kind,
		"metadata": map[string]interface{}{
			"name":      instance.Name,
			"namespace": instance.Namespace,
		},
		"spec": vmSpec,
	}}
	vm.SetGroupVersionKind(kubeVirtVirtualMachineGVK)
	return vm
}

// imageByRef finds the Image CR whose spec.image matches the given reference,
// or nil when no image matches (in which case cloud-init seeding is skipped).
func imageByRef(ctx context.Context, reader client.Reader, namespace, imageRef string) (*kubeworkspacesiov1alpha1.Image, error) {
	images := &kubeworkspacesiov1alpha1.ImageList{}
	if err := reader.List(ctx, images, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range images.Items {
		if images.Items[i].Spec.Image == imageRef {
			return &images.Items[i], nil
		}
	}
	return nil, nil
}

// cloudInitUserData derives the cloud-init user-data for a VM workspace from
// the image's declared defaults. Explicit DefaultUserData wins; otherwise, when
// the image opts in via DefaultCloudInit and provides a user/password, a
// minimal cloud-config is generated that sets the password on the account (the
// containerDisk root is ephemeral, so this applies on every boot). Empty
// user-data means no datasource is attached.
func cloudInitUserData(image *kubeworkspacesiov1alpha1.Image) string {
	if image == nil {
		return ""
	}
	if image.Spec.DefaultUserData != "" {
		return image.Spec.DefaultUserData
	}
	if image.Spec.DefaultCloudInit && image.Spec.DefaultUser != "" && image.Spec.DefaultPassword != "" {
		return fmt.Sprintf("#cloud-config\n"+
			"ssh_pwauth: true\n"+
			"chpasswd:\n"+
			"  expire: false\n"+
			"  list: |\n"+
			"    %s:%s\n",
			image.Spec.DefaultUser, image.Spec.DefaultPassword)
	}
	return ""
}

// virtualMachineNeedsUpdate compares the running flag and the VMI template.
func virtualMachineNeedsUpdate(desired, current *unstructured.Unstructured) bool {
	dRunning, _, _ := unstructured.NestedBool(desired.Object, "spec", "running")
	cRunning, _, _ := unstructured.NestedBool(current.Object, "spec", "running")
	if dRunning != cRunning {
		return true
	}
	dT, _, _ := unstructured.NestedMap(desired.Object, "spec", "template")
	cT, _, _ := unstructured.NestedMap(current.Object, "spec", "template")
	return !reflect.DeepEqual(dT, cT)
}

// deploymentNeedsUpdate compares replicas, containers, init containers, volumes.
// Uses apimachinery's Semantic (DeepDerivative-style) comparison, which treats
// API-server-defaulted empty maps (e.g. resources: {}) as equal to absent ones,
// avoiding a hot update loop.
func deploymentNeedsUpdate(desired, current *appsv1.Deployment) bool {
	if *desired.Spec.Replicas != *current.Spec.Replicas {
		return true
	}
	if !apiequality.Semantic.DeepEqual(desired.Spec.Template.Spec.Containers, current.Spec.Template.Spec.Containers) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(desired.Spec.Template.Spec.InitContainers, current.Spec.Template.Spec.InitContainers) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(desired.Spec.Template.Spec.Volumes, current.Spec.Template.Spec.Volumes) {
		return true
	}
	return false
}

// generateService creates the desired Service for a Workspace.
// It exposes all container ports: the first port is mapped to Service port 80 (named "http"),
// and additional ports are exposed on their own port number.
// The selector matches the workload type: StatefulSet pods for container,
// workspace-name labelled pods for vm and scratch.
func generateService(instance *kubeworkspacesiov1alpha1.Workspace, wsType string) *corev1.Service {
	servicePorts := []corev1.ServicePort{}

	if len(instance.Spec.Template.Spec.Containers) > 0 {
		containerPorts := instance.Spec.Template.Spec.Containers[0].Ports
		if len(containerPorts) > 0 && containerPorts[0].ContainerPort > 0 {
			// First port maps to Service port 80 (backward compatible)
			servicePorts = append(servicePorts, corev1.ServicePort{
				Name:       "http",
				Port:       DefaultServingPort,
				TargetPort: intstr.FromInt32(containerPorts[0].ContainerPort),
				Protocol:   "TCP",
			})
			// Additional ports are exposed on their own port number
			for i := 1; i < len(containerPorts); i++ {
				cp := containerPorts[i]
				name := cp.Name
				if name == "" {
					name = fmt.Sprintf("port-%d", cp.ContainerPort)
				}
				servicePorts = append(servicePorts, corev1.ServicePort{
					Name:       name,
					Port:       cp.ContainerPort,
					TargetPort: intstr.FromInt32(cp.ContainerPort),
					Protocol:   "TCP",
				})
			}
		}
	}

	// Fallback: if no ports were found, use defaults
	if len(servicePorts) == 0 {
		servicePorts = append(servicePorts, corev1.ServicePort{
			Name:       "http",
			Port:       DefaultServingPort,
			TargetPort: intstr.FromInt32(DefaultContainerPort),
			Protocol:   "TCP",
		})
	}

	selector := map[string]string{"statefulset": instance.Name}
	if wsType == WorkspaceTypeVM || wsType == WorkspaceTypeScratch {
		selector = map[string]string{LabelWorkspaceName: instance.Name}
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector,
			Ports:    servicePorts,
		},
	}
}

// statefulSetNeedsUpdate checks if the StatefulSet needs to be updated.
// We compare replicas, containers, init containers, volumes, and security context.
// Uses apimachinery semantic equality to tolerate API-server-defaulted fields.
func statefulSetNeedsUpdate(desired, current *appsv1.StatefulSet) bool {
	if *desired.Spec.Replicas != *current.Spec.Replicas {
		return true
	}
	if !apiequality.Semantic.DeepEqual(desired.Spec.Template.Spec.Containers, current.Spec.Template.Spec.Containers) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(desired.Spec.Template.Spec.InitContainers, current.Spec.Template.Spec.InitContainers) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(desired.Spec.Template.Spec.Volumes, current.Spec.Template.Spec.Volumes) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(desired.Spec.Template.Spec.SecurityContext, current.Spec.Template.Spec.SecurityContext) {
		return true
	}
	return false
}

// serviceNeedsUpdate checks if the Service needs to be updated.
func serviceNeedsUpdate(desired, current *corev1.Service) bool {
	if !reflect.DeepEqual(desired.Spec.Ports, current.Spec.Ports) {
		return true
	}
	if !reflect.DeepEqual(desired.Spec.Selector, current.Spec.Selector) {
		return true
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Map function to convert pod events to reconciliation requests
	mapPodToRequest := handler.MapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
		if nbName, ok := object.GetLabels()[LabelWorkspaceName]; ok {
			return []reconcile.Request{
				{NamespacedName: types.NamespacedName{
					Name:      nbName,
					Namespace: object.GetNamespace(),
				}},
			}
		}
		return nil
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&kubeworkspacesiov1alpha1.Workspace{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(mapPodToRequest)).
		Named("workspace").
		Complete(r)
}
