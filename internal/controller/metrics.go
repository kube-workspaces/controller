package controller

import (
	"context"
	"time"

	kubeworkspacesv1 "github.com/kube-workspaces/controller/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// ReconcileTotal is a counter tracking reconciliation attempts by result.
	ReconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kubeworkspaces_workspace_reconcile_total",
			Help: "Total number of workspace reconciliations by result",
		},
		[]string{"result"},
	)

	// ReconcileDuration is a histogram tracking reconciliation duration in seconds.
	ReconcileDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "kubeworkspaces_workspace_reconcile_duration_seconds",
			Help:    "Duration of workspace reconciliation in seconds",
			Buckets: prometheus.DefBuckets,
		},
	)

	// WorkspaceReadyTime is a histogram tracking the time from workspace creation to ready state.
	WorkspaceReadyTime = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "kubeworkspaces_workspace_ready_time_seconds",
			Help:    "Time from workspace creation to ready state in seconds",
			Buckets: []float64{5, 10, 30, 60, 120, 300, 600},
		},
	)
)

func init() {
	metrics.Registry.MustRegister(
		ReconcileTotal,
		ReconcileDuration,
		WorkspaceReadyTime,
	)
}

// Counts are derived from the manager cache at scrape time, so transitions and
// deletions cannot leave stale status series behind. No reconcile-time Set(1).
type workspaceCounts struct{ reader client.Reader }

var workspaceCountDesc = prometheus.NewDesc("kubeworkspaces_workspaces_total", "Total number of workspaces by namespace and status", []string{"namespace", "status"}, nil)

func (*workspaceCounts) Describe(ch chan<- *prometheus.Desc) { ch <- workspaceCountDesc }
func (c *workspaceCounts) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var list kubeworkspacesv1.WorkspaceList
	if err := c.reader.List(ctx, &list); err != nil {
		ch <- prometheus.NewInvalidMetric(workspaceCountDesc, err)
		return
	}
	counts := map[string]map[string]float64{}
	for _, ws := range list.Items {
		if counts[ws.Namespace] == nil {
			counts[ws.Namespace] = map[string]float64{"running": 0, "stopped": 0, "starting": 0}
		}
		state := "starting"
		if ws.Status.ReadyReplicas > 0 {
			state = "running"
		}
		if _, stopped := ws.Annotations[AnnotationStopped]; stopped {
			state = "stopped"
		}
		counts[ws.Namespace][state]++
	}
	for ns, states := range counts {
		for state, n := range states {
			ch <- prometheus.MustNewConstMetric(workspaceCountDesc, prometheus.GaugeValue, n, ns, state)
		}
	}
}
func registerWorkspaceCounts(reader client.Reader) error {
	return metrics.Registry.Register(&workspaceCounts{reader: reader})
}
