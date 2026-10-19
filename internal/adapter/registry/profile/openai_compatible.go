package profile

// OpenAICompatibleResponse is the response structure from OpenAI-compatible /v1/models endpoint
type OpenAICompatibleResponse struct {
	Object string                  `json:"object"`
	Data   []OpenAICompatibleModel `json:"data"`
}

// OpenAICompatibleModel represents a model in OpenAI-compatible response
type OpenAICompatibleModel struct {
	Created     *int64  `json:"created,omitempty"`
	OwnedBy     *string `json:"owned_by,omitempty"`
	MaxModelLen *int64  `json:"max_model_len,omitempty"`
	// Capabilities is the engine-declared feature map (radiance: {"image":true,
	// "video":true}); absent on strict OpenAI servers.
	Capabilities map[string]bool `json:"capabilities,omitempty"`
	ID           string          `json:"id"`
	Object       string          `json:"object"`
}
