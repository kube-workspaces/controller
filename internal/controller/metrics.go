package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// WorkspacesTotal is a gauge tracking the total number of workspaces by namespace and status.
	WorkspacesTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kubeworkspaces_workspaces_total",
			Help: "Total number of workspaces by namespace and status",
		},
		[]string{"namespace", "status"},
	)

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
		WorkspacesTotal,
		ReconcileTotal,
		ReconcileDuration,
		WorkspaceReadyTime,
	)
}
