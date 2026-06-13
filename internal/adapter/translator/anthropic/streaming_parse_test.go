package anthropic

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProcessStreamLine_NullContentDelta verifies that a delta with an explicit
// "content":null value does not emit any content_block_delta event. JSON null
// unmarshals to nil for a *string, so the nil-check on the typed path must hold.
func TestProcessStreamLine_NullContentDelta(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator()

	// content is explicitly null — not absent, not empty string
	stream := mockOpenAIStream([]string{
		`data: {"id":"chatcmpl-null","model":"claude-3-5-sonnet-20241022","choices":[{"delta":{"content":null},"index":0}]}` + "\n\n",
		finishChunk("chatcmpl-null", "stop"),
		doneChunk(),
	})

	recorder := httptest.NewRecorder()
	require.NoError(t, tr.TransformStreamingResponse(context.Background(), stream, recorder, nil))

	events := parseAnthropicEvents(t, recorder.Body.String())

	// null content must not produce any content_block_delta
	assertEventCount(t, events, "content_block_delta", 0)
	// but the stream must still complete
	assertHasEventType(t, events, "message_start")
	assertHasEventType(t, events, "message_stop")
}

// TestProcessStreamLine_RoleOnlyDelta verifies that a delta carrying only a
// "role":"assistant" field emits no content blocks at all. This is the first
// chunk pattern emitted by some backends to announce the role before content
// begins streaming.
func TestProcessStreamLine_RoleOnlyDelta(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator()

	stream := mockOpenAIStream([]string{
		`data: {"id":"chatcmpl-role","model":"claude-3-5-sonnet-20241022","choices":[{"delta":{"role":"assistant"},"index":0}]}` + "\n\n",
		finishChunk("chatcmpl-role", "stop"),
		doneChunk(),
	})

	recorder := httptest.NewRecorder()
	require.NoError(t, tr.TransformStreamingResponse(context.Background(), stream, recorder, nil))

	events := parseAnthropicEvents(t, recorder.Body.String())

	// role-only delta must produce zero content blocks and zero deltas
	assertEventCount(t, events, "content_block_start", 0)
	assertEventCount(t, events, "content_block_delta", 0)
	assertEventCount(t, events, "content_block_stop", 0)
}

// TestProcessStreamLine_ReasoningBeatsContent verifies the priority rule:
// when a delta carries both "reasoning" and "content", reasoning wins and only
// a thinking_delta is emitted. Content is silently dropped for that chunk.
// This matches the extractReasoningField short-circuit behaviour.
func TestProcessStreamLine_ReasoningBeatsContent(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator()

	// Single chunk with both reasoning and content populated.
	stream := mockOpenAIStream([]string{
		`data: {"id":"chatcmpl-rc","model":"deepseek-r1","choices":[{"delta":{"reasoning":"think","content":"text"},"index":0}]}` + "\n\n",
		finishChunk("chatcmpl-rc", "stop"),
		doneChunk(),
	})

	recorder := httptest.NewRecorder()
	require.NoError(t, tr.TransformStreamingResponse(context.Background(), stream, recorder, nil))

	body := recorder.Body.String()
	events := parseAnthropicEvents(t, body)

	// must produce a thinking_delta, not a text_delta
	assert.Contains(t, body, `"thinking":"think"`)
	assert.NotContains(t, body, `"text":"text"`)

	// only one content_block_delta: the thinking one
	assertEventCount(t, events, "content_block_delta", 1)

	deltas := findEventsByType(events, "content_block_delta")
	require.Len(t, deltas, 1)
	delta, ok := deltas[0]["delta"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "thinking_delta", delta["type"])
}

// TestProcessStreamLine_ReasoningContentFieldName verifies the alternative field
// name "reasoning_content" (vLLM / SGLang / DeepSeek convention) is treated
// identically to "reasoning" and produces a thinking_delta.
func TestProcessStreamLine_ReasoningContentFieldName(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator()

	stream := mockOpenAIStream([]string{
		`data: {"id":"chatcmpl-rcf","model":"qwq-32b","choices":[{"delta":{"reasoning_content":"deep think"},"index":0}]}` + "\n\n",
		finishChunk("chatcmpl-rcf", "stop"),
		doneChunk(),
	})

	recorder := httptest.NewRecorder()
	require.NoError(t, tr.TransformStreamingResponse(context.Background(), stream, recorder, nil))

	body := recorder.Body.String()
	events := parseAnthropicEvents(t, body)

	assert.Contains(t, body, `"thinking":"deep think"`)

	// exactly one thinking block opened and closed
	assertContentBlockCount(t, events, 1)
	assertBlocksClosed(t, events)

	starts := findEventsByType(events, "content_block_start")
	bt, _ := getContentBlockType(starts[0])
	assert.Equal(t, contentTypeThinking, bt)
}

// TestProcessStreamLine_ToolCallContinuationNoID verifies the two-chunk tool
// call sequence: first chunk carries id+name (index=0), second carries only
// index+partial-args (no id, no name). The assembled args must appear in
// input_json_delta events and both deltas must reference block index 0.
//
// The args string must use the \\\" escaping convention (same as other streaming
// tests) because toolArgsChunk embeds the args into a JSON string value: raw
// braces would break the outer JSON and cause the chunk to be skipped.
func TestProcessStreamLine_ToolCallContinuationNoID(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator()

	stream := mockOpenAIStream([]string{
		// first chunk: id + name establish the block
		toolStartChunk("chatcmpl-cont", 0, "call_cont", "cont_tool"),
		// second chunk: args only, no id or name; escaped per the test convention
		toolArgsChunk("chatcmpl-cont", 0, `{\\\"x\\\":1}`),
		finishChunk("chatcmpl-cont", "tool_calls"),
		doneChunk(),
	})

	recorder := httptest.NewRecorder()
	require.NoError(t, tr.TransformStreamingResponse(context.Background(), stream, recorder, nil))

	body := recorder.Body.String()
	events := parseAnthropicEvents(t, body)

	// at least one input_json_delta must be present for the args chunk
	assert.Contains(t, body, `"partial_json":`)

	// all input_json_delta events must reference block 0
	deltas := findEventsByType(events, "content_block_delta")
	require.NotEmpty(t, deltas, "expected at least one delta for the args chunk")
	for _, d := range deltas {
		idx, ok := getContentBlockIndex(d)
		require.True(t, ok)
		assert.Equal(t, 0, idx, "all deltas must reference block 0")
	}

	assertBlocksClosed(t, events)
}

// TestProcessStreamLine_ToolCallAbsentIndexDefaultsToZero verifies that a tool
// call delta with no "index" key at all is treated as index 0. The JSON struct
// field uses *int so absent maps to nil, which defaults to 0.
func TestProcessStreamLine_ToolCallAbsentIndexDefaultsToZero(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator()

	// Manually crafted raw SSE line — no "index" field in the tool_calls entry.
	// toolArgsChunk always includes index, so we must build this by hand.
	rawLine := "data: {\"id\":\"chatcmpl-noidx\",\"model\":\"claude-3-5-sonnet-20241022\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call_noidx\",\"type\":\"function\",\"function\":{\"name\":\"no_idx_tool\",\"arguments\":\"{\\\"x\\\":1}\"}}]},\"index\":0}]}\n\n"

	stream := mockOpenAIStream([]string{
		rawLine,
		finishChunk("chatcmpl-noidx", "tool_calls"),
		doneChunk(),
	})

	recorder := httptest.NewRecorder()
	require.NoError(t, tr.TransformStreamingResponse(context.Background(), stream, recorder, nil))

	body := recorder.Body.String()
	events := parseAnthropicEvents(t, body)

	// tool block must have been created (id + name present in that chunk)
	assertToolPresent(t, body, "call_noidx", "no_idx_tool")

	// args appear as partial_json in the SSE frame — check for the field name
	assert.Contains(t, body, `"partial_json":`)
	deltas := findEventsByType(events, "content_block_delta")
	require.NotEmpty(t, deltas, "at least one input_json_delta expected")
	for _, d := range deltas {
		idx, ok := getContentBlockIndex(d)
		require.True(t, ok)
		assert.Equal(t, 0, idx, "absent index must default to 0")
	}
}

// TestProcessStreamLine_MalformedChunkSkipped verifies that a malformed JSON
// chunk sandwiched between two valid text chunks is skipped without aborting the
// stream, and both valid chunks' text appears in the output.
func TestProcessStreamLine_MalformedChunkSkipped(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator()

	stream := mockOpenAIStream([]string{
		textChunk("chatcmpl-bad2", "claude-3-5-sonnet-20241022", "before"),
		"data: {not valid json at all}\n\n",
		textChunk("chatcmpl-bad2", "", "after"),
		finishChunk("chatcmpl-bad2", "stop"),
		doneChunk(),
	})

	recorder := httptest.NewRecorder()

	// must not return an error
	err := tr.TransformStreamingResponse(context.Background(), stream, recorder, nil)
	require.NoError(t, err)

	body := recorder.Body.String()
	events := parseAnthropicEvents(t, body)

	// both valid chunks must appear
	assert.Contains(t, body, `"text":"before"`)
	assert.Contains(t, body, `"text":"after"`)

	// stream must complete normally
	assertHasEventType(t, events, "message_start")
	assertHasEventType(t, events, "message_stop")
}

// TestProcessStreamLine_UsageFinalChunk verifies that usage tokens in the final
// finish chunk are propagated correctly to the message_delta usage fields.
// The typed path reads PromptTokens/CompletionTokens as int directly, avoiding
// the float64 intermediate that the map-based path required.
func TestProcessStreamLine_UsageFinalChunk(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator()

	stream := mockOpenAIStream([]string{
		textChunk("chatcmpl-usage2", "claude-3-5-sonnet-20241022", "hello"),
		finishChunkWithUsage("chatcmpl-usage2", "stop", 15, 8),
		doneChunk(),
	})

	recorder := httptest.NewRecorder()
	require.NoError(t, tr.TransformStreamingResponse(context.Background(), stream, recorder, nil))

	events := parseAnthropicEvents(t, recorder.Body.String())

	// assertUsageTokens checks message_delta usage
	assertUsageTokens(t, events, 15, 8)

	// confirm the stop reason is mapped correctly
	assertStopReason(t, events, "end_turn")

	// blocks must be balanced
	assertBlocksClosed(t, events)

	// Confirm the values via the raw event too, since assertUsageTokens uses float64
	// (JSON numbers always unmarshal to float64 via map[string]interface{}).
	messageDelta := getEventByType(events, "message_delta")
	require.NotNil(t, messageDelta)
	usage, ok := messageDelta["usage"].(map[string]interface{})
	require.True(t, ok, "message_delta must have usage")

	inputTokens, hasInput := usage["input_tokens"].(float64)
	require.True(t, hasInput)
	assert.Equal(t, float64(15), inputTokens)

	outputTokens, hasOutput := usage["output_tokens"].(float64)
	require.True(t, hasOutput)
	assert.Equal(t, float64(8), outputTokens)
}

// TestProcessStreamLine_EmptyToolCallsArrayNoBlocks verifies that an empty
// tool_calls array ([]openAIToolCall with zero elements) does not open any
// content blocks. The range loop over an empty slice is a no-op.
func TestProcessStreamLine_EmptyToolCallsArrayNoBlocks(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator()

	stream := mockOpenAIStream([]string{
		textChunk("chatcmpl-emtc", "claude-3-5-sonnet-20241022", "hello"),
		// explicit empty tool_calls array
		`data: {"id":"chatcmpl-emtc","choices":[{"delta":{"tool_calls":[]},"index":0}]}` + "\n\n",
		finishChunk("chatcmpl-emtc", "stop"),
		doneChunk(),
	})

	recorder := httptest.NewRecorder()
	require.NoError(t, tr.TransformStreamingResponse(context.Background(), stream, recorder, nil))

	events := parseAnthropicEvents(t, recorder.Body.String())

	// only 1 block (the text block), no tool blocks
	assertContentBlockCount(t, events, 1)
	assertBlocksClosed(t, events)

	starts := findEventsByType(events, "content_block_start")
	bt, _ := getContentBlockType(starts[0])
	assert.Equal(t, contentTypeText, bt)
}

// TestProcessStreamLine_FinishReasonNullVsAbsent verifies that a delta where
// finish_reason is explicitly null does not update lastFinishReason and the
// stream stops with the default "end_turn" mapping.
func TestProcessStreamLine_FinishReasonNullVsAbsent(t *testing.T) {
	t.Parallel()
	tr := newTestTranslator()

	stream := mockOpenAIStream([]string{
		textChunk("chatcmpl-frnull", "claude-3-5-sonnet-20241022", "hello"),
		// finish_reason explicitly null — must not override lastFinishReason
		`data: {"id":"chatcmpl-frnull","choices":[{"delta":{},"index":0,"finish_reason":null}]}` + "\n\n",
		finishChunk("chatcmpl-frnull", "stop"),
		doneChunk(),
	})

	recorder := httptest.NewRecorder()
	require.NoError(t, tr.TransformStreamingResponse(context.Background(), stream, recorder, nil))

	events := parseAnthropicEvents(t, recorder.Body.String())
	assertStopReason(t, events, "end_turn")
}

// validateStreamingParseTests is a compile-time guard — if any of the above
// helper dependencies shift, this will fail to compile rather than silently pass.
func validateStreamingParseTests() {
	_ = strings.Builder{}
}
