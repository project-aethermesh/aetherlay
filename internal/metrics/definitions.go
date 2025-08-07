package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// HTTP Metrics for the Load Balancer
var (
	HTTPRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests.",
		},
		[]string{"code", "method", "route"},
	)

	HTTPRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Duration of HTTP requests.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"code", "method", "route"},
	)

	HTTPRequestsInFlight = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Current number of in-flight HTTP requests.",
		},
	)
)

// Health Checker Metrics
var (
	HealthCheckTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "health_check_total",
			Help: "Total number of health checks.",
		},
		[]string{"chain", "endpoint", "status"}, // status can be "success" or "failure"
	)

	HealthCheckDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "health_check_duration_seconds",
			Help:    "Duration of health checks.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"chain", "endpoint"},
	)

	EndpointHealthStatus = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "endpoint_health_status",
			Help: "Current health status of an endpoint (1 for healthy, 0 for unhealthy).",
		},
		[]string{"chain", "endpoint"},
	)
)
