// Package metrics defines every series the demo workload exposes.
//
// These are declared in one place because they are a contract, not an implementation
// detail: M5 scales on queue depth, M7 charts throughput and latency, and M4 correlates
// cost against them. Renaming a series here breaks a scaler and a dashboard, so changes
// belong in the same review as the manifests that consume them.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// QueueDepth is the demand signal M5 scales on. It is published by the API from
	// Redis LLEN rather than counted per-process, so the value is the true shared
	// backlog: adding replicas must actually reduce it. A per-process counter would
	// rise and fall with replica count and make the scaling demo meaningless.
	QueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "app_queue_depth",
		Help: "Number of jobs currently waiting in the shared queue.",
	})

	// QueueDepthScrapeErrors lets a dashboard distinguish "the queue is empty" from
	// "we cannot see the queue", two states that look identical on a depth gauge
	// alone and would otherwise cause M5 to scale to minimum during an outage.
	QueueDepthScrapeErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "app_queue_depth_scrape_errors_total",
		Help: "Failures to read queue depth from the backing store.",
	})

	JobsEnqueued = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "app_jobs_enqueued_total",
		Help: "Jobs accepted by the API and pushed onto the queue.",
	}, []string{"outcome"})

	JobsProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "app_jobs_processed_total",
		Help: "Jobs consumed by a worker.",
	}, []string{"outcome"})

	JobDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "app_job_duration_seconds",
		Help:    "Wall-clock time a worker spent executing a job.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 12), // 5ms .. ~20s
	})

	// JobWaitTime is the queueing delay, the metric that actually degrades under load
	// and recovers after M5 scales up. It is the honest measure of whether autoscaling
	// helped, whereas job duration stays flat no matter how backed up the queue is.
	JobWaitTime = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "app_job_wait_seconds",
		Help:    "Time a job spent waiting in the queue before a worker picked it up.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 14), // 10ms .. ~160s
	})

	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "app_http_requests_total",
		Help: "HTTP requests handled by the API.",
	}, []string{"path", "method", "code"})

	HTTPDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "app_http_request_duration_seconds",
		Help:    "HTTP request latency.",
		Buckets: prometheus.DefBuckets,
	}, []string{"path"})
)
