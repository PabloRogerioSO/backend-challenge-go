package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests",
	}, []string{"method", "path", "status"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request duration in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	SQSDuplicatesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sqs_duplicates_total",
		Help: "Total number of duplicate SQS messages ignored by the inbox",
	})

	OutboxPublishErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "outbox_publish_errors_total",
		Help: "Total number of errors publishing outbox events",
	})

	ReconcileDivergencesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "reconcile_divergences_total",
		Help: "Total number of reconciliation divergences detected",
	})
)

// Register explicitly initializes the metrics.
// promauto already registers them on package import, but this function
// can be called from main for explicitness.
func Register() {
	// Force initialization of labels to show up as 0 in Prometheus
	HTTPRequestsTotal.WithLabelValues("GET", "/", "200")
	HTTPRequestDuration.WithLabelValues("GET", "/")
}
