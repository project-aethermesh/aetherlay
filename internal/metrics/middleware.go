package metrics

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
)

// Middleware instruments HTTP requests with Prometheus metrics.
// It tracks in-flight requests, total requests, and request duration.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get the route pattern from the router. This gives us a stable
		// name for the route, like "/v1/chain/{chain}/status", instead
		// of the full request path.
		route := "unknown"
		currentRoute := mux.CurrentRoute(r)
		if currentRoute != nil {
			if tmpl, err := currentRoute.GetPathTemplate(); err == nil {
				route = tmpl
			}
		}

		// Instrument the request with our metrics
		instrumentedWriter := newResponseWriter(w)
		handler := instrumentHandler(next, instrumentedWriter, route)

		// Serve the request
		handler.ServeHTTP(instrumentedWriter, r)
	})
}

// responseWriter is a wrapper around http.ResponseWriter to capture the status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	wRotten    bool // Track if the ResponseWriter has been hijacked
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{w, http.StatusOK, false}
}

// WriteHeader captures the status code and calls the original WriteHeader
func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Hijack implements the http.Hijacker interface to allow for WebSocket upgrades.
// It marks the writer as "rotten" to prevent metrics from being recorded
// for the hijacked connection, as the lifecycle is no longer standard HTTP.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("http.Hijacker is not implemented by the underlying http.ResponseWriter")
	}
	rw.wRotten = true
	return h.Hijack()
}

// instrumentHandler wraps the handler with Prometheus instrumentation.
func instrumentHandler(handler http.Handler, w *responseWriter, route string) http.Handler {
	// Increment the in-flight gauge
	HTTPRequestsInFlight.Inc()

	return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Start the timer
		timer := prometheus.NewTimer(HTTPRequestDuration.WithLabelValues(
			"", // code is set after the request
			strings.ToLower(r.Method),
			route,
		))

		// Defer the decrementing of the in-flight gauge and recording of metrics
		defer func() {
			HTTPRequestsInFlight.Dec()
			// Only record metrics if the connection was not hijacked
			if !w.wRotten {
				statusCode := fmt.Sprintf("%d", w.statusCode)
				// Update the duration histogram with the correct status code
				timer.ObserveDuration()
				// Increment the total requests counter
				HTTPRequestsTotal.WithLabelValues(
					statusCode,
					strings.ToLower(r.Method),
					route,
				).Inc()
			}
		}()

		// Call the next handler in the chain
		handler.ServeHTTP(w, r)
	})
}
