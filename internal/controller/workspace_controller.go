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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	// LabelWorkspaceName is the label applied to pods to identify the workspace
	LabelWorkspaceName = "workspace-name"
)

// WorkspaceReconciler reconciles a Workspace object
type WorkspaceReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	EventRecorder record.EventRecorder
}

// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=images,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=workspaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubeworkspaces.io,resources=workspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is the main reconciliation loop for Workspace resources.
// It ensures a StatefulSet and Service exist for each Workspace CR and
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

	// Reconcile StatefulSet
	ss := generateStatefulSet(instance)
	if err := ctrl.SetControllerReference(instance, ss, r.Scheme); err != nil {
		return ctrl.Result{}, err
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
			return ctrl.Result{}, err
		}
	} else if err != nil {
		log.Error(err, "error getting StatefulSet")
		return ctrl.Result{}, err
	}

	// Update the StatefulSet if needed
	if !justCreated && statefulSetNeedsUpdate(ss, foundStateful) {
		log.Info("Updating StatefulSet", "namespace", ss.Namespace, "name", ss.Name)
		foundStateful.Spec.Replicas = ss.Spec.Replicas
		foundStateful.Spec.Template.Spec.Containers = ss.Spec.Template.Spec.Containers
		err = r.Update(ctx, foundStateful)
		if err != nil {
			log.Error(err, "unable to update StatefulSet")
			return ctrl.Result{}, err
		}
	}

	// Reconcile Service
	service := generateService(instance)
	if err := ctrl.SetControllerReference(instance, service, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	foundService := &corev1.Service{}
	justCreated = false
	err = r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, foundService)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating Service", "namespace", service.Namespace, "name", service.Name)
		err = r.Create(ctx, service)
		justCreated = true
		if err != nil {
			log.Error(err, "unable to create Service")
			return ctrl.Result{}, err
		}
	} else if err != nil {
		log.Error(err, "error getting Service")
		return ctrl.Result{}, err
	}

	// Update the Service if needed
	if !justCreated && serviceNeedsUpdate(service, foundService) {
		log.Info("Updating Service", "namespace", service.Namespace, "name", service.Name)
		foundService.Spec.Ports = service.Spec.Ports
		foundService.Spec.Selector = service.Spec.Selector
		err = r.Update(ctx, foundService)
		if err != nil {
			log.Error(err, "unable to update Service")
			return ctrl.Result{}, err
		}
	}

	// Get the workspace pod for status
	foundPod := &corev1.Pod{}
	err = r.Get(ctx, types.NamespacedName{Name: ss.Name + "-0", Namespace: ss.Namespace}, foundPod)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info(fmt.Sprintf("No pods are currently running for workspace: %s/%s", instance.Namespace, instance.Name))
		foundPod = &corev1.Pod{}
	} else if err != nil {
		return ctrl.Result{}, err
	}

	// Update Workspace CR status
	if err := r.updateWorkspaceStatus(ctx, instance, foundStateful, foundPod); err != nil {
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
	} else if foundStateful.Status.ReadyReplicas > 0 {
		WorkspacesTotal.WithLabelValues(instance.Namespace, "running").Set(1)
	} else {
		WorkspacesTotal.WithLabelValues(instance.Namespace, "starting").Set(1)
	}

	// Record ready time (time from creation to first ready state)
	if foundStateful.Status.ReadyReplicas > 0 && !instance.CreationTimestamp.IsZero() {
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

// updateWorkspaceStatus updates the Workspace CR status based on the StatefulSet and Pod state.
func (r *WorkspaceReconciler) updateWorkspaceStatus(ctx context.Context,
	ws *kubeworkspacesiov1alpha1.Workspace, sts *appsv1.StatefulSet, pod *corev1.Pod) error {

	log := logf.FromContext(ctx)

	status := kubeworkspacesiov1alpha1.WorkspaceStatus{
		Conditions:     make([]kubeworkspacesiov1alpha1.WorkspaceCondition, 0),
		ReadyReplicas:  sts.Status.ReadyReplicas,
		ContainerState: corev1.ContainerState{},
	}

	// Update the status based on the Pod's status
	if !reflect.DeepEqual(pod.Status, corev1.PodStatus{}) {
		// Use the first container status (single-container workspaces)
		// or match by the container name from the workspace spec
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

// generateService creates the desired Service for a Workspace.
// It exposes all container ports: the first port is mapped to Service port 80 (named "http"),
// and additional ports are exposed on their own port number.
func generateService(instance *kubeworkspacesiov1alpha1.Workspace) *corev1.Service {
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

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"statefulset": instance.Name},
			Ports:    servicePorts,
		},
	}
}

// statefulSetNeedsUpdate checks if the StatefulSet needs to be updated.
// We compare replicas, containers, init containers, volumes, and security context.
func statefulSetNeedsUpdate(desired, current *appsv1.StatefulSet) bool {
	if *desired.Spec.Replicas != *current.Spec.Replicas {
		return true
	}
	if !reflect.DeepEqual(desired.Spec.Template.Spec.Containers, current.Spec.Template.Spec.Containers) {
		return true
	}
	if !reflect.DeepEqual(desired.Spec.Template.Spec.InitContainers, current.Spec.Template.Spec.InitContainers) {
		return true
	}
	if !reflect.DeepEqual(desired.Spec.Template.Spec.Volumes, current.Spec.Template.Spec.Volumes) {
		return true
	}
	if !reflect.DeepEqual(desired.Spec.Template.Spec.SecurityContext, current.Spec.Template.Spec.SecurityContext) {
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
		Owns(&corev1.Service{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(mapPodToRequest)).
		Named("workspace").
		Complete(r)
}
