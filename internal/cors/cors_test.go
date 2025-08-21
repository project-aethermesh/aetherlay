package cors

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestCORSMiddleware(t *testing.T) {
	// Create a test handler
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("test response"))
	})

	// Test with default environment variables
	t.Run("Default CORS configuration", func(t *testing.T) {
		corsHandler := Middleware(testHandler, "Accept, Authorization, Content-Type, Origin, X-Requested-With", "GET, POST, OPTIONS", "*")

		// Test POST request
		t.Run("POST request with CORS headers", func(t *testing.T) {
			req := httptest.NewRequest("POST", "/test", nil)
			req.Header.Set("Origin", "http://example.com")
			w := httptest.NewRecorder()

			corsHandler.ServeHTTP(w, req)

			// Check CORS headers
			if got := w.Header().Get("Access-Control-Allow-Headers"); got != "Accept, Authorization, Content-Type, Origin, X-Requested-With" {
				t.Errorf("Access-Control-Allow-Headers = %v, want 'Accept, Authorization, Content-Type, Origin, X-Requested-With'", got)
			}
			if got := w.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST, OPTIONS" {
				t.Errorf("Access-Control-Allow-Methods = %v, want 'GET, POST, OPTIONS'", got)
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
				t.Errorf("Access-Control-Allow-Origin = %v, want *", got)
			}

			// Check that the response was processed normally
			if w.Code != http.StatusOK {
				t.Errorf("Status code = %v, want %v", w.Code, http.StatusOK)
			}
			if body := w.Body.String(); body != "test response" {
				t.Errorf("Body = %v, want 'test response'", body)
			}
		})

		// Test OPTIONS preflight request
		t.Run("OPTIONS preflight request", func(t *testing.T) {
			req := httptest.NewRequest("OPTIONS", "/test", nil)
			req.Header.Set("Origin", "http://example.com")
			req.Header.Set("Access-Control-Request-Method", "POST")
			w := httptest.NewRecorder()

			corsHandler.ServeHTTP(w, req)

			// Check CORS headers
			if got := w.Header().Get("Access-Control-Allow-Headers"); got != "Accept, Authorization, Content-Type, Origin, X-Requested-With" {
				t.Errorf("Access-Control-Allow-Headers = %v, want 'Accept, Authorization, Content-Type, Origin, X-Requested-With'", got)
			}
			if got := w.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST, OPTIONS" {
				t.Errorf("Access-Control-Allow-Methods = %v, want 'GET, POST, OPTIONS'", got)
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
				t.Errorf("Access-Control-Allow-Origin = %v, want *", got)
			}

			// Check that OPTIONS request returns 200 OK and doesn't call the handler
			if w.Code != http.StatusOK {
				t.Errorf("Status code = %v, want %v", w.Code, http.StatusOK)
			}
			if body := w.Body.String(); body == "test response" {
				t.Errorf("OPTIONS request should not call the wrapped handler")
			}
		})
	})

	// Test with custom environment variables
	t.Run("Custom CORS configuration", func(t *testing.T) {
		// Set custom environment variables
		os.Setenv("CORS_HEADERS", "Content-Type, X-Custom-Header")
		os.Setenv("CORS_METHODS", "GET, PUT, DELETE")
		os.Setenv("CORS_ORIGIN", "https://example.com")

		// Clean up after test
		defer func() {
			os.Unsetenv("CORS_HEADERS")
			os.Unsetenv("CORS_METHODS")
			os.Unsetenv("CORS_ORIGIN")
		}()

		corsHandler := Middleware(testHandler, os.Getenv("CORS_HEADERS"), os.Getenv("CORS_METHODS"), os.Getenv("CORS_ORIGIN"))

		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("Origin", "https://example.com")
		w := httptest.NewRecorder()

		corsHandler.ServeHTTP(w, req)

		// Check custom CORS headers
		if got := w.Header().Get("Access-Control-Allow-Headers"); got != "Content-Type, X-Custom-Header" {
			t.Errorf("Access-Control-Allow-Headers = %v, want 'Content-Type, X-Custom-Header'", got)
		}
		if got := w.Header().Get("Access-Control-Allow-Methods"); got != "GET, PUT, DELETE" {
			t.Errorf("Access-Control-Allow-Methods = %v, want 'GET, PUT, DELETE'", got)
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
			t.Errorf("Access-Control-Allow-Origin = %v, want https://example.com", got)
		}
	})
}
