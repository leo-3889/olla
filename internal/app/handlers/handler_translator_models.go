package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/thushan/olla/internal/adapter/translator"
	"github.com/thushan/olla/internal/core/constants"
	"github.com/thushan/olla/internal/core/domain"
)

// processStartTime is captured once at startup so that created_at in the
// Anthropic models response is stable across calls. There is no genuine
// creation timestamp on UnifiedModel (LastSeen is a discovery heartbeat that
// shifts on every health poll), so process start is the best stable proxy.
var processStartTime = time.Now().UTC().Format(time.RFC3339)

// list models in translator format (eg /olla/anthropic/v1/models)
func (a *Application) translatorModelsHandler(trans translator.RequestTranslator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// fetch available models from registry
		var unifiedModels []*domain.UnifiedModel
		var err error

		// check if registry supports getting unified models
		type unifiedModelsGetter interface {
			GetUnifiedModels(ctx context.Context) ([]*domain.UnifiedModel, error)
		}

		if getter, ok := a.modelRegistry.(unifiedModelsGetter); ok {
			unifiedModels, err = getter.GetUnifiedModels(ctx)
			if err != nil {
				a.writeTranslatorModelsError(w, trans, "failed to get unified models", http.StatusInternalServerError)
				return
			}
		} else {
			a.writeTranslatorModelsError(w, trans, "unified models not supported", http.StatusInternalServerError)
			return
		}

		// skip unhealthy models for endpoint response
		healthyModels, err := a.filterModelsByHealth(ctx, unifiedModels)
		if err != nil {
			a.writeTranslatorModelsError(w, trans, "failed to filter models by health", http.StatusInternalServerError)
			return
		}

		// Anthropic format matches the Python reference: {data: [{id, name, created, description, type}]}
		response := a.convertModelsToAnthropicFormat(healthyModels)

		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
	}
}

// convertModelsToAnthropicFormat emits the Anthropic GET /v1/models wire format.
// The SDK uses "type" as a deserialise discriminator so it must be "model", not "chat".
// "display_name" and "created_at" (RFC3339) match the published Anthropic spec.
func (a *Application) convertModelsToAnthropicFormat(models []*domain.UnifiedModel) map[string]interface{} {
	data := make([]map[string]interface{}, 0, len(models))

	for _, model := range models {
		// prefer the first alias as the canonical id clients send back to us
		modelID := model.ID
		if len(model.Aliases) > 0 {
			modelID = model.Aliases[0].Name
		}

		entry := map[string]interface{}{
			"type":         "model",
			"id":           modelID,
			"display_name": modelID,
			"created_at":   processStartTime,
		}

		data = append(data, entry)
	}

	// first_id / last_id are null when the list is empty
	var firstID, lastID interface{}
	if len(data) > 0 {
		firstID = data[0]["id"]
		lastID = data[len(data)-1]["id"]
	}

	return map[string]interface{}{
		"data":     data,
		"has_more": false,
		"first_id": firstID,
		"last_id":  lastID,
	}
}

// send error response for models endpoint
func (a *Application) writeTranslatorModelsError(w http.ResponseWriter, trans translator.RequestTranslator, message string, statusCode int) {
	a.logger.Error("Translator models request failed",
		"translator", trans.Name(),
		"error", message,
		"status", statusCode)

	// use custom error format or fallback to generic
	if errorWriter, ok := trans.(translator.ErrorWriter); ok {
		errorWriter.WriteError(w, err{message: message}, statusCode)
		return
	}

	errorResp := map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "models_error",
		},
	}

	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(errorResp)
}

// minimal error type for response handling
type err struct {
	message string
}

func (e err) Error() string {
	return e.message
}
