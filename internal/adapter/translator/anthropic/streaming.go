package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/thushan/olla/internal/core/constants"
	"github.com/thushan/olla/internal/util"
)

// sseDeltaEvent is the hot-path content_block_delta envelope.
// Field order is chosen for struct packing (interface{} + string + int fits without
// padding on amd64). JSON tag order (delta, index, type) differs from field
// declaration order — clients parse JSON by key, not position, so this is safe.
type sseDeltaEvent struct {
	Delta interface{} `json:"delta"` // sseTextDelta | sseThinkingDelta | sseInputJSONDelta
	Type  string      `json:"type"`
	Index int         `json:"index"`
}

// sseTextDelta is the delta payload for text_delta events.
// Field order matches alphabetical JSON tag order (text < type).
type sseTextDelta struct {
	Text string `json:"text"`
	Type string `json:"type"`
}

// sseThinkingDelta is the delta payload for thinking_delta events.
// Field order matches alphabetical JSON tag order (thinking < type).
type sseThinkingDelta struct {
	Thinking string `json:"thinking"`
	Type     string `json:"type"`
}

// sseInputJSONDelta is the delta payload for input_json_delta events.
// Field order matches alphabetical JSON tag order (partial_json < type).
type sseInputJSONDelta struct {
	PartialJSON string `json:"partial_json"`
	Type        string `json:"type"`
}

// errInterleavedToolArguments signals that args arrived for a tool block that was
// already stopped and closed on the wire. Corrupt tool input is worse than a failed
// stream — the client can retry a clean failure, but cannot recover silent data loss.
var errInterleavedToolArguments = errors.New("tool arguments received for already-stopped block")

// tracks state while streaming - buffers partial data, blocks in progress
type StreamingState struct {
	currentBlock     *ContentBlock
	toolCallBuffers  map[int]*strings.Builder // keyed by tool index, avoids string formatting overhead
	toolIndexToBlock map[int]int              // maps tool index to content block index for finalisation
	messageID        string
	model            string
	lastFinishReason string
	contentBlocks    []ContentBlock
	currentIndex     int
	inputTokens      int
	outputTokens     int
	messageStartSent bool
	// reasoningOpen is true while a thinking block has been started but not yet stopped.
	// The first non-reasoning delta closes the thinking block before opening its own.
	reasoningOpen bool
}

// convert openai sse stream to anthropic format
func (t *Translator) TransformStreamingResponse(ctx context.Context, openaiStream io.Reader, w http.ResponseWriter, original *http.Request) error {
	// text/event-stream, no caching
	w.Header().Set(constants.HeaderContentType, "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	rc := http.NewResponseController(w)

	state := &StreamingState{
		messageID:        t.generateMessageID(),
		contentBlocks:    make([]ContentBlock, 0, 4),
		toolCallBuffers:  make(map[int]*strings.Builder),
		toolIndexToBlock: make(map[int]int),
	}

	// Seed input_tokens from the pre-computed estimate injected by the handler.
	// vLLM and lmdeploy both emit real input_tokens in message_start rather than 0;
	// this brings the translation path in line with that behaviour. The value is
	// overwritten by actual upstream usage when (and if) the backend sends it.
	if estimate, ok := ctx.Value(constants.ContextInputTokensKey).(int); ok && estimate > 0 {
		state.inputTokens = estimate
	}

	// sync streaming for now (async needs more work for agent workflows)
	streamErr := t.transformStreamingSync(ctx, openaiStream, w, rc, state)

	if streamErr != nil {
		// Return immediately for all stream errors. For errInterleavedToolArguments the
		// SSE error event is already on the wire; emitting message_delta/message_stop after
		// an error event is invalid per the Anthropic spec.
		return streamErr
	}

	// send message_start even if stream was empty
	if !state.messageStartSent {
		if err := t.writeEvent(w, "message_start", t.createMessageStart(state)); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil {
			return fmt.Errorf("flush failed: %w", err)
		}
		state.messageStartSent = true
	}

	// send final events (stop reason + token counts)
	if err := t.finalizeStream(state, w, rc, original); err != nil {
		return err
	}

	return nil
}

// process stream using blocking scanner, safer and simpler
func (t *Translator) transformStreamingSync(ctx context.Context, openaiStream io.Reader, w http.ResponseWriter, rc *http.ResponseController, state *StreamingState) error {
	scanner := bufio.NewScanner(openaiStream)
	// allow large deltas and tool arg chunks, prevents "token too long" errors
	// initial buffer 64 KiB, max 1 MiB per SSE line (handles large tool arguments)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1<<20)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Text()
		if err := t.processStreamLine(line, state, w, rc); err != nil {
			// Interleaved tool args corrupt the client's tool input; abort the stream.
			if errors.Is(err, errInterleavedToolArguments) {
				return err
			}
			t.logger.Error("Error processing stream line", "error", err)
			continue // keep going, don't fail entire stream on one bad line
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading stream: %w", err)
	}

	return nil
}

// process single sse line from openai, route to content or tool handlers
func (t *Translator) processStreamLine(line string, state *StreamingState, w http.ResponseWriter, rc *http.ResponseController) error {
	if !strings.HasPrefix(line, "data: ") {
		return nil
	}

	data := strings.TrimPrefix(line, "data: ")
	if strings.TrimSpace(data) == "[DONE]" {
		return nil
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		// log bad chunks but keep going, partial responses better than nothing
		t.logger.Warn("Malformed chunk encountered, skipping", "error", err,
			"data", util.TruncateString(data, util.DefaultTruncateLengthPII), "data_len", len(data))
		return nil
	}

	// grab model name for message_start event
	if state.model == "" {
		if model, ok := chunk["model"].(string); ok {
			state.model = model
		}
	}

	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return nil
	}

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return nil
	}

	// capture finish_reason for later stop_reason mapping
	if finishReason, finishOk := choice["finish_reason"].(string); finishOk && finishReason != "" {
		state.lastFinishReason = finishReason
	}

	// grab usage stats if present (usually in final chunk)
	if usage, usageOk := chunk["usage"].(map[string]interface{}); usageOk {
		if promptTokens, promptOk := usage["prompt_tokens"].(float64); promptOk {
			state.inputTokens = int(promptTokens)
		}
		if completionTokens, completionsOk := usage["completion_tokens"].(float64); completionsOk {
			state.outputTokens = int(completionTokens)
		}
	}

	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return nil
	}

	// reasoning / reasoning_content are the per-backend field names for chain-of-thought.
	// Ollama, LM Studio, and Lemonade use "reasoning"; vLLM, SGLang, and DeepSeek use
	// "reasoning_content". Treat them as equivalent and prefer whichever is non-empty.
	reasoning := extractReasoningField(delta)
	if reasoning != "" {
		return t.handleReasoningDelta(reasoning, state, w, rc)
	}

	if content, ok := delta["content"].(string); ok && content != "" {
		return t.handleContentDelta(content, state, w, rc)
	}

	if toolCalls, ok := delta["tool_calls"].([]interface{}); ok {
		return t.handleToolCallsDelta(toolCalls, state, w, rc)
	}

	return nil
}

// extractReasoningField returns the non-empty reasoning text from a delta object,
// checking both field name variants used across backends:
//   - "reasoning"         - Ollama, LM Studio, Lemonade
//   - "reasoning_content" - vLLM, SGLang, DeepSeek
func extractReasoningField(delta map[string]interface{}) string {
	if v, ok := delta["reasoning"].(string); ok && v != "" {
		return v
	}
	if v, ok := delta["reasoning_content"].(string); ok && v != "" {
		return v
	}
	return ""
}

// send message_start if we haven't already, needs to be first event
func (t *Translator) ensureMessageStartSent(state *StreamingState, w http.ResponseWriter, rc *http.ResponseController) error {
	if !state.messageStartSent {
		if err := t.writeEvent(w, "message_start", t.createMessageStart(state)); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil {
			return fmt.Errorf("flush failed: %w", err)
		}
		state.messageStartSent = true
	}
	return nil
}

// process text delta, starts new block if needed
func (t *Translator) handleContentDelta(content string, state *StreamingState, w http.ResponseWriter, rc *http.ResponseController) error {
	if err := t.ensureMessageStartSent(state, w, rc); err != nil {
		return err
	}

	// start new text block if needed (anthropic wants block_start before deltas)
	if state.currentBlock == nil || state.currentBlock.Type != contentTypeText {
		// close whatever block is open (tool_use or a prior text block) before
		// opening a new text block; every start needs a matching stop.
		if err := t.closeCurrentBlock(state, w, rc); err != nil {
			return err
		}

		state.currentBlock = &ContentBlock{
			Type: contentTypeText,
			Text: "",
		}
		state.currentIndex = len(state.contentBlocks)
		state.contentBlocks = append(state.contentBlocks, *state.currentBlock)

		if err := t.writeEvent(w, "content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": state.currentIndex,
			"content_block": map[string]interface{}{
				"type": contentTypeText,
				"text": "",
			},
		}); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil {
			return fmt.Errorf("flush failed: %w", err)
		}
	}

	// send delta event for each chunk — typed struct avoids a per-chunk map allocation
	if err := t.writeEvent(w, "content_block_delta", sseDeltaEvent{
		Delta: sseTextDelta{Text: content, Type: "text_delta"},
		Index: state.currentIndex,
		Type:  "content_block_delta",
	}); err != nil {
		return err
	}
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("flush failed: %w", err)
	}

	// track accumulated text
	state.currentBlock.Text += content
	state.contentBlocks[state.currentIndex] = *state.currentBlock

	return nil
}

// handleReasoningDelta streams chain-of-thought text as an Anthropic thinking block.
// Reasoning always precedes content in model output, so we open a thinking block on the
// first reasoning chunk and leave it open until the first non-reasoning delta arrives,
// at which point closeCurrentBlock handles the stop event before the next block opens.
func (t *Translator) handleReasoningDelta(reasoning string, state *StreamingState, w http.ResponseWriter, rc *http.ResponseController) error {
	if err := t.ensureMessageStartSent(state, w, rc); err != nil {
		return err
	}

	// Open the thinking block on the first reasoning chunk.
	if !state.reasoningOpen {
		// Close anything that was already open (shouldn't happen in practice, but be safe).
		if err := t.closeCurrentBlock(state, w, rc); err != nil {
			return err
		}

		state.currentBlock = &ContentBlock{
			Type: contentTypeThinking,
		}
		state.currentIndex = len(state.contentBlocks)
		state.contentBlocks = append(state.contentBlocks, *state.currentBlock)
		state.reasoningOpen = true

		if err := t.writeEvent(w, "content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": state.currentIndex,
			"content_block": map[string]interface{}{
				"type":     contentTypeThinking,
				"thinking": "",
			},
		}); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil {
			return fmt.Errorf("flush failed: %w", err)
		}
	}

	// typed struct avoids a per-chunk map allocation on the thinking_delta hot path
	if err := t.writeEvent(w, "content_block_delta", sseDeltaEvent{
		Delta: sseThinkingDelta{Thinking: reasoning, Type: "thinking_delta"},
		Index: state.currentIndex,
		Type:  "content_block_delta",
	}); err != nil {
		return err
	}
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("flush failed: %w", err)
	}

	// Accumulate for inspector/logging.
	state.currentBlock.Thinking += reasoning
	state.contentBlocks[state.currentIndex] = *state.currentBlock

	return nil
}

// toolCallData holds extracted and validated tool call information
type toolCallData struct {
	id        string
	name      string
	arguments string
	toolIndex int
}

// extractToolCallData validates and extracts data from a tool call delta
func extractToolCallData(tc interface{}) (*toolCallData, bool) {
	toolCall, ok := tc.(map[string]interface{})
	if !ok {
		return nil, false
	}

	index, _ := toolCall["index"].(float64)
	toolIndex := int(index)

	function, ok := toolCall["function"].(map[string]interface{})
	if !ok {
		return nil, false
	}

	data := &toolCallData{
		toolIndex: toolIndex,
	}

	// extract optional fields
	if id, ok := toolCall["id"].(string); ok {
		data.id = id
	}
	if name, ok := function["name"].(string); ok {
		data.name = name
	}
	if args, ok := function["arguments"].(string); ok {
		data.arguments = args
	}

	return data, true
}

// closeCurrentBlock closes the currently open block regardless of type.
// Every content_block_start must be paired with a content_block_stop; SDK
// accumulators (including Claude Code's own stream reader) finalise tool
// input on the stop event, so omitting it silently breaks multi-tool calls.
func (t *Translator) closeCurrentBlock(state *StreamingState, w http.ResponseWriter, rc *http.ResponseController) error {
	if state.currentBlock == nil {
		return nil
	}
	if err := t.writeEvent(w, "content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": state.currentIndex,
	}); err != nil {
		return err
	}
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("flush failed: %w", err)
	}
	// Clear thinking state when closing a thinking block so subsequent reasoning
	// chunks (if any) would open a new block rather than append to a closed one.
	if state.currentBlock.Type == contentTypeThinking {
		state.reasoningOpen = false
	}
	state.currentBlock = nil
	return nil
}

// initializeToolBlock creates and sends a new tool_use block start event.
// If args arrived before the id+name chunk (pre-init buffering), flushes the
// buffer as a single input_json_delta immediately after content_block_start so
// the pre-init case is lossless.
func (t *Translator) initializeToolBlock(id, name string, toolIndex int, state *StreamingState, w http.ResponseWriter, rc *http.ResponseController) error {
	// close whatever block is currently open before starting a new one; both text
	// and prior tool blocks must be stopped before the next block_start is emitted.
	if err := t.closeCurrentBlock(state, w, rc); err != nil {
		return err
	}

	state.currentBlock = &ContentBlock{
		Type: contentTypeToolUse,
		ID:   id,
		Name: name,
	}
	state.currentIndex = len(state.contentBlocks)
	state.contentBlocks = append(state.contentBlocks, *state.currentBlock)

	// track which content block this tool index maps to for finalisation
	state.toolIndexToBlock[toolIndex] = state.currentIndex

	if err := t.writeEvent(w, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": state.currentIndex,
		"content_block": map[string]interface{}{
			"type": contentTypeToolUse,
			"id":   id,
			"name": name,
		},
	}); err != nil {
		return err
	}

	if err := rc.Flush(); err != nil {
		return fmt.Errorf("flush failed: %w", err)
	}

	// Flush any args that arrived before the id+name chunk opened this block.
	// Without this the pre-init buffer is silently discarded on the wire.
	if buf, exists := state.toolCallBuffers[toolIndex]; exists && buf.Len() > 0 {
		buffered := buf.String()
		if err := t.writeEvent(w, "content_block_delta", sseDeltaEvent{
			Delta: sseInputJSONDelta{PartialJSON: buffered, Type: "input_json_delta"},
			Index: state.currentIndex,
			Type:  "content_block_delta",
		}); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil {
			return fmt.Errorf("flush failed: %w", err)
		}
	}

	return nil
}

// sendToolArgumentsDelta buffers and emits a partial_json delta for the given tool index.
//
// Three cases:
//   - Block not yet initialised: buffer only; initializeToolBlock will flush on init.
//   - Block is the current open block: emit input_json_delta normally.
//   - Block was already stopped: corrupt wire state — emit an SSE error event and
//     return errInterleavedToolArguments so the stream is aborted cleanly.
func (t *Translator) sendToolArgumentsDelta(args string, toolIndex int, state *StreamingState, w http.ResponseWriter, rc *http.ResponseController) error {
	// Always buffer regardless of block state so finalisation stays correct.
	state.toolCallBuffers[toolIndex].WriteString(args)

	blockIndex, mapped := state.toolIndexToBlock[toolIndex]
	if !mapped {
		// Args arrived before id+name chunk; initializeToolBlock flushes the buffer.
		t.logger.Debug("Tool args received before block initialised, buffering only",
			"tool_index", toolIndex)
		return nil
	}

	// Interleaved or late delivery: args for a block that is already stopped.
	// Corrupt tool input is worse than a failed stream — abort with a spec-valid error event.
	if state.currentBlock == nil || blockIndex != state.currentIndex {
		msg := fmt.Sprintf("tool arguments received for already-stopped block (tool_index=%d, block_index=%d)", toolIndex, blockIndex)
		_ = t.writeEvent(w, "error", map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    "api_error",
				"message": msg,
			},
		})
		_ = rc.Flush()
		return errInterleavedToolArguments
	}

	// typed struct avoids a per-chunk map allocation on the input_json_delta hot path
	if err := t.writeEvent(w, "content_block_delta", sseDeltaEvent{
		Delta: sseInputJSONDelta{PartialJSON: args, Type: "input_json_delta"},
		Index: blockIndex,
		Type:  "content_block_delta",
	}); err != nil {
		return err
	}

	return rc.Flush()
}

// process tool call deltas, buffers partial json args
func (t *Translator) handleToolCallsDelta(toolCalls []interface{}, state *StreamingState, w http.ResponseWriter, rc *http.ResponseController) error {
	if err := t.ensureMessageStartSent(state, w, rc); err != nil {
		return err
	}

	for _, tc := range toolCalls {
		data, ok := extractToolCallData(tc)
		if !ok {
			continue
		}

		// initialise buffer if first time seeing this tool index
		if _, exists := state.toolCallBuffers[data.toolIndex]; !exists {
			state.toolCallBuffers[data.toolIndex] = &strings.Builder{}
		}

		// start block when we get id + name
		if data.id != "" && data.name != "" {
			if err := t.initializeToolBlock(data.id, data.name, data.toolIndex, state, w, rc); err != nil {
				return err
			}
		}

		// buffer args chunks and send as partial_json
		if data.arguments != "" {
			if err := t.sendToolArgumentsDelta(data.arguments, data.toolIndex, state, w, rc); err != nil {
				return err
			}
		}
	}

	return nil
}

// send final events, parse tool buffers, determine stop_reason
func (t *Translator) finalizeStream(state *StreamingState, w http.ResponseWriter, rc *http.ResponseController, original *http.Request) error {
	// close the last open block; with the single-active-block invariant restored
	// by closeCurrentBlock, exactly one stop is emitted here at most.
	if err := t.closeCurrentBlock(state, w, rc); err != nil {
		return err
	}

	// parse buffered json args into objects using the tool index mapping
	for toolIndex, builder := range state.toolCallBuffers {
		argsJSON := builder.String()
		if argsJSON != "" {
			var input map[string]interface{}
			if err := json.Unmarshal([]byte(argsJSON), &input); err == nil {
				// use mapping to find the correct block, avoids linear search
				if blockIndex, found := state.toolIndexToBlock[toolIndex]; found {
					// validate block type before updating to catch any state inconsistencies
					if state.contentBlocks[blockIndex].Type != contentTypeToolUse {
						t.logger.Error("Tool index maps to non-tool block",
							"tool_index", toolIndex,
							"block_index", blockIndex,
							"block_type", state.contentBlocks[blockIndex].Type)
						continue
					}
					state.contentBlocks[blockIndex].Input = input
				} else {
					// shouldn't happen if state is consistent, log for debugging
					t.logger.Error("Tool index not found in mapping",
						"tool_index", toolIndex,
						"available_mappings", len(state.toolIndexToBlock))
				}
			}
		}
	}

	// map finish_reason to stop_reason (same logic as non-streaming)
	stopReason := mapFinishReasonToStopReason(state.lastFinishReason)

	// send delta with stop_reason + usage
	if err := t.writeEvent(w, "message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]interface{}{
			"input_tokens":                state.inputTokens,
			"output_tokens":               state.outputTokens,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
		},
	}); err != nil {
		return err
	}
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("flush failed: %w", err)
	}

	// final event
	if err := t.writeEvent(w, "message_stop", map[string]interface{}{
		"type": "message_stop",
	}); err != nil {
		return err
	}
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("flush failed: %w", err)
	}

	// Log complete streaming response to inspector if enabled
	// Reconstructs the final response from streaming state for debugging
	if t.inspector.Enabled() {
		t.logStreamingResponse(state, original)
	}

	return nil
}

// logStreamingResponse logs the complete streaming response to inspector
// Reconstructs a complete Anthropic response from the streaming state
func (t *Translator) logStreamingResponse(state *StreamingState, original *http.Request) {
	// Build complete response matching the non-streaming format
	response := AnthropicResponse{
		ID:           state.messageID,
		Type:         "message",
		Role:         "assistant",
		Model:        state.model,
		Content:      state.contentBlocks,
		StopReason:   mapFinishReasonToStopReason(state.lastFinishReason),
		StopSequence: nil,
		Usage: AnthropicUsage{
			InputTokens:  state.inputTokens,
			OutputTokens: state.outputTokens,
		},
	}

	// Marshal to JSON for logging
	respBytes, err := json.Marshal(response)
	if err != nil {
		t.logger.Warn("Failed to marshal streaming response for inspector", "error", err)
		return
	}

	// Extract session ID from request header or fall back to defaults
	// Uses same logic as non-streaming response logging
	sessionID := defaultSessionID
	if original != nil {
		sessionID = t.getSessionID(original)
	}

	// Log the response
	if err := t.inspector.LogResponse(sessionID, respBytes); err != nil {
		t.logger.Warn("Failed to log streaming response to inspector", "error", err)
	}
}

// create initial message_start event with metadata
func (t *Translator) createMessageStart(state *StreamingState) map[string]interface{} {
	return map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            state.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         state.model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":                state.inputTokens,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		},
	}
}

// writeEvent serialises data as a Server-Sent Events frame and writes it to w.
//
// Uses the translator's buffer pool to avoid a heap allocation per event.
// The SSE frame format is: "event: <name>\ndata: <json>\n\n".
// The pooled buffer is reset and returned before this function returns, so
// there is no aliasing risk — w receives a copy via its own internal write path.
func (t *Translator) writeEvent(w http.ResponseWriter, event string, data interface{}) error {
	buf := t.bufferPool.Get()
	// Encode JSON directly into the pooled buffer, avoiding a separate []byte allocation.
	enc := json.NewEncoder(buf)
	if err := enc.Encode(data); err != nil {
		t.bufferPool.Put(buf)
		return fmt.Errorf("failed to marshal event data: %w", err)
	}
	// json.Encoder.Encode appends a trailing newline; trim it so our framing is exact.
	dataJSON := buf.Bytes()
	if len(dataJSON) > 0 && dataJSON[len(dataJSON)-1] == '\n' {
		dataJSON = dataJSON[:len(dataJSON)-1]
	}

	// Write the SSE frame in three direct writes — cheaper than fmt.Fprintf's
	// format string parsing and avoids a string concatenation allocation.
	var writeErr error
	if _, writeErr = io.WriteString(w, "event: "); writeErr == nil {
		if _, writeErr = io.WriteString(w, event); writeErr == nil {
			if _, writeErr = io.WriteString(w, "\ndata: "); writeErr == nil {
				if _, writeErr = w.Write(dataJSON); writeErr == nil {
					_, writeErr = io.WriteString(w, "\n\n")
				}
			}
		}
	}

	buf.Reset()
	t.bufferPool.Put(buf)

	if writeErr != nil {
		return fmt.Errorf("failed to write event: %w", writeErr)
	}
	return nil
}
