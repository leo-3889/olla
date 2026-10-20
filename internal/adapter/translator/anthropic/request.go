package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/thushan/olla/internal/adapter/translator"
)

// convert anthropic format to openai, handles messages/tools/params
func (t *Translator) TransformRequest(ctx context.Context, r *http.Request) (*translator.TransformedRequest, error) {
	// limit request body to prevent DOS, uses configured max size
	limitedBody := io.LimitReader(r.Body, t.maxMessageSize)
	defer r.Body.Close()

	// Parse leniently: as a proxy we must not be stricter than the upstream API we emulate.
	// Unknown or newer Anthropic fields (e.g. context_management, experimental beta fields)
	// are silently ignored rather than rejected. The streaming decoder is kept for memory
	// efficiency on large requests.
	var anthropicReq AnthropicRequest
	decoder := json.NewDecoder(limitedBody)

	if err := decoder.Decode(&anthropicReq); err != nil {
		return nil, fmt.Errorf("failed to parse Anthropic request: %w", err)
	}

	if err := anthropicReq.Validate(); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	// re-marshal to get body bytes for passthrough
	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Log request to inspector if enabled
	if t.inspector.Enabled() {
		sessionID := t.getSessionID(r)
		if lerr := t.inspector.LogRequest(sessionID, anthropicReq.Model, body); lerr != nil {
			t.logger.Warn("Failed to log request to inspector", "error", lerr)
		}
	}

	// Convert to OpenAI format
	openaiReq := make(map[string]interface{})

	// Basic parameters - direct mapping
	openaiReq["model"] = anthropicReq.Model
	openaiReq["max_tokens"] = anthropicReq.MaxTokens
	openaiReq["stream"] = anthropicReq.Stream

	// Optional parameters
	if anthropicReq.Temperature != nil {
		openaiReq["temperature"] = *anthropicReq.Temperature
	}
	if anthropicReq.TopP != nil {
		openaiReq["top_p"] = *anthropicReq.TopP
	}
	if len(anthropicReq.StopSequences) > 0 {
		openaiReq["stop"] = anthropicReq.StopSequences
	}

	// Convert messages (includes system prompt injection)
	openaiMessages, err := t.convertMessages(anthropicReq.Messages, anthropicReq.System)
	if err != nil {
		return nil, fmt.Errorf("failed to convert messages: %w", err)
	}
	openaiReq["messages"] = openaiMessages

	// Convert tools if present
	if len(anthropicReq.Tools) > 0 {
		openaiTools := t.convertTools(anthropicReq.Tools)
		openaiReq["tools"] = openaiTools

		// Convert tool_choice
		if anthropicReq.ToolChoice != nil {
			openaiToolChoice, tcErr := t.convertToolChoice(anthropicReq.ToolChoice)
			if tcErr != nil {
				return nil, fmt.Errorf("failed to convert tool_choice: %w", tcErr)
			}
			openaiReq["tool_choice"] = openaiToolChoice
		}
	}

	t.logger.Debug("Transformed Anthropic request to OpenAI",
		"model", anthropicReq.Model,
		"message_count", len(anthropicReq.Messages),
		"has_tools", len(anthropicReq.Tools) > 0,
		"streaming", anthropicReq.Stream)

	return &translator.TransformedRequest{
		OpenAIRequest: openaiReq,
		OriginalBody:  body,
		ModelName:     anthropicReq.Model,
		IsStreaming:   anthropicReq.Stream,
		TargetPath:    "/v1/chat/completions",
		Metadata: map[string]interface{}{
			"format": "anthropic",
		},
	}, nil
}

// convert messages + inject system prompt if present
func (t *Translator) convertMessages(anthropicMessages []AnthropicMessage, systemPrompt interface{}) ([]map[string]interface{}, error) {
	openaiMessages := make([]map[string]interface{}, 0, len(anthropicMessages)+1)

	// openai wants system as first message, can be string or content blocks.
	//
	// Claude Code also emits system-reminder turns as role:"system" entries
	// *inside* messages: block content on the first request, string content when
	// the same turn is replayed on later ones. Emitting those in place puts a
	// system message at index > 0, which chat templates reject ("System message
	// must be at the beginning"), so fold them into the leading system message.
	systemParts := make([]string, 0, len(anthropicMessages)+1)
	if systemPrompt != nil {
		if s, ok := t.convertSystemPrompt(systemPrompt).(string); ok && s != "" {
			systemParts = append(systemParts, s)
		}
	}
	for _, msg := range anthropicMessages {
		if msg.Role != "system" {
			continue
		}
		if s, ok := t.convertSystemPrompt(msg.Content).(string); ok && s != "" {
			systemParts = append(systemParts, s)
		}
	}
	if len(systemParts) > 0 {
		openaiMessages = append(openaiMessages, map[string]interface{}{
			"role":    "system",
			"content": strings.Join(systemParts, "\n\n"),
		})
	}

	for _, msg := range anthropicMessages {
		if msg.Role == "system" {
			continue // already hoisted into the leading system message
		}
		converted, err := t.convertSingleMessage(msg)
		if err != nil {
			return nil, err
		}
		openaiMessages = append(openaiMessages, converted...)
	}

	return openaiMessages, nil
}

// convert single message, might produce multiple openai messages (eg user + tool results)
func (t *Translator) convertSingleMessage(msg AnthropicMessage) ([]map[string]interface{}, error) {
	result := make([]map[string]interface{}, 0, 2)

	// only user/assistant reach the wire; the string fast-path below copies
	// msg.Role verbatim, so any other role would land at the wrong index.
	if msg.Role != "user" && msg.Role != "assistant" {
		return result, nil
	}

	if contentStr, ok := msg.Content.(string); ok {
		if contentStr != "" {
			result = append(result, map[string]interface{}{
				"role":    msg.Role,
				"content": contentStr,
			})
		}
		return result, nil
	}

	// anthropic uses content blocks for rich messages
	contentBlocks, ok := msg.Content.([]interface{})
	if !ok {
		if contentMap, ok := msg.Content.(map[string]interface{}); ok {
			contentBlocks = []interface{}{contentMap}
		} else {
			return nil, fmt.Errorf("invalid content type: %T", msg.Content)
		}
	}

	// user msgs can have text + tool results, assistant msgs have text + tool uses
	if msg.Role == "user" {
		userMsg, toolMsgs, err := t.convertUserMessage(contentBlocks)
		if err != nil {
			return nil, err
		}
		// tool messages must immediately follow the assistant tool_calls message;
		// OpenAI-compatible backends reject any other role between them.
		result = append(result, toolMsgs...)
		if userMsg != nil {
			result = append(result, userMsg)
		}
	} else if msg.Role == "assistant" {
		assistantMsg := t.convertAssistantMessage(contentBlocks)
		if assistantMsg != nil {
			result = append(result, assistantMsg)
		}
	}

	return result, nil
}

// split user message into text + tool results (openai needs tool results as separate messages)
func (t *Translator) convertUserMessage(blocks []interface{}) (map[string]interface{}, []map[string]interface{}, error) {
	var textParts []string
	var toolResults []map[string]interface{}
	// parts keeps document order for the multimodal wire format. It is only
	// used when the message carries an image: a text-only user message keeps
	// the joined-string content byte-for-byte, so the overwhelmingly common
	// path is untouched.
	var parts []map[string]interface{}
	hasImage := false

	for _, block := range blocks {
		blockMap, ok := block.(map[string]interface{})
		if !ok {
			continue
		}

		blockType, _ := blockMap["type"].(string)
		switch blockType {
		case contentTypeText:
			if text, ok := blockMap["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
				parts = append(parts, map[string]interface{}{"type": contentTypeText, "text": text})
			}
		case contentTypeToolResult:
			// map tool_use_id to tool_call_id
			toolUseID, _ := blockMap["tool_use_id"].(string)

			// content can be string or structured, convert to string
			content := ""
			if contentStr, ok := blockMap["content"].(string); ok {
				content = contentStr
			} else if contentObj := blockMap["content"]; contentObj != nil {
				if contentBytes, err := json.Marshal(contentObj); err == nil {
					content = string(contentBytes)
				}
			}

			// OpenAI tool messages have no is_error field; encode the signal in the
			// content string so the model can distinguish error results on the way back.
			// Skip the prefix if the content already starts with "Error" to avoid
			// double-prefixing when re-translating an already-annotated result.
			isError, _ := blockMap["is_error"].(bool)
			if isError && !strings.HasPrefix(strings.ToLower(content), "error") {
				content = "Error: " + content
			}

			toolResults = append(toolResults, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": toolUseID,
				"content":      content,
			})
		case contentTypeImage:
			part, err := imagePart(blockMap)
			if err != nil {
				return nil, nil, err
			}
			hasImage = true
			parts = append(parts, part)
		}
	}

	var userMsg map[string]interface{}
	switch {
	case hasImage && len(parts) > 0:
		// Multimodal: the OpenAI chat format wants the parts array; text
		// blocks ride in document order next to their images.
		userMsg = map[string]interface{}{
			"role":    "user",
			"content": parts,
		}
	case len(textParts) > 0:
		userMsg = map[string]interface{}{
			"role":    "user",
			"content": strings.Join(textParts, ""),
		}
	}

	return userMsg, toolResults, nil
}

// imagePart maps an Anthropic image block to its OpenAI chat-format twin.
// base64 sources travel as data URLs. source.type=url is rejected, not
// rewritten: fetching a client-supplied URL from the gateway would turn it
// into an SSRF primitive, and shipping the raw URL to the backend hands the
// fetch to an engine whose egress the operator never chose. A client that
// means the model to see the image sends base64 (the Anthropic SDKs'
// default for local files).
func imagePart(block map[string]interface{}) (map[string]interface{}, error) {
	source, _ := block["source"].(map[string]interface{})
	if source == nil {
		return nil, errors.New("invalid content type: image block without source")
	}
	sourceType, _ := source["type"].(string)
	if sourceType != "base64" {
		return nil, &translator.UnsupportedContentError{
			Detail: fmt.Sprintf("image source.type=%q; the Anthropic translator carries base64 images only (url sources are not fetched server-side)", sourceType),
		}
	}
	mediaType, _ := source["media_type"].(string)
	data, _ := source["data"].(string)
	if mediaType == "" || data == "" {
		return nil, errors.New("invalid content type: base64 image block missing media_type or data")
	}
	return map[string]interface{}{
		"type":      "image_url",
		"image_url": map[string]interface{}{"url": "data:" + mediaType + ";base64," + data},
	}, nil
}

// combine text + tool uses into single openai message
func (t *Translator) convertAssistantMessage(blocks []interface{}) map[string]interface{} {
	msg := map[string]interface{}{
		"role": "assistant",
	}

	var textContent string
	var toolCalls []map[string]interface{}

	for _, block := range blocks {
		blockMap, ok := block.(map[string]interface{})
		if !ok {
			continue
		}

		blockType, _ := blockMap["type"].(string)
		switch blockType {
		case contentTypeText:
			if text, ok := blockMap["text"].(string); ok {
				textContent += text
			}
		case contentTypeToolUse:
			toolCall := t.convertToolUse(blockMap)
			if toolCall != nil {
				toolCalls = append(toolCalls, toolCall)
			}
		}
	}

	// openai wants null content when only tool calls present
	if textContent != "" {
		msg["content"] = textContent
	} else if len(toolCalls) > 0 {
		msg["content"] = nil
	}

	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	// OLLA-287: skip empty assistant messages
	if textContent == "" && len(toolCalls) == 0 {
		return nil
	}
	return msg
}

// convert tool_use to openai tool_call format
func (t *Translator) convertToolUse(block map[string]interface{}) map[string]interface{} {
	id, _ := block["id"].(string)
	name, _ := block["name"].(string)
	input, _ := block["input"].(map[string]interface{})

	if id == "" || name == "" {
		return nil
	}

	// openai wants args as json string
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.logger.Warn("Failed to marshal tool input", "error", err)
		inputJSON = []byte("{}")
	}

	return map[string]interface{}{
		"id":   id,
		"type": openAITypeFunction,
		"function": map[string]interface{}{
			"name":      name,
			"arguments": string(inputJSON),
		},
	}
}

// convert system prompt, handles string or content blocks
func (t *Translator) convertSystemPrompt(systemPrompt interface{}) interface{} {
	// use the iterator to extract text from all content blocks
	var textParts []string

	// iterator handles all type conversions (string, []ContentBlock, *[]ContentBlock, []interface{})
	_ = t.forEachSystemContentBlock(systemPrompt, func(block ContentBlock) error {
		if block.Type == contentTypeText && block.Text != "" {
			textParts = append(textParts, block.Text)
		}
		return nil
	})

	if len(textParts) == 0 {
		return nil
	}

	// join text blocks directly without separator
	return strings.Join(textParts, "")
}
