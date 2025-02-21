package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Global Prometheus Metrics
var (
	DonorPublishedTasksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "donor_published_tasks_total",
			Help: "Total tasks published by donor",
		},
		[]string{"donorUUID"},
	)

	DonorResultsReceivedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "donor_results_received_total",
			Help: "Total results received by donor",
		},
		[]string{"donorUUID"},
	)

	StolenTasksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tasks_stolen_total",
			Help: "Total number of pods stolen per donor-stealer pair",
		},
		[]string{"donorUUID", "stealerUUID"},
	)

	StealerClaimedTasksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "stealer_claimed_tasks_total",
			Help: "Total number of tasks claimed by Stealer",
		},
		[]string{"stealerUUID"},
	)

	StealerProcessingDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "stealer_processing_duration_seconds",
			Help:    "Time taken by stealer to process tasks",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"stealerUUID"},
	)
)

// RegisterDonorMetrics registers donor metrics with Prometheus
func RegisterDonorMetrics() {
	prometheus.MustRegister(DonorPublishedTasksTotal, DonorResultsReceivedTotal)
}

// RegisterStealerMetrics registers stealer metrics with Prometheus
func RegisterStealerMetrics() {
	prometheus.MustRegister(StolenTasksTotal, StealerClaimedTasksTotal, StealerProcessingDuration)
}

// StartMetricsServer starts an HTTP server for Prometheus metrics
func StartDonorMetricsServer(mPath string, mPort string) {
	RegisterDonorMetrics()
	http.Handle(mPath, promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	go http.ListenAndServe(mPort, nil)
}

func StartStealerMetricsServer(mPath string, mPort string) {
	RegisterStealerMetrics()
	http.Handle(mPath, promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	go http.ListenAndServe(mPort, nil)
}
