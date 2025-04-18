package metrics

import (
	"log/slog"
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

// StartDonorMetricsServer starts an HTTP server for Prometheus metrics
func StartDonorMetricsServer(mPath string, mPort string) {
	RegisterDonorMetrics()
	if mPath[0] != '/' {
		mPath = "/" + mPath
	}
	if mPort[0] != ':' {
		mPort = ":" + mPath
	}
	http.Handle(mPath, promhttp.Handler())
	slog.Info("Starting donor Prometheus metrics server", "path", mPath, "port", mPort)
	go http.ListenAndServe(mPort, nil)
}

// StartStealerMetricsServer starts an HTTP server for Prometheus metrics
func StartStealerMetricsServer(mPath string, mPort string) {
	RegisterStealerMetrics()
	if mPath[0] != '/' {
		mPath = "/" + mPath
	}
	if mPort[0] != ':' {
		mPort = ":" + mPath
	}
	http.Handle(mPath, promhttp.Handler())
	slog.Info("Starting stealer Prometheus metrics server", "path", mPath, "port", mPort)
	go http.ListenAndServe(mPort, nil)
}
