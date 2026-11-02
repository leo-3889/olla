package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	ollitranslator "github.com/thushan/olla/internal/adapter/translator"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The 2026-10-14 vision audit proved the Anthropic path parsed image blocks,
// logged at Debug, and DROPPED them: HTTP 200 with prompt_tokens identical to
// the text-only control. These tests pin the replacement: base64 images
// become OpenAI image_url parts, url sources are an explicit 415-class
// rejection, and a text-only message keeps the joined-string content
// byte-for-byte.

func transformBody(t *testing.T, tr *Translator, req AnthropicRequest) []map[string]interface{} {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	result, err := tr.TransformRequest(context.Background(), &http.Request{Body: io.NopCloser(bytes.NewReader(body))})
	require.NoError(t, err)
	messages, ok := result.OpenAIRequest["messages"].([]map[string]interface{})
	require.True(t, ok)
	return messages
}

func imageBlock(source map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "image", "source": source}
}

func TestTransformRequest_TextBlocksStayAJoinedString(t *testing.T) {
	tr := mustNewTranslator(createTestLogger(), createTestConfig())
	messages := transformBody(t, tr, AnthropicRequest{
		Model: "m", MaxTokens: 8,
		Messages: []AnthropicMessage{{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "a"},
			map[string]interface{}{"type": "text", "text": "b"},
		}}},
	})
	require.Len(t, messages, 1)
	assert.Equal(t, "ab", messages[0]["content"], "no images must keep the legacy joined-string content")
}

func TestTransformRequest_Base64ImageBecomesAnImageURLPart(t *testing.T) {
	tr := mustNewTranslator(createTestLogger(), createTestConfig())
	messages := transformBody(t, tr, AnthropicRequest{
		Model: "m", MaxTokens: 8,
		Messages: []AnthropicMessage{{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "What colour is the LEFT half?"},
			imageBlock(map[string]interface{}{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}),
		}}},
	})
	require.Len(t, messages, 1)
	parts, ok := messages[0]["content"].([]map[string]interface{})
	require.True(t, ok, "image-carrying content must be the OpenAI parts array")
	require.Len(t, parts, 2)
	assert.Equal(t, "text", parts[0]["type"])
	assert.Equal(t, "What colour is the LEFT half?", parts[0]["text"])
	assert.Equal(t, "image_url", parts[1]["type"])
	urlPart, ok := parts[1]["image_url"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "data:image/png;base64,iVBORw0KGgo=", urlPart["url"])
}

func TestTransformRequest_ImageOnlyMessageIsNotDropped(t *testing.T) {
	tr := mustNewTranslator(createTestLogger(), createTestConfig())
	messages := transformBody(t, tr, AnthropicRequest{
		Model: "m", MaxTokens: 8,
		Messages: []AnthropicMessage{{Role: "user", Content: []interface{}{
			imageBlock(map[string]interface{}{"type": "base64", "media_type": "image/jpeg", "data": "/9j/4AAQ"}),
		}}},
	})
	require.Len(t, messages, 1, "an image-only user message used to vanish with the text it rode with")
	parts, ok := messages[0]["content"].([]map[string]interface{})
	require.True(t, ok)
	require.Len(t, parts, 1)
	urlPart := parts[0]["image_url"].(map[string]interface{})
	assert.Equal(t, "data:image/jpeg;base64,/9j/4AAQ", urlPart["url"])
}

func TestTransformRequest_URLSourceIsAnExplicitRejection(t *testing.T) {
	tr := mustNewTranslator(createTestLogger(), createTestConfig())
	body, err := json.Marshal(AnthropicRequest{
		Model: "m", MaxTokens: 8,
		Messages: []AnthropicMessage{{Role: "user", Content: []interface{}{
			imageBlock(map[string]interface{}{"type": "url", "url": "http://169.254.169.254/latest/meta-data/"}),
		}}},
	})
	require.NoError(t, err)
	_, err = tr.TransformRequest(context.Background(), &http.Request{Body: io.NopCloser(bytes.NewReader(body))})
	require.Error(t, err, "a host-referenced image must never be silently dropped (the SSRF surface is also why it is not fetched)")
	var unsupported *ollitranslator.UnsupportedContentError
	require.True(t, errors.As(err, &unsupported), "url sources must surface as UnsupportedContentError so the handler answers 415, got %T: %v", err, err)
}

func TestTransformRequest_MalformedBase64BlockIsA400ClassError(t *testing.T) {
	tr := mustNewTranslator(createTestLogger(), createTestConfig())
	body, err := json.Marshal(AnthropicRequest{
		Model: "m", MaxTokens: 8,
		Messages: []AnthropicMessage{{Role: "user", Content: []interface{}{
			imageBlock(map[string]interface{}{"type": "base64", "media_type": "image/png"}),
		}}},
	})
	require.NoError(t, err)
	_, err = tr.TransformRequest(context.Background(), &http.Request{Body: io.NopCloser(bytes.NewReader(body))})
	require.Error(t, err)
	var unsupported *ollitranslator.UnsupportedContentError
	assert.False(t, errors.As(err, &unsupported), "a malformed block is the client's 400, not an unsupported-format 415")
}

func TestTransformRequest_ToolResultStaysSeparateFromImageParts(t *testing.T) {
	tr := mustNewTranslator(createTestLogger(), createTestConfig())
	messages := transformBody(t, tr, AnthropicRequest{
		Model: "m", MaxTokens: 8,
		Messages: []AnthropicMessage{
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "tu1", "content": "ok"},
				map[string]interface{}{"type": "text", "text": "and this one?"},
				imageBlock(map[string]interface{}{"type": "base64", "media_type": "image/png", "data": "AAAA"}),
			}},
		},
	})
	require.Len(t, messages, 2)
	assert.Equal(t, "tool", messages[0]["role"], "tool results keep their own message (OpenAI ordering rule)")
	parts, ok := messages[1]["content"].([]map[string]interface{})
	require.True(t, ok)
	require.Len(t, parts, 2, "the tool_result must not occupy a multimodal part slot")
	assert.Equal(t, "text", parts[0]["type"])
	assert.Equal(t, "image_url", parts[1]["type"])
}
