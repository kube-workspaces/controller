package controller

import (
	"context"
	"testing"

	kubeworkspacesv1 "github.com/kube-workspaces/controller/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestWorkspaceCountsReflectTransitionsAndDeletion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kubeworkspacesv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	a := &kubeworkspacesv1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"}, Status: kubeworkspacesv1.WorkspaceStatus{ReadyReplicas: 1}}
	b := a.DeepCopy()
	b.Name = "b"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(a, b).Build()
	reg := prometheus.NewRegistry()
	reg.MustRegister(&workspaceCounts{reader: c})
	count := func(state string) float64 {
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			for _, metric := range family.Metric {
				for _, label := range metric.Label {
					if label.GetName() == "status" && label.GetValue() == state {
						return metric.Gauge.GetValue()
					}
				}
			}
		}
		return -1
	}
	if count("running") != 2 {
		t.Fatal("multiple workspaces were not counted")
	}
	a.Annotations = map[string]string{AnnotationStopped: "true"}
	if err := c.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if count("running") != 1 || count("stopped") != 1 {
		t.Fatal("status transition retained stale count")
	}
	if err := c.Delete(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if count("running") != 0 {
		t.Fatal("deletion retained stale count")
	}
}
