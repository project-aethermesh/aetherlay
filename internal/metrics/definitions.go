package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

// HTTP Metrics for the Load Balancer
var (
	// HTTPRequestDuration tracks the duration of HTTP requests in seconds, labeled by status code, method, and route.
	HTTPRequestDuration *prometheus.HistogramVec
	// HTTPRequestsInFlight tracks the current number of in-flight HTTP requests.
	HTTPRequestsInFlight prometheus.Gauge
	// HTTPRequestsTotal counts the total number of HTTP requests, labeled by status code, method, and route.
	HTTPRequestsTotal *prometheus.CounterVec
)

// Health Checker Metrics
var (
	// EndpointHealthStatus tracks the current health status of each endpoint (1 = healthy, 0 = unhealthy).
	EndpointHealthStatus *prometheus.GaugeVec
	// HealthCheckDuration tracks the duration of health checks in seconds, labeled by chain and endpoint.
	HealthCheckDuration *prometheus.HistogramVec
	// HealthCheckTotal counts the total number of health checks, labeled by chain, endpoint, and status.
	HealthCheckTotal *prometheus.CounterVec
)

// init initializes all metrics with error handling
func init() {
	initHTTPMetrics()
	initHealthMetrics()
}

// initHTTPMetrics initializes HTTP-related metrics
func initHTTPMetrics() {
	HTTPRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "aetherlay_http_request_duration_seconds",
			Help:    "Duration of HTTP requests.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"code", "method", "route"},
	)
	if HTTPRequestDuration == nil {
		log.Warn().Msg("Failed to register metricaetherlay_http_request_duration_seconds")
	}

	HTTPRequestsInFlight = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "aetherlay_http_requests_in_flight",
			Help: "Current number of in-flight HTTP requests.",
		},
	)
	if HTTPRequestsInFlight == nil {
		log.Warn().Msg("Failed to register metricaetherlay_http_requests_in_flight")
	}

	HTTPRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aetherlay_http_requests_total",
			Help: "Total number of HTTP requests.",
		},
		[]string{"code", "method", "route"},
	)
	if HTTPRequestsTotal == nil {
		log.Warn().Msg("Failed to register metricaetherlay_http_requests_total")
	}
}

// initHealthMetrics initializes health check-related metrics
func initHealthMetrics() {
	EndpointHealthStatus = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "aetherlay_endpoint_health_status",
			Help: "Current health status of an endpoint (1 for healthy, 0 for unhealthy).",
		},
		[]string{"chain", "endpoint"},
	)
	if EndpointHealthStatus == nil {
		log.Warn().Msg("Failed to register metricaetherlay_endpoint_health_status")
	}

	HealthCheckDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "aetherlay_health_check_duration_seconds",
			Help:    "Duration of health checks.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"chain", "endpoint"},
	)
	if HealthCheckDuration == nil {
		log.Warn().Msg("Failed to register metric aetherlay_health_check_duration_seconds")
	}

	HealthCheckTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aetherlay_health_check_total",
			Help: "Total number of health checks.",
		},
		[]string{"chain", "endpoint", "status"}, // status can be "success" or "failure"
	)
	if HealthCheckTotal == nil {
		log.Warn().Msg("Failed to register metricaetherlay_health_check_total")
	}
}
