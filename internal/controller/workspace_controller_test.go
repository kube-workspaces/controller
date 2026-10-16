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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeworkspacesiov1alpha1 "github.com/kube-workspaces/controller/api/v1alpha1"
)

var _ = Describe("Workspace Controller", func() {
	for _, workspaceType := range []string{WorkspaceTypeContainer, WorkspaceTypeScratch} {
		Context("When reconciling a "+workspaceType+" resource", func() {
			const resourceName = "test-resource"

			ctx := context.Background()

			typeNamespacedName := types.NamespacedName{
				Name:      resourceName,
				Namespace: "default", // TODO(user):Modify as needed
			}
			workspace := &kubeworkspacesiov1alpha1.Workspace{}

			BeforeEach(func() {
				By("creating the custom resource for the Kind Workspace")
				err := k8sClient.Get(ctx, typeNamespacedName, workspace)
				if err != nil && errors.IsNotFound(err) {
					resource := &kubeworkspacesiov1alpha1.Workspace{
						ObjectMeta: metav1.ObjectMeta{
							Name:      resourceName,
							Namespace: "default",
						},
						Spec: kubeworkspacesiov1alpha1.WorkspaceSpec{
							Type: workspaceType,
							Template: kubeworkspacesiov1alpha1.WorkspaceTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "workspace",
											Image: "codercom/code-server:latest",
										},
									},
								},
							},
						},
					}
					Expect(k8sClient.Create(ctx, resource)).To(Succeed())
				}
				Expect(k8sClient.Get(ctx, typeNamespacedName, workspace)).To(Succeed())
			})

			AfterEach(func() {
				// Envtest does not run garbage collection, so remove owned resources explicitly.
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"}}))).To(Succeed())
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"}}))).To(Succeed())
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"}}))).To(Succeed())
				resource := &kubeworkspacesiov1alpha1.Workspace{}
				err := k8sClient.Get(ctx, typeNamespacedName, resource)
				Expect(err).NotTo(HaveOccurred())

				By("Cleanup the specific resource instance Workspace")
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			})
			It("should create, update, stop and restart the workload without redundant writes", func() {
				By("Reconciling the created resource")
				controllerReconciler := &WorkspaceReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				reconcileWorkspace := func() {
					_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
					Expect(err).NotTo(HaveOccurred())
				}
				loadWorkload := func() (client.Object, corev1.PodTemplateSpec, int32) {
					if workspaceType == WorkspaceTypeContainer {
						ss := &appsv1.StatefulSet{}
						Expect(k8sClient.Get(ctx, typeNamespacedName, ss)).To(Succeed())
						return ss, ss.Spec.Template, *ss.Spec.Replicas
					}
					dep := &appsv1.Deployment{}
					Expect(k8sClient.Get(ctx, typeNamespacedName, dep)).To(Succeed())
					return dep, dep.Spec.Template, *dep.Spec.Replicas
				}
				reconcileWorkspace()
				workload, template, replicas := loadWorkload()
				Expect(replicas).To(Equal(int32(1)))
				Expect(template.Spec.Containers[0].Image).To(Equal("codercom/code-server:latest"))
				Expect(template.Spec.Containers[0].Ports[0].ContainerPort).To(Equal(int32(DefaultContainerPort)))
				Expect(workload.GetOwnerReferences()).To(HaveLen(1))
				Expect(workload.GetOwnerReferences()[0].UID).To(Equal(workspace.UID))
				svc := &corev1.Service{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, svc)).To(Succeed())
				Expect(svc.Spec.Ports[0].Port).To(Equal(int32(DefaultServingPort)))
				Expect(svc.Spec.Ports[0].TargetPort.IntVal).To(Equal(int32(DefaultContainerPort)))
				for key, value := range svc.Spec.Selector {
					Expect(template.Labels[key]).To(Equal(value))
				}
				Expect(svc.OwnerReferences[0].UID).To(Equal(workspace.UID))
				reconcileWorkspace()
				unchanged, _, _ := loadWorkload()
				Expect(unchanged.GetResourceVersion()).To(Equal(workload.GetResourceVersion()))
				unchangedSvc := &corev1.Service{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, unchangedSvc)).To(Succeed())
				Expect(unchangedSvc.ResourceVersion).To(Equal(svc.ResourceVersion))

				Expect(k8sClient.Get(ctx, typeNamespacedName, workspace)).To(Succeed())
				workspace.Spec.Template.Spec.NodeSelector = map[string]string{"pool": "workspaces"}
				workspace.Spec.Template.Spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}}
				workspace.Spec.Template.Spec.ServiceAccountName = "workspace-runner"
				nonRoot := true
				workspace.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: &nonRoot}
				workspace.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "pool", Operator: corev1.NodeSelectorOpIn, Values: []string{"workspaces"}}},
					}}},
				}}
				Expect(k8sClient.Update(ctx, workspace)).To(Succeed())
				reconcileWorkspace()
				_, template, _ = loadWorkload()
				Expect(template.Spec.NodeSelector).To(Equal(workspace.Spec.Template.Spec.NodeSelector))
				Expect(template.Spec.Tolerations).To(Equal(workspace.Spec.Template.Spec.Tolerations))
				Expect(template.Spec.Affinity).To(Equal(workspace.Spec.Template.Spec.Affinity))
				Expect(template.Spec.ServiceAccountName).To(Equal("workspace-runner"))
				Expect(template.Spec.SecurityContext.RunAsNonRoot).To(HaveValue(BeTrue()))
				Expect(k8sClient.Get(ctx, typeNamespacedName, workspace)).To(Succeed())
				workspace.Spec.Template.Spec.Containers[0].Image = "codercom/code-server:updated"
				workspace.Labels = map[string]string{"team": "example"}
				workspace.Annotations = map[string]string{"example.com/config": "updated", AnnotationStopped: "true"}
				Expect(k8sClient.Update(ctx, workspace)).To(Succeed())
				reconcileWorkspace()
				_, template, replicas = loadWorkload()
				Expect(replicas).To(BeZero())
				Expect(template.Spec.NodeSelector).To(Equal(workspace.Spec.Template.Spec.NodeSelector))
				Expect(template.Spec.Tolerations).To(Equal(workspace.Spec.Template.Spec.Tolerations))
				Expect(template.Spec.ServiceAccountName).To(Equal("workspace-runner"))
				Expect(template.Spec.SecurityContext.RunAsNonRoot).To(HaveValue(BeTrue()))
				Expect(template.Spec.Containers[0].Image).To(Equal("codercom/code-server:updated"))
				Expect(template.Labels["team"]).To(Equal("example"))
				Expect(template.Annotations["example.com/config"]).To(Equal("updated"))

				Expect(k8sClient.Get(ctx, typeNamespacedName, workspace)).To(Succeed())
				delete(workspace.Annotations, AnnotationStopped)
				delete(workspace.Annotations, "example.com/config")
				delete(workspace.Labels, "team")
				workspace.Spec.Template.Spec.NodeSelector = nil
				workspace.Spec.Template.Spec.Tolerations = nil
				workspace.Spec.Template.Spec.Affinity = nil
				workspace.Spec.Template.Spec.SecurityContext = nil
				workspace.Spec.Template.Spec.ServiceAccountName = ""
				Expect(k8sClient.Update(ctx, workspace)).To(Succeed())
				reconcileWorkspace()
				workload, template, replicas = loadWorkload()
				Expect(replicas).To(Equal(int32(1)))
				Expect(template.Spec.NodeSelector).To(BeEmpty())
				Expect(template.Spec.Tolerations).To(BeEmpty())
				Expect(template.Spec.Affinity).To(BeNil())
				Expect(template.Spec.ServiceAccountName).To(BeEmpty())
				Expect(template.Spec.SecurityContext.RunAsNonRoot).To(BeNil())
				Expect(template.Labels).NotTo(HaveKey("team"))
				Expect(template.Annotations).NotTo(HaveKey("example.com/config"))
				reconcileWorkspace()
				unchanged, _, _ = loadWorkload()
				Expect(unchanged.GetResourceVersion()).To(Equal(workload.GetResourceVersion()))
			})
		})
	}
})
