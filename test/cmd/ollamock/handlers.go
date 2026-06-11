package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"time"
)

// errUnknownMode is returned when a PATCH body sets an unrecognised mode.
var errUnknownMode = errors.New("unknown mode")

// serverConfig holds static configuration set at startup.
// Field order largest-to-smallest for alignment.
type serverConfig struct {
	name         string
	models       []string
	ttftMS       int
	tps          int
	streamChunks int
}

// mockServer is the central state container wired into the HTTP mux.
type mockServer struct {
	bstate *behaviourState
	stats  statsStore
	cfg    serverConfig
}

func newServer(cfg serverConfig) *mockServer {
	// Seed from FNV-1a hash of the instance name so behaviour is deterministic
	// per-instance but distinct across instances in a multi-node test fleet.
	h := fnv.New64a()
	_, _ = h.Write([]byte(cfg.name))
	seed := int64(h.Sum64())

	return &mockServer{
		cfg:    cfg,
		bstate: newBehaviourState(seed),
	}
}

// handler builds and returns the ServeMux for this server instance.
func (srv *mockServer) handler() http.Handler {
	mux := http.NewServeMux()

	// Control plane - always healthy, immune to behaviour.
	srv.controlHandlers(mux)

	// LLM protocol routes.
	mux.HandleFunc("GET /health", srv.wrap(srv.handleHealth))
	mux.HandleFunc("GET /", srv.wrap(srv.handleRoot))

	// Model listing endpoints - one per protocol.
	mux.HandleFunc("GET /v1/models", srv.wrap(srv.handleOpenAIModels))
	mux.HandleFunc("GET /api/tags", srv.wrap(srv.handleOllamaTags))
	mux.HandleFunc("GET /api/version", srv.wrap(srv.handleOllamaVersion))
	mux.HandleFunc("GET /api/v0/models", srv.wrap(srv.handleLMStudioModels))
	mux.HandleFunc("GET /api/v1/models", srv.wrap(srv.handleLemonadeModels))

	// Inference endpoints - streaming handled in streaming.go.
	mux.HandleFunc("POST /api/chat", srv.wrap(srv.handleOllamaChat))
	mux.HandleFunc("POST /api/generate", srv.wrap(srv.handleOllamaGenerate))
	mux.HandleFunc("POST /v1/chat/completions", srv.wrap(srv.handleOpenAIChat))
	mux.HandleFunc("POST /v1/completions", srv.wrap(srv.handleOpenAICompletion))
	mux.HandleFunc("POST /api/v1/chat/completions", srv.wrap(srv.handleOpenAIChat))
	mux.HandleFunc("POST /v1/messages", srv.wrap(srv.handleAnthropicMessages))

	return mux
}

// wrap returns a handler that records stats, sets the instance header, and
// applies the current behaviour before delegating to the inner handler.
func (srv *mockServer) wrap(inner http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		srv.stats.inc(r.URL.Path)
		w.Header().Set("X-Ollamock-Instance", srv.cfg.name)
		if !srv.applyBehaviour(w, r) {
			return
		}
		inner(w, r)
	}
}

// applyBehaviour enforces the current Mode, returning false when the request
// should be short-circuited (response already written).
func (srv *mockServer) applyBehaviour(w http.ResponseWriter, r *http.Request) bool {
	b := srv.bstate.get()

	isHealthPath := r.URL.Path == "/health" || r.URL.Path == "/"

	// fail_health only gates the health endpoints - all other routes are unaffected.
	if b.FailHealth && isHealthPath {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `{"status":"unhealthy"}`)
		return false
	}

	switch b.Mode {
	case ModeOK:
		// No-op - proceed to handler.

	case ModeHang:
		// Block until the client disconnects or context is cancelled.
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Minute):
		}
		return false

	case ModeError:
		writeErrorResponse(w, b.ErrorStatus)
		return false

	case ModeFlaky:
		if srv.bstate.float64() < b.ErrorRate {
			writeErrorResponse(w, b.ErrorStatus)
			return false
		}

	case ModeSlow:
		if b.LatencyMS > 0 {
			select {
			case <-r.Context().Done():
				return false
			case <-time.After(time.Duration(b.LatencyMS) * time.Millisecond):
			}
		}
	}

	if b.MalformedJSON {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"broken":`)
		return false
	}

	return true
}

func writeErrorResponse(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"message":"mock error","type":"mock_error"}}`)
}

// writeJSON serialises v as JSON and writes it with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- Health / liveness ---

func (srv *mockServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (srv *mockServer) handleRoot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "Ollama is running")
}

// --- Ollama protocol ---

func (srv *mockServer) handleOllamaTags(w http.ResponseWriter, _ *http.Request) {
	type details struct {
		Family            string `json:"family"`
		ParameterSize     string `json:"parameter_size"`
		QuantizationLevel string `json:"quantization_level"`
	}
	type model struct {
		Details    details `json:"details"`
		ModifiedAt string  `json:"modified_at"`
		Digest     string  `json:"digest"`
		Name       string  `json:"name"`
		Model      string  `json:"model"`
		Size       int64   `json:"size"`
	}
	type response struct {
		Models []model `json:"models"`
	}

	now := time.Now().UTC().Format(time.RFC3339)
	models := make([]model, len(srv.cfg.models))
	for i, m := range srv.cfg.models {
		models[i] = model{
			Name:       m,
			Model:      m,
			ModifiedAt: now,
			Size:       4_000_000_000,
			Digest:     fmt.Sprintf("sha256:ollamock%s%s", srv.cfg.name, m),
			Details: details{
				Family:            "llama",
				ParameterSize:     "7B",
				QuantizationLevel: "Q4_0",
			},
		}
	}
	writeJSON(w, http.StatusOK, response{Models: models})
}

func (srv *mockServer) handleOllamaVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": "0.6.0-ollamock"})
}

// --- LM Studio protocol ---

func (srv *mockServer) handleLMStudioModels(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		Type             string `json:"type"`
		Publisher        string `json:"publisher"`
		Arch             string `json:"arch"`
		State            string `json:"state"`
		ID               string `json:"id"`
		Object           string `json:"object"`
		MaxContextLength int64  `json:"max_context_length"`
	}
	type response struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}

	models := make([]model, len(srv.cfg.models))
	for i, m := range srv.cfg.models {
		models[i] = model{
			ID:               m,
			Object:           "model",
			Type:             "llm",
			Publisher:        "ollamock",
			Arch:             "llama",
			State:            "loaded",
			MaxContextLength: 4096,
		}
	}
	writeJSON(w, http.StatusOK, response{Object: "list", Data: models})
}

// --- OpenAI-compatible protocol ---

func (srv *mockServer) handleOpenAIModels(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		OwnedBy string `json:"owned_by"`
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
	}
	type response struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}

	now := time.Now().Unix()
	models := make([]model, len(srv.cfg.models))
	for i, m := range srv.cfg.models {
		models[i] = model{
			ID:      m,
			Object:  "model",
			Created: now,
			OwnedBy: "ollamock",
		}
	}
	writeJSON(w, http.StatusOK, response{Object: "list", Data: models})
}

// --- Lemonade protocol ---

func (srv *mockServer) handleLemonadeModels(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		ID         string `json:"id"`
		Object     string `json:"object"`
		OwnedBy    string `json:"owned_by"`
		Checkpoint string `json:"checkpoint"`
		Recipe     string `json:"recipe"`
		Created    int64  `json:"created"`
		Downloaded bool   `json:"downloaded"`
	}
	type response struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}

	now := time.Now().Unix()
	models := make([]model, len(srv.cfg.models))
	for i, m := range srv.cfg.models {
		models[i] = model{
			ID:         m,
			Object:     "model",
			OwnedBy:    "lemonade",
			Checkpoint: fmt.Sprintf("amd/%s", m),
			Recipe:     "oga-cpu",
			Created:    now,
			Downloaded: true,
		}
	}
	writeJSON(w, http.StatusOK, response{Object: "list", Data: models})
}

// contextDone returns a closed channel when ctx is already done, otherwise the
// ctx.Done() channel. Used in select statements that must compile on Go 1.24.
func contextDone(ctx context.Context) <-chan struct{} {
	return ctx.Done()
}
