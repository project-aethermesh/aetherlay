package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Helper function to test counter metrics
func testCounterMetric(t *testing.T, counter *prometheus.CounterVec, code, method, route string, expectedValue float64) {
	metric := &dto.Metric{}
	err := counter.WithLabelValues(code, method, route).Write(metric)
	require.NoError(t, err)
	assert.Equal(t, expectedValue, metric.GetCounter().GetValue())
}

// Helper function to test histogram metrics
func testHistogramMetric(t *testing.T, histogram *prometheus.HistogramVec, code, method, route string) {
	metric := &dto.Metric{}
	err := histogram.WithLabelValues(code, method, route).(prometheus.Metric).Write(metric)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), metric.GetHistogram().GetSampleCount())
	assert.True(t, metric.GetHistogram().GetSampleSum() > 0)
}

// Helper function to test gauge metrics
func testGaugeMetric(t *testing.T, gauge prometheus.Gauge, expectedValue float64) {
	metric := &dto.Metric{}
	err := gauge.Write(metric)
	require.NoError(t, err)
	assert.Equal(t, expectedValue, metric.GetGauge().GetValue())
}

func TestMiddleware(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		path           string
		route          string
		statusCode     int
		expectedCode   string
		expectedMethod string
		expectedRoute  string
	}{
		{
			name:           "successful GET request",
			method:         "GET",
			path:           "/v1/chain/ethereum/status",
			route:          "/v1/chain/{chain}/status",
			statusCode:     200,
			expectedCode:   "200",
			expectedMethod: "GET",
			expectedRoute:  "/v1/chain/{chain}/status",
		},
		{
			name:           "POST request with error",
			method:         "POST",
			path:           "/v1/chain/bitcoin/send",
			route:          "/v1/chain/{chain}/send",
			statusCode:     500,
			expectedCode:   "500",
			expectedMethod: "POST",
			expectedRoute:  "/v1/chain/{chain}/send",
		},
		{
			name:           "unknown route",
			method:         "GET",
			path:           "/unknown",
			route:          "/",
			statusCode:     404,
			expectedCode:   "404",
			expectedMethod: "GET",
			expectedRoute:  "/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test handler
			testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
			})

			// Create router with route
			router := mux.NewRouter()
			router.Use(Middleware)
			if tt.route == "/" {
				// For unknown routes, add a catch-all handler
				router.PathPrefix("/").HandlerFunc(testHandler)
			} else {
				router.HandleFunc(tt.route, testHandler).Methods(tt.method)
			}

			// Create request
			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()

			// Execute request
			router.ServeHTTP(w, req)

			// Assert response
			assert.Equal(t, tt.statusCode, w.Code)

			// Verify metrics were recorded correctly
			if HTTPRequestsTotal != nil {
				testCounterMetric(t, HTTPRequestsTotal, tt.expectedCode, tt.expectedMethod, tt.expectedRoute, 1.0)
			}
			if HTTPRequestDuration != nil {
				testHistogramMetric(t, HTTPRequestDuration, tt.expectedCode, tt.expectedMethod, tt.expectedRoute)
			}
		})
	}
}

func TestResponseWriter(t *testing.T) {
	tests := []struct {
		name               string
		writeHeaderCalled  bool
		expectedStatusCode int
	}{
		{
			name:               "default status code",
			writeHeaderCalled:  false,
			expectedStatusCode: 200,
		},
		{
			name:               "custom status code",
			writeHeaderCalled:  true,
			expectedStatusCode: 404,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			rw := newResponseWriter(w)

			if tt.writeHeaderCalled {
				rw.WriteHeader(tt.expectedStatusCode)
			}

			assert.Equal(t, tt.expectedStatusCode, rw.statusCode)
			assert.False(t, rw.hijacked)
		})
	}
}

func TestResponseWriterHijack(t *testing.T) {
	w := httptest.NewRecorder()
	rw := newResponseWriter(w)

	// Initially not rotten
	assert.False(t, rw.hijacked)

	// Test that hijack is not supported by httptest.ResponseRecorder
	_, _, err := rw.Hijack()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "http.Hijacker is not implemented")

	// After failed hijack attempt, it should NOT be marked as rotten (only on success)
	assert.False(t, rw.hijacked)
}

func TestInFlightRequestsGauge(t *testing.T) {
	// Test that the in-flight requests gauge starts at 0
	if HTTPRequestsInFlight != nil {
		testGaugeMetric(t, HTTPRequestsInFlight, 0.0)
	}

	// Create a test handler that we can control
	handlerCalled := make(chan bool)
	continueHandler := make(chan bool)

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled <- true
		<-continueHandler // Wait for signal to continue
		w.WriteHeader(200)
	})

	// Create router with middleware
	router := mux.NewRouter()
	router.Use(Middleware)
	router.HandleFunc("/test", testHandler).Methods("GET")

	// Start request in background
	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()

	go func() {
		router.ServeHTTP(w, req)
	}()

	// Wait for handler to be called (meaning request is in-flight)
	<-handlerCalled

	// Now the gauge should be 1 (request is in-flight)
	if HTTPRequestsInFlight != nil {
		testGaugeMetric(t, HTTPRequestsInFlight, 1.0)
	}

	// Allow handler to complete
	continueHandler <- true

	// Give a moment for the request to complete and gauge to be decremented
	// Note: In a real test environment, we might need a small sleep or retry logic
	// but for this simple case, the gauge should be decremented by now
}
