package inspector

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/logger"
)

func newMultipartBody(t *testing.T, fields map[string]string, filePartName string, fileData []byte) (io.Reader, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	if name, ok := fields["model"]; ok {
		require.NoError(t, w.WriteField("model", name))
	}
	if filePartName != "" {
		part, err := w.CreateFormFile(filePartName, "audio.wav")
		require.NoError(t, err)
		_, err = part.Write(fileData)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf, w.FormDataContentType()
}

func newBodyInspectorForMultipart(t *testing.T) *BodyInspector {
	t.Helper()
	logCfg := &logger.Config{Level: "debug", PrettyLogs: false}
	log, _, err := logger.New(logCfg)
	require.NoError(t, err)
	styledLog := &mockStyledLogger{underlying: log}
	bi, err := NewBodyInspector(styledLog)
	require.NoError(t, err)
	return bi
}

func TestBodyInspector_Multipart_ExtractsModel(t *testing.T) {
	ctx := context.Background()
	bi := newBodyInspectorForMultipart(t)

	audio := bytes.Repeat([]byte("RIFFwave"), 1024)
	body, ct := newMultipartBody(t, map[string]string{"model": "whisper-small"}, "file", audio)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/audio/transcriptions", body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", ct)
	req.ContentLength = int64(len(body.(*bytes.Buffer).Bytes()))

	profile := &domain.RequestProfile{}
	require.NoError(t, bi.Inspect(ctx, req, profile))
	assert.Equal(t, "whisper-small", profile.ModelName)

	// The body must reach the downstream proxy byte-identical.
	restored, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.True(t, bytes.Contains(restored, audio), "audio bytes survive the inspection")
	assert.Contains(t, string(restored), "whisper-small", "model field survives in the restored body")
}

func TestBodyInspector_Multipart_ModelAfterLargeFile(t *testing.T) {
	ctx := context.Background()
	bi := newBodyInspectorForMultipart(t)

	// OpenAI SDKs place the file first: the "model" field follows a 3 MB part.
	audio := bytes.Repeat([]byte("RIFFwave"), 384*1024)
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	part, err := w.CreateFormFile("file", "audio.wav")
	require.NoError(t, err)
	_, err = part.Write(audio)
	require.NoError(t, err)
	require.NoError(t, w.WriteField("model", "whisper-small"))
	require.NoError(t, w.Close())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/audio/transcriptions", buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.ContentLength = int64(buf.Len())

	profile := &domain.RequestProfile{}
	require.NoError(t, bi.Inspect(ctx, req, profile))
	assert.Equal(t, "whisper-small", profile.ModelName)

	restored, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Contains(t, string(restored), "whisper-small", "model field survives in the restored body")
}

func TestBodyInspector_Multipart_NoModelField(t *testing.T) {
	ctx := context.Background()
	bi := newBodyInspectorForMultipart(t)

	body, ct := newMultipartBody(t, map[string]string{}, "file", []byte("RIFF"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/audio/transcriptions", body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", ct)

	profile := &domain.RequestProfile{}
	require.NoError(t, bi.Inspect(ctx, req, profile))
	assert.Empty(t, profile.ModelName, "no model field means default routing, not a guess")
}

func TestBodyInspector_Multipart_LargeBodySkipped(t *testing.T) {
	ctx := context.Background()
	bi := newBodyInspectorForMultipart(t)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/audio/transcriptions", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.ContentLength = MultipartModelScanSize + 1

	profile := &domain.RequestProfile{}
	require.NoError(t, bi.Inspect(ctx, req, profile))
	assert.Empty(t, profile.ModelName, "beyond the scan ceiling the body is left untouched")
}

var _ = slog.LevelDebug
