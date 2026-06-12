package anthropic

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/core/constants"
)

// TestGetAPIPath tests the PathProvider interface implementation
func TestGetAPIPath(t *testing.T) {
	translator := mustNewTranslator(createTestLogger(), createTestConfig())

	path := translator.GetAPIPath()
	assert.Equal(t, "/olla/anthropic/v1/messages", path, "should return the correct Anthropic API path")
}

// TestWriteError tests the ErrorWriter interface implementation
func TestWriteError(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		statusCode     int
		expectedType   string
		expectedStatus int
	}{
		{
			name:           "bad_request",
			err:            errors.New("invalid model specified"),
			statusCode:     http.StatusBadRequest,
			expectedType:   "invalid_request_error",
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "unauthorized",
			err:            errors.New("missing API key"),
			statusCode:     http.StatusUnauthorized,
			expectedType:   "authentication_error",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "forbidden",
			err:            errors.New("insufficient permissions"),
			statusCode:     http.StatusForbidden,
			expectedType:   "permission_error",
			expectedStatus: http.StatusForbidden,
		},
		{
			name:           "not_found",
			err:            errors.New("model not found"),
			statusCode:     http.StatusNotFound,
			expectedType:   "not_found_error",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "rate_limit",
			err:            errors.New("rate limit exceeded"),
			statusCode:     http.StatusTooManyRequests,
			expectedType:   "rate_limit_error",
			expectedStatus: http.StatusTooManyRequests,
		},
		{
			// 503 from a backend means the backend is unavailable, not that Anthropic's
			// own infra is overloaded. overloaded_error is an Anthropic-origin signal;
			// a generic gateway 503 maps to api_error per litellm/bifrost conventions.
			name:           "service_unavailable",
			err:            errors.New("service overloaded"),
			statusCode:     http.StatusServiceUnavailable,
			expectedType:   "api_error",
			expectedStatus: http.StatusServiceUnavailable,
		},
		{
			name:           "generic_error",
			err:            errors.New("something went wrong"),
			statusCode:     http.StatusInternalServerError,
			expectedType:   "api_error",
			expectedStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			translator := mustNewTranslator(createTestLogger(), createTestConfig())
			rec := httptest.NewRecorder()

			translator.WriteError(rec, tt.err, tt.statusCode)

			// Verify status code
			assert.Equal(t, tt.expectedStatus, rec.Code)

			// Verify content type
			assert.Equal(t, constants.ContentTypeJSON, rec.Header().Get(constants.HeaderContentType))

			// Verify response body structure
			var response map[string]interface{}
			err := json.Unmarshal(rec.Body.Bytes(), &response)
			require.NoError(t, err)

			// Check error type wrapper
			assert.Equal(t, "error", response["type"])

			// Check error details
			errorObj, ok := response["error"].(map[string]interface{})
			require.True(t, ok, "error field should be an object")

			assert.Equal(t, tt.expectedType, errorObj["type"], "error type should match")
			assert.Equal(t, tt.err.Error(), errorObj["message"], "error message should match")
		})
	}
}

// TestWriteError_ErrorFormat tests Anthropic error format compliance
func TestWriteError_ErrorFormat(t *testing.T) {
	translator := mustNewTranslator(createTestLogger(), createTestConfig())
	rec := httptest.NewRecorder()

	testErr := errors.New("test error message")
	translator.WriteError(rec, testErr, http.StatusBadRequest)

	var response map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)

	// Verify Anthropic error format structure
	// According to https://docs.anthropic.com/claude/reference/errors
	assert.Contains(t, response, "type", "response should have type field")
	assert.Contains(t, response, "error", "response should have error field")

	errorObj := response["error"].(map[string]interface{})
	assert.Contains(t, errorObj, "type", "error object should have type field")
	assert.Contains(t, errorObj, "message", "error object should have message field")
}

// TestName tests the Name method
func TestName(t *testing.T) {
	translator := mustNewTranslator(createTestLogger(), createTestConfig())
	assert.Equal(t, "anthropic", translator.Name())
}

// TestNewTranslator_Success verifies the happy path returns a non-nil translator
// and no error, confirming the error-return refactor did not break normal construction.
func TestNewTranslator_Success(t *testing.T) {
	t.Parallel()

	tr, err := NewTranslator(createTestLogger(), createTestConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr == nil {
		t.Fatal("expected non-nil translator")
	}
}

// TestWriteError_JSONEncodingFailure tests handling of JSON encoding errors
// This test is mostly for coverage, as encoding errors are rare
func TestWriteError_JSONEncodingSuccess(t *testing.T) {
	translator := mustNewTranslator(createTestLogger(), createTestConfig())
	rec := httptest.NewRecorder()

	// Standard error that should encode successfully
	translator.WriteError(rec, errors.New("test"), http.StatusBadRequest)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.NotEmpty(t, rec.Body.Bytes(), "response body should not be empty")

	// Verify it's valid JSON
	var response map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &response)
	assert.NoError(t, err, "response should be valid JSON")
}

// TestWriteError_ConformanceMapping verifies Anthropic-conformant error type mappings.
//
// Key correctness constraints:
//   - 503 must map to api_error, NOT overloaded_error. overloaded_error is an Anthropic-origin
//     signal (litellm maps it only for responses from Anthropic's own infrastructure). A backend
//     503 means the upstream gateway is unavailable — that is api_error territory.
//   - 504 and 408 must map to timeout_error per litellm's error taxonomy.
//   - Response body must be the Anthropic error envelope: {"type":"error","error":{"type":"...","message":"..."}}.
func TestWriteError_ConformanceMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status       int
		expectedType string
	}{
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusUnauthorized, "authentication_error"},
		{http.StatusForbidden, "permission_error"},
		{http.StatusNotFound, "not_found_error"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusInternalServerError, "api_error"},
		{http.StatusBadGateway, "api_error"},
		// 503 from a backend is NOT Anthropic-origin overloaded; it is api_error.
		{http.StatusServiceUnavailable, "api_error"},
		{http.StatusGatewayTimeout, "timeout_error"},
		{http.StatusRequestTimeout, "timeout_error"},
	}

	for _, tc := range cases {

		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()

			trans := mustNewTranslator(createTestLogger(), createTestConfig())
			rec := httptest.NewRecorder()
			trans.WriteError(rec, errors.New("test error"), tc.status)

			require.Equal(t, tc.status, rec.Code)
			require.Equal(t, constants.ContentTypeJSON, rec.Header().Get(constants.HeaderContentType))

			var body map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

			// Anthropic error envelope: {"type":"error","error":{...}}
			assert.Equal(t, "error", body["type"], "outer type must be 'error'")

			errObj, ok := body["error"].(map[string]interface{})
			require.True(t, ok, "body.error must be an object")
			assert.Equal(t, tc.expectedType, errObj["type"],
				"status %d should map to %s", tc.status, tc.expectedType)
			assert.NotEmpty(t, errObj["message"], "error.message must not be empty")
		})
	}
}
