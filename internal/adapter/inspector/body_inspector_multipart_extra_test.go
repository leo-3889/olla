// Extra table tests for the multipart model-scan path (inspectMultipart in
// body_inspector.go): field-limit 256, MultipartModelScanSize ceiling,
// quoted-boundary parsing, malformed bodies, and byte-identical body
// restoration on every path. Reuses newBodyInspectorForMultipart from
// body_inspector_multipart_test.go (same package).
package inspector

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thushan/olla/internal/core/domain"
)

const (
	// Safe model name under the assumed normalizeModelName semantics.
	scanTestModelName = "llava-next"
	// 4 KiB fixture required by the explicit byte-identical restore case.
	scanTestFileBytes = 4 << 10
)

// inspectRequest drives the real inspection entry: Inspect mutates the
// request body in place (restoration) and fills the caller's profile.
func inspectRequest(t *testing.T, r *http.Request) (*http.Request, *domain.RequestProfile) {
	t.Helper()
	bi := newBodyInspectorForMultipart(t)
	profile := &domain.RequestProfile{}
	require.NoError(t, bi.Inspect(context.Background(), r, profile))
	return r, profile
}

// scanPartSpec describes one part of a multipart/form-data body. When
// filename is non-empty the part is created as a file part of fileSize
// bytes; otherwise it is a plain form field carrying value.
type scanPartSpec struct {
	name        string
	value       string
	filename    string
	fileSize    int
	contentType string
}

// buildMultipartBody assembles a multipart/form-data body with the given
// parts in order and returns the raw bytes, the generated Content-Type
// header value, and the boundary.
func buildMultipartBody(t *testing.T, specs []scanPartSpec) (body []byte, contentType string, boundary string) {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, s := range specs {
		if s.filename != "" {
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, s.name, s.filename))
			ct := s.contentType
			if ct == "" {
				ct = "application/octet-stream"
			}
			h.Set("Content-Type", ct)
			part, err := w.CreatePart(h)
			if err != nil {
				t.Fatalf("CreatePart(%s): %v", s.name, err)
			}
			if _, err = part.Write(bytes.Repeat([]byte("x"), s.fileSize)); err != nil {
				t.Fatalf("write file part %s: %v", s.name, err)
			}
			continue
		}
		field, err := w.CreateFormField(s.name)
		if err != nil {
			t.Fatalf("CreateFormField(%s): %v", s.name, err)
		}
		if _, err = field.Write([]byte(s.value)); err != nil {
			t.Fatalf("write field %s: %v", s.name, err)
		}
	}
	boundary = w.Boundary()
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType(), boundary
}

// newScanRequest builds an *http.Request directly (no httptest) with the
// given Content-Type and body bytes.
func newScanRequest(contentType string, body []byte) *http.Request {
	r, err := http.NewRequest(http.MethodPost, "/api/generate", bytes.NewReader(body))
	if err != nil {
		panic("invalid test request: " + err.Error())
	}
	r.Header.Set("Content-Type", contentType)
	return r
}

func TestInspectRequestMultipartModelScanExtraCases(t *testing.T) {
	ok256 := "q" + strings.Repeat("9", multipartModelFieldLimit-1)  // exactly 256 bytes
	oversize := "q" + strings.Repeat("9", multipartModelFieldLimit) // 257 bytes -> rejected

	tests := []struct {
		name string
		// specs build a multipart body via multipart.Writer when rawBody is nil.
		specs []scanPartSpec
		// rawBody replaces the built body entirely (non-multipart fixtures).
		rawBody []byte
		// contentType overrides the content type when rawBody is set.
		contentType string
		// quoteBoundary rewrites the generated Content-Type to the quoted
		// boundary form: multipart/form-data; boundary="<b>".
		quoteBoundary bool
		// wantModel is the expected profile.ModelName ("" = left unset).
		wantModel string
	}{
		{
			name:      "model field present before other fields",
			specs:     []scanPartSpec{{name: "model", value: scanTestModelName}, {name: "prompt", value: "describe the image"}},
			wantModel: scanTestModelName,
		},
		{
			name:  "no model field",
			specs: []scanPartSpec{{name: "prompt", value: "hello"}, {name: "stream", value: "false"}},
		},
		{
			name: "model field after multi-KB file part",
			specs: []scanPartSpec{
				{name: "file", filename: "chart.png", fileSize: 64 << 10, contentType: "image/png"},
				{name: "model", value: scanTestModelName},
			},
			wantModel: scanTestModelName,
		},
		{
			name: "oversized model field leaves model unset",
			specs: []scanPartSpec{
				{name: "model", value: oversize},
				{name: "prompt", value: "hello"},
			},
		},
		{
			name:  "empty model field leaves model unset",
			specs: []scanPartSpec{{name: "model", value: ""}, {name: "prompt", value: "hello"}},
		},
		{
			name:  "whitespace-only model field leaves model unset",
			specs: []scanPartSpec{{name: "model", value: " \t "}, {name: "prompt", value: "hello"}},
		},
		{
			name:      "model field exactly at field limit is accepted",
			specs:     []scanPartSpec{{name: "model", value: ok256}},
			wantModel: ok256,
		},
		{
			name: "4KiB file part plus model field restores body byte-identical",
			specs: []scanPartSpec{
				{name: "file", filename: "photo.jpg", fileSize: scanTestFileBytes, contentType: "image/jpeg"},
				{name: "model", value: scanTestModelName},
				{name: "prompt", value: "what is in this image"},
			},
			wantModel: scanTestModelName,
		},
		{
			name:        "non-multipart content type untouched with no model",
			rawBody:     bytes.Repeat([]byte{0x00, 0x01, 'k', 'l'}, 96),
			contentType: "application/octet-stream",
		},
		{
			name:        "malformed multipart body leaves model unset without panic",
			rawBody:     []byte("total-garbage-not-multipart\r\n--broken\r\nno headers here\r\n"),
			contentType: "multipart/form-data; boundary=will-not-match",
		},
		{
			name: "quoted boundary parameter is parsed",
			specs: []scanPartSpec{
				{name: "prompt", value: "transcribe"},
				{name: "model", value: scanTestModelName},
			},
			quoteBoundary: true,
			wantModel:     scanTestModelName,
		},
		{
			name: "model field beyond scan window leaves model unset",
			specs: []scanPartSpec{
				{name: "file", filename: "big.bin", fileSize: MultipartModelScanSize + 64<<10},
				{name: "model", value: scanTestModelName},
			},
			// Model sits past MultipartModelScanSize, so the bounded scan
			// must stop (parse error / not found) and leave ModelName unset.
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contentType := tc.contentType
			body := tc.rawBody
			if body == nil {
				var boundary string
				body, contentType, boundary = buildMultipartBody(t, tc.specs)
				if tc.quoteBoundary {
					contentType = `multipart/form-data; boundary="` + boundary + `"`
				}
			}
			original := append([]byte(nil), body...)

			outReq, profile := inspectRequest(t, newScanRequest(contentType, body))

			if !assert.NotNil(t, profile, "inspection must return a profile") {
				return
			}
			assert.Equal(t, tc.wantModel, profile.ModelName, "profile.ModelName")

			if !assert.NotNil(t, outReq, "inspection must return a request") {
				return
			}
			if !assert.NotNil(t, outReq.Body, "returned request must carry a body") {
				return
			}
			consumed, err := io.ReadAll(outReq.Body)
			assert.NoError(t, err, "consuming restored body")
			assert.Equal(t, original, consumed, "restored body must be byte-identical to the original")
			assert.NoError(t, outReq.Body.Close())
		})
	}
}
