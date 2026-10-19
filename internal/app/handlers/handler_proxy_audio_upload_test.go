package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/adapter/inspector"
	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/constants"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/core/ports"
	"github.com/thushan/olla/internal/logger"
)

// W4 (PLAN-VISION-AUDIO-2026-10-19): multipart bodies above the inspector's
// multipart model-scan ceiling are skipped by the inspector, so an audio
// transcription upload in the 8-16 MB band (max_body_size is 16 MiB) used to
// take the default routing chain SILENTLY - the proven class where a >8 MB
// upload lands on a text-only endpoint and 501s. rejectOversizeAudioUpload
// turns that silent misroute into a loud 413 on the transcription route only.

const audioUploadBoundary = "olla-audio-test-boundary"

// multipartAudioBody returns a small syntactically valid multipart body with
// a "model" field. The guard decides on r.ContentLength (what the inspector's
// ceiling compares against), so the body stays tiny; the inspector's scan of
// the "passes" cases parses this body for real.
func multipartAudioBody() string {
	return "--" + audioUploadBoundary + "\r\n" +
		"Content-Disposition: form-data; name=\"model\"\r\n\r\n" +
		"whisper-small\r\n" +
		"--" + audioUploadBoundary + "--\r\n"
}

// warnSpyLogger records Warn calls. mockStyledLogger.WithRequestID returns the
// embedded struct, which would drop the spy from pr.requestLogger, so the
// method is overridden to keep the chain on the spy.
type warnSpyLogger struct {
	*mockStyledLogger
	mu    sync.Mutex
	warns []string
	args  [][]any
}

func (m *warnSpyLogger) Warn(msg string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.warns = append(m.warns, msg)
	m.args = append(m.args, args)
}

func (m *warnSpyLogger) WithRequestID(requestID string) logger.StyledLogger { return m }

func (m *warnSpyLogger) findWarn(substr string) ([]any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, w := range m.warns {
		if strings.Contains(w, substr) {
			return m.args[i], true
		}
	}
	return nil, false
}

func newAudioUploadApp(t *testing.T, spy *warnSpyLogger) (*Application, *bool) {
	t.Helper()
	proxyCalled := new(bool)
	app := &Application{
		logger: spy,
		proxyService: &mockProxyService{
			proxyFunc: func(ctx context.Context, w http.ResponseWriter, r *http.Request, endpoints []*domain.Endpoint, stats *ports.RequestStats, l logger.StyledLogger) error {
				*proxyCalled = true
				w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
				w.WriteHeader(http.StatusOK)
				_, err := w.Write([]byte(`{"ok":true}`))
				return err
			},
		},
		discoveryService: &mockDiscoveryServiceForTranslation{},
		inspectorChain:   inspector.NewChain(spy),
		profileFactory:   &mockProfileFactory{},
		Config: &config.Config{
			Server: config.ServerConfig{
				RateLimits: config.ServerRateLimits{},
			},
		},
		StartTime: time.Now(),
	}
	return app, proxyCalled
}

func TestProxyRejectsOversizeAudioMultipartUpload(t *testing.T) {
	t.Parallel()

	const ceiling = inspector.MultipartModelScanSize

	tests := []struct {
		name          string
		requestPath   string
		contentType   string
		contentLength int64
		wantStatus    int
		wantProxy     bool
	}{
		{
			name:          "audio_multipart_one_byte_over_ceiling_is_413",
			requestPath:   "/olla/proxy/v1/audio/transcriptions",
			contentType:   "multipart/form-data; boundary=" + audioUploadBoundary,
			contentLength: ceiling + 1,
			wantStatus:    http.StatusRequestEntityTooLarge,
			wantProxy:     false,
		},
		{
			name:          "audio_multipart_exactly_at_ceiling_passes",
			requestPath:   "/olla/proxy/v1/audio/transcriptions",
			contentType:   "multipart/form-data; boundary=" + audioUploadBoundary,
			contentLength: ceiling,
			wantStatus:    http.StatusOK,
			wantProxy:     true,
		},
		{
			name:          "non_audio_multipart_over_ceiling_passes",
			requestPath:   "/olla/proxy/v1/chat/completions",
			contentType:   "multipart/form-data; boundary=" + audioUploadBoundary,
			contentLength: ceiling + 1,
			wantStatus:    http.StatusOK,
			wantProxy:     true,
		},
		{
			name:          "audio_multipart_chunked_unknown_length_passes",
			requestPath:   "/olla/proxy/v1/audio/transcriptions",
			contentType:   "multipart/form-data; boundary=" + audioUploadBoundary,
			contentLength: -1,
			wantStatus:    http.StatusOK,
			wantProxy:     true,
		},
		{
			name:          "audio_speech_multipart_over_ceiling_passes",
			requestPath:   "/olla/proxy/v1/audio/speech",
			contentType:   "multipart/form-data; boundary=" + audioUploadBoundary,
			contentLength: ceiling + 1,
			wantStatus:    http.StatusOK,
			wantProxy:     true,
		},
		{
			name:          "audio_transcriptions_json_body_over_ceiling_passes",
			requestPath:   "/olla/proxy/v1/audio/transcriptions",
			contentType:   constants.ContentTypeJSON,
			contentLength: ceiling + 1,
			wantStatus:    http.StatusOK,
			wantProxy:     true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spy := &warnSpyLogger{mockStyledLogger: &mockStyledLogger{}}
			app, proxyCalled := newAudioUploadApp(t, spy)

			req := httptest.NewRequest(http.MethodPost, tt.requestPath, strings.NewReader(multipartAudioBody()))
			req.Header.Set(constants.HeaderContentType, tt.contentType)
			req.ContentLength = tt.contentLength
			ctx := context.WithValue(req.Context(), constants.ContextRoutePrefixKey, "/olla/proxy/")
			req = req.WithContext(ctx)

			rec := httptest.NewRecorder()
			app.proxyHandler(rec, req)

			require.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, tt.wantProxy, *proxyCalled, "proxy service invocation")

			if tt.wantStatus != http.StatusRequestEntityTooLarge {
				_, warned := spy.findWarn("Rejecting multipart audio upload")
				assert.False(t, warned, "non-rejected requests must not log the audio-upload warning")
				return
			}

			// Rejected: JSON error body matching the translator error shape.
			assert.Equal(t, constants.ContentTypeJSON, rec.Header().Get(constants.HeaderContentType))
			var body map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			errObj, ok := body["error"].(map[string]interface{})
			require.True(t, ok, "error envelope: %s", rec.Body.String())
			assert.Equal(t, "invalid_request_error", errObj["type"])
			msg, _ := errObj["message"].(string)
			assert.Contains(t, msg, "model-scan ceiling")

			// Loud, not silent: WARN with path + content_length.
			args, warned := spy.findWarn("Rejecting multipart audio upload above the model-scan ceiling")
			require.True(t, warned, "expected a WARN rejection line")
			kv := map[string]any{}
			for i := 0; i+1 < len(args); i += 2 {
				if k, ok := args[i].(string); ok {
					kv[k] = args[i+1]
				}
			}
			assert.Equal(t, "/v1/audio/transcriptions", kv["path"])
			assert.Equal(t, int64(ceiling+1), kv["content_length"])
		})
	}
}

func TestProviderProxyRejectsOversizeAudioMultipartUpload(t *testing.T) {
	t.Parallel()

	spy := &warnSpyLogger{mockStyledLogger: &mockStyledLogger{}}
	app, proxyCalled := newAudioUploadApp(t, spy)

	app.profileFactory = &mockProfileFactory{validProfiles: map[string]bool{"openai": true}}

	req := httptest.NewRequest(http.MethodPost, "/olla/openai/v1/audio/transcriptions", strings.NewReader(multipartAudioBody()))
	req.Header.Set(constants.HeaderContentType, "multipart/form-data; boundary="+audioUploadBoundary)
	req.ContentLength = inspector.MultipartModelScanSize + 1

	rec := httptest.NewRecorder()
	app.providerProxyHandler(rec, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.False(t, *proxyCalled, "rejected upload must not reach the proxy engine")

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj, ok := body["error"].(map[string]interface{})
	require.True(t, ok, "error envelope: %s", rec.Body.String())
	assert.Equal(t, "invalid_request_error", errObj["type"])
}
