package health

import (
	"net/http"
	"testing"
	"time"
)

func TestIsJSONRPCRateLimitCode(t *testing.T) {
	tests := []struct {
		name     string
		code     int
		expected bool
	}{
		{"rate limit code -32005", -32005, true},
		{"method not found code", -32601, false},
		{"generic error code", -32000, false},
		{"zero code", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsJSONRPCRateLimitCode(tt.code); got != tt.expected {
				t.Errorf("IsJSONRPCRateLimitCode(%d) = %v, want %v", tt.code, got, tt.expected)
			}
		})
	}
}

func TestDetectRateLimit(t *testing.T) {
	rateLimitedRPCResp := &RpcResponse{Error: &struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{Code: -32005, Message: "Request limit exceeded"}}

	methodNotFoundRPCResp := &RpcResponse{Error: &struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{Code: -32601, Message: "Method not found"}}

	tests := []struct {
		name             string
		provider         string
		statusCode       int
		headers          http.Header
		rpcResp          *RpcResponse
		wantRateLimited  bool
		wantDailyQuota   bool
		wantRetryAfterEq time.Duration
	}{
		{
			name:            "429 for any provider is rate limited",
			provider:        "alchemy",
			statusCode:      429,
			wantRateLimited: true,
		},
		{
			name:            "402 for infura is rate limited and daily quota",
			provider:        "infura",
			statusCode:      402,
			wantRateLimited: true,
			wantDailyQuota:  true,
		},
		{
			name:            "402 for other provider is not rate limited",
			provider:        "alchemy",
			statusCode:      402,
			wantRateLimited: false,
		},
		{
			name:            "402 for infura with different case still matches",
			provider:        "Infura",
			statusCode:      402,
			wantRateLimited: true,
			wantDailyQuota:  true,
		},
		{
			name:            "200 with -32005 JSON-RPC error is rate limited",
			provider:        "alchemy",
			statusCode:      200,
			rpcResp:         rateLimitedRPCResp,
			wantRateLimited: true,
		},
		{
			name:            "200 with unrelated JSON-RPC error is not rate limited",
			provider:        "alchemy",
			statusCode:      200,
			rpcResp:         methodNotFoundRPCResp,
			wantRateLimited: false,
		},
		{
			name:            "200 with no error is not rate limited",
			provider:        "alchemy",
			statusCode:      200,
			wantRateLimited: false,
		},
		{
			name:             "429 with Retry-After as integer seconds",
			provider:         "alchemy",
			statusCode:       429,
			headers:          http.Header{"Retry-After": []string{"5"}},
			wantRateLimited:  true,
			wantRetryAfterEq: 5 * time.Second,
		},
		{
			name:             "429 with missing Retry-After",
			provider:         "alchemy",
			statusCode:       429,
			headers:          http.Header{},
			wantRateLimited:  true,
			wantRetryAfterEq: 0,
		},
		{
			name:             "429 with garbage Retry-After",
			provider:         "alchemy",
			statusCode:       429,
			headers:          http.Header{"Retry-After": []string{"not-a-number"}},
			wantRateLimited:  true,
			wantRetryAfterEq: 0,
		},
		{
			name:             "429 with negative Retry-After is ignored",
			provider:         "alchemy",
			statusCode:       429,
			headers:          http.Header{"Retry-After": []string{"-5"}},
			wantRateLimited:  true,
			wantRetryAfterEq: 0,
		},
		{
			name:             "200 success does not surface a stray Retry-After header",
			provider:         "alchemy",
			statusCode:       200,
			headers:          http.Header{"Retry-After": []string{"30"}},
			wantRateLimited:  false,
			wantRetryAfterEq: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig := DetectRateLimit(tt.provider, tt.statusCode, tt.headers, tt.rpcResp)
			if sig.IsRateLimited != tt.wantRateLimited {
				t.Errorf("IsRateLimited = %v, want %v", sig.IsRateLimited, tt.wantRateLimited)
			}
			if sig.IsDailyQuota != tt.wantDailyQuota {
				t.Errorf("IsDailyQuota = %v, want %v", sig.IsDailyQuota, tt.wantDailyQuota)
			}
			if sig.RetryAfter != tt.wantRetryAfterEq {
				t.Errorf("RetryAfter = %v, want %v", sig.RetryAfter, tt.wantRetryAfterEq)
			}
		})
	}

	t.Run("429 with Retry-After as HTTP-date", func(t *testing.T) {
		future := time.Now().Add(10 * time.Second).UTC().Truncate(time.Second)
		headers := http.Header{"Retry-After": []string{future.Format(http.TimeFormat)}}
		sig := DetectRateLimit("alchemy", 429, headers, nil)
		if !sig.IsRateLimited {
			t.Fatal("expected IsRateLimited to be true")
		}
		// Allow a small delta since time.Until(future) is computed after truncation to the second.
		if sig.RetryAfter <= 0 || sig.RetryAfter > 11*time.Second {
			t.Errorf("RetryAfter = %v, want roughly 10s", sig.RetryAfter)
		}
	})
}
