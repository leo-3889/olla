package security

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/ports"
	"github.com/thushan/olla/internal/logger"
)

func createTestSizeLogger() logger.StyledLogger {
	loggerCfg := &logger.Config{Level: "error", Theme: "default"}
	log, _, _ := logger.New(loggerCfg)
	return logger.NewPlainStyledLogger(log)
}
func createTestSizeLimitValidator(limits config.ServerRequestLimits) *SizeValidator {
	logger := createTestRateLimitLogger()
	statsCollector := createTestStatsCollector(logger)
	metricsAdapter := NewSecurityMetricsAdapter(statsCollector, logger)

	return NewSizeValidator(limits, metricsAdapter, logger)
}
func TestNewSizeValidator(t *testing.T) {
	limits := config.ServerRequestLimits{
		MaxBodySize:   1024,
		MaxHeaderSize: 512,
	}

	validator := createTestSizeLimitValidator(limits)

	if validator.Name() != "size_limit" {
		t.Errorf("Expected name 'size_limit', got %q", validator.Name())
	}
	if validator.maxBodySize != 1024 {
		t.Errorf("Expected max body size 1024, got %d", validator.maxBodySize)
	}
	if validator.maxHeaderSize != 512 {
		t.Errorf("Expected max header size 512, got %d", validator.maxHeaderSize)
	}
}

func TestSizeValidator_Validate_SmallRequest(t *testing.T) {
	limits := config.ServerRequestLimits{
		MaxBodySize:   1024,
		MaxHeaderSize: 512,
	}

	validator := createTestSizeLimitValidator(limits)
	ctx := context.Background()

	req := ports.SecurityRequest{
		ClientID:   "192.168.1.100",
		Endpoint:   "/api/test",
		Method:     "POST",
		BodySize:   100,
		HeaderSize: 200,
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
			"User-Agent":   {"TestAgent/1.0"},
		},
	}

	result, err := validator.Validate(ctx, req)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if !result.Allowed {
		t.Errorf("Small request should be allowed, got: %s", result.Reason)
	}
}

func TestSizeValidator_Validate_BodyTooLarge(t *testing.T) {
	limits := config.ServerRequestLimits{
		MaxBodySize:   100,
		MaxHeaderSize: 1024,
	}

	validator := createTestSizeLimitValidator(limits)
	ctx := context.Background()

	req := ports.SecurityRequest{
		ClientID:   "192.168.1.100",
		Endpoint:   "/api/test",
		Method:     "POST",
		BodySize:   200,
		HeaderSize: 50,
		Headers:    map[string][]string{},
	}

	result, err := validator.Validate(ctx, req)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if result.Allowed {
		t.Error("Large body should be rejected")
	}
	if !strings.Contains(result.Reason, "Request body too large") {
		t.Errorf("Expected body size error, got: %s", result.Reason)
	}
}

func TestSizeValidator_Validate_HeadersTooLarge(t *testing.T) {
	limits := config.ServerRequestLimits{
		MaxBodySize:   1024,
		MaxHeaderSize: 100,
	}

	validator := createTestSizeLimitValidator(limits)
	ctx := context.Background()

	req := ports.SecurityRequest{
		ClientID:   "192.168.1.100",
		Endpoint:   "/api/test",
		Method:     "POST",
		BodySize:   50,
		HeaderSize: 200,
		Headers: map[string][]string{
			"X-Large-Header": {strings.Repeat("x", 200)},
		},
	}

	result, err := validator.Validate(ctx, req)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if result.Allowed {
		t.Error("Large headers should be rejected")
	}
	if !strings.Contains(result.Reason, "Request headers too large") {
		t.Errorf("Expected header size error, got: %s", result.Reason)
	}
}

func TestSizeValidator_Validate_ZeroLimitsDisabled(t *testing.T) {
	limits := config.ServerRequestLimits{
		MaxBodySize:   0,
		MaxHeaderSize: 0,
	}

	validator := createTestSizeLimitValidator(limits)
	ctx := context.Background()

	req := ports.SecurityRequest{
		ClientID:   "192.168.1.100",
		Endpoint:   "/api/test",
		Method:     "POST",
		BodySize:   10000,
		HeaderSize: 5000,
		Headers: map[string][]string{
			"X-Huge-Header": {strings.Repeat("z", 5000)},
		},
	}

	result, err := validator.Validate(ctx, req)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if !result.Allowed {
		t.Errorf("Request should be allowed when limits are disabled, got: %s", result.Reason)
	}
}

func TestSizeValidator_Validate_NegativeLimitsDisabled(t *testing.T) {
	limits := config.ServerRequestLimits{
		MaxBodySize:   -1,
		MaxHeaderSize: -100,
	}

	validator := createTestSizeLimitValidator(limits)
	ctx := context.Background()

	req := ports.SecurityRequest{
		ClientID:   "192.168.1.100",
		Endpoint:   "/api/test",
		Method:     "POST",
		BodySize:   5000,
		HeaderSize: 2000,
		Headers: map[string][]string{
			"X-Large-Header": {strings.Repeat("z", 2000)},
		},
	}

	result, err := validator.Validate(ctx, req)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if !result.Allowed {
		t.Errorf("Request should be allowed when limits are negative (disabled), got: %s", result.Reason)
	}
}

func TestSizeValidator_Validate_ExactLimit(t *testing.T) {
	limits := config.ServerRequestLimits{
		MaxBodySize:   100,
		MaxHeaderSize: 200,
	}

	validator := createTestSizeLimitValidator(limits)
	ctx := context.Background()

	req := ports.SecurityRequest{
		ClientID:   "192.168.1.100",
		Endpoint:   "/api/test",
		Method:     "POST",
		BodySize:   100,
		HeaderSize: 150,
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
		},
	}

	result, err := validator.Validate(ctx, req)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if !result.Allowed {
		t.Errorf("Request at exact limit should be allowed, got: %s", result.Reason)
	}
}

func TestSizeValidator_Validate_OneByteTooLarge(t *testing.T) {
	limits := config.ServerRequestLimits{
		MaxBodySize:   100,
		MaxHeaderSize: 200,
	}

	validator := createTestSizeLimitValidator(limits)
	ctx := context.Background()

	req := ports.SecurityRequest{
		ClientID:   "192.168.1.100",
		Endpoint:   "/api/test",
		Method:     "POST",
		BodySize:   101,
		HeaderSize: 150,
		Headers:    map[string][]string{},
	}

	result, err := validator.Validate(ctx, req)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if result.Allowed {
		t.Error("Request one byte over limit should be rejected")
	}
}

func TestSizeValidator_Validate_EmptyBody(t *testing.T) {
	limits := config.ServerRequestLimits{
		MaxBodySize:   100,
		MaxHeaderSize: 200,
	}

	validator := createTestSizeLimitValidator(limits)
	ctx := context.Background()

	req := ports.SecurityRequest{
		ClientID:   "192.168.1.100",
		Endpoint:   "/api/test",
		Method:     "GET",
		BodySize:   0,
		HeaderSize: 50,
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
		},
	}

	result, err := validator.Validate(ctx, req)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if !result.Allowed {
		t.Errorf("Empty body should be allowed, got: %s", result.Reason)
	}
}

func TestSizeValidator_Validate_MultipleHeaders(t *testing.T) {
	limits := config.ServerRequestLimits{
		MaxBodySize:   1024,
		MaxHeaderSize: 200,
	}

	validator := createTestSizeLimitValidator(limits)
	ctx := context.Background()

	req := ports.SecurityRequest{
		ClientID:   "192.168.1.100",
		Endpoint:   "/api/test",
		Method:     "POST",
		BodySize:   100,
		HeaderSize: 300,
		Headers: map[string][]string{
			"Content-Type":    {"application/json"},
			"Authorization":   {"Bearer " + strings.Repeat("x", 50)},
			"X-Custom-Header": {strings.Repeat("y", 50)},
			"User-Agent":      {"TestAgent/1.0"},
		},
	}

	result, err := validator.Validate(ctx, req)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if result.Allowed {
		t.Error("Large multiple headers should be rejected")
	}
}

func TestSizeValidator_Validate_ConcurrentRequests(t *testing.T) {
	limits := config.ServerRequestLimits{
		MaxBodySize:   1000,
		MaxHeaderSize: 500,
	}

	validator := createTestSizeLimitValidator(limits)
	ctx := context.Background()

	var wg sync.WaitGroup
	errors := make(chan error, 20)
	results := make(chan bool, 20)

	for i := range 10 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			var bodySize int64
			if id%2 == 0 {
				bodySize = 500
			} else {
				bodySize = 1500
			}

			req := ports.SecurityRequest{
				ClientID:   "192.168.1.100",
				Endpoint:   "/api/test",
				Method:     "POST",
				BodySize:   bodySize,
				HeaderSize: 100,
				Headers:    map[string][]string{"Content-Type": {"application/json"}},
			}

			result, err := validator.Validate(ctx, req)
			if err != nil {
				errors <- err
				return
			}
			results <- result.Allowed
		}(i)
	}

	wg.Wait()
	close(errors)
	close(results)

	for err := range errors {
		t.Errorf("Concurrent access error: %v", err)
	}

	allowedCount := 0
	rejectedCount := 0
	for allowed := range results {
		if allowed {
			allowedCount++
		} else {
			rejectedCount++
		}
	}

	if allowedCount != 5 {
		t.Errorf("Expected 5 allowed requests, got %d", allowedCount)
	}
	if rejectedCount != 5 {
		t.Errorf("Expected 5 rejected requests, got %d", rejectedCount)
	}
}

func TestSizeValidator_Validate_MultiValueHeaders(t *testing.T) {
	limits := config.ServerRequestLimits{
		MaxBodySize:   1024,
		MaxHeaderSize: 150,
	}

	validator := createTestSizeLimitValidator(limits)
	ctx := context.Background()

	req := ports.SecurityRequest{
		ClientID:   "192.168.1.100",
		Endpoint:   "/api/test",
		Method:     "POST",
		BodySize:   100,
		HeaderSize: 200,
		Headers: map[string][]string{
			"Accept": {"application/json", "text/html", "application/xml"},
		},
	}

	result, err := validator.Validate(ctx, req)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if !result.Allowed {
		t.Errorf("Multi-value headers should be calculated correctly, got: %s", result.Reason)
	}

	req.Headers["Large-Header"] = []string{strings.Repeat("z", 100)}
	req.HeaderSize = 350

	result, err = validator.Validate(ctx, req)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if result.Allowed {
		t.Error("Large multi-value headers should exceed limit")
	}
}

// TestCreateNonProxyMiddleware_ChunkedOversizedBody confirms that an oversized
// chunked request (Content-Length == -1) to a non-proxy route is rejected with
// 413. This is the regression guard for P1: MaxBytesReader alone never fires on
// routes that don't read the body, so CreateNonProxyMiddleware must drain the
// body itself.
func TestCreateNonProxyMiddleware_ChunkedOversizedBody(t *testing.T) {
	t.Parallel()

	const cap = 10

	validator := createTestSizeLimitValidator(config.ServerRequestLimits{
		MaxBodySize:   cap,
		MaxHeaderSize: 0,
	})

	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	handler := validator.CreateNonProxyMiddleware()(next)

	body := strings.Repeat("x", cap+1)
	req := httptest.NewRequest(http.MethodPost, "/internal/status", strings.NewReader(body))
	// Simulate chunked: no Content-Length header
	req.ContentLength = -1

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversized chunked body, got %d", rr.Code)
	}
	if reached {
		t.Error("handler must not be reached when oversized chunked body is rejected")
	}
}

// TestCreateNonProxyMiddleware_ChunkedSmallBody confirms that a small chunked
// body passes through and the handler still receives the body content intact.
func TestCreateNonProxyMiddleware_ChunkedSmallBody(t *testing.T) {
	t.Parallel()

	const cap = 100

	validator := createTestSizeLimitValidator(config.ServerRequestLimits{
		MaxBodySize:   cap,
		MaxHeaderSize: 0,
	})

	var receivedBody string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.WriteHeader(http.StatusOK)
	})

	handler := validator.CreateNonProxyMiddleware()(next)

	body := "hello"
	req := httptest.NewRequest(http.MethodPost, "/internal/status", strings.NewReader(body))
	req.ContentLength = -1

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for small chunked body, got %d", rr.Code)
	}
	if receivedBody != body {
		t.Errorf("handler received wrong body: want %q, got %q", body, receivedBody)
	}
}

// TestCreateNonProxyMiddleware_KnownLengthOversized confirms that the fast-path
// Content-Length rejection still works in CreateNonProxyMiddleware.
func TestCreateNonProxyMiddleware_KnownLengthOversized(t *testing.T) {
	t.Parallel()

	const cap = 10

	validator := createTestSizeLimitValidator(config.ServerRequestLimits{
		MaxBodySize:   cap,
		MaxHeaderSize: 0,
	})

	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	handler := validator.CreateNonProxyMiddleware()(next)

	body := strings.Repeat("x", cap+1)
	req := httptest.NewRequest(http.MethodPost, "/internal/status", strings.NewReader(body))
	req.ContentLength = int64(len(body))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for known-length oversized body, got %d", rr.Code)
	}
	if reached {
		t.Error("handler must not be reached when oversized known-length body is rejected")
	}
}
