package main

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
)

// Mode describes how ollamock should behave when processing requests.
type Mode string

const (
	// ModeOK is normal operation — all requests succeed.
	ModeOK Mode = "ok"
	// ModeError causes all requests to return a fixed error status.
	ModeError Mode = "error"
	// ModeFlaky returns errors at a configurable rate so tests can exercise
	// retry and circuit-breaker logic.
	ModeFlaky Mode = "flaky"
	// ModeHang blocks requests indefinitely (until context cancellation) to
	// simulate a hung backend.
	ModeHang Mode = "hang"
	// ModeSlow adds a fixed latency to every response.
	ModeSlow Mode = "slow"
)

// validModes is the closed set of accepted mode strings.
var validModes = map[Mode]bool{
	ModeOK:    true,
	ModeError: true,
	ModeFlaky: true,
	ModeHang:  true,
	ModeSlow:  true,
}

// Behaviour captures the current fault-injection configuration.
// Field order is largest-to-smallest for alignment.
type Behaviour struct {
	Mode          Mode    `json:"mode"`
	ErrorRate     float64 `json:"error_rate"`
	LatencyMS     int     `json:"latency_ms"`
	ErrorStatus   int     `json:"error_status"`
	FailHealth    bool    `json:"fail_health"`
	DropMidStream bool    `json:"drop_mid_stream"`
	MalformedJSON bool    `json:"malformed_json"`
}

func defaultBehaviour() Behaviour {
	return Behaviour{
		Mode:        ModeOK,
		ErrorStatus: 500,
		ErrorRate:   0.5,
	}
}

// behaviourState holds the mutable behaviour alongside a seeded RNG for flaky
// mode. Both are protected by the same mutex so reads and writes are always
// consistent — the rand source is not goroutine-safe on its own.
type behaviourState struct {
	rng  *rand.Rand
	b    Behaviour
	seed int64
	mu   sync.RWMutex
}

func newBehaviourState(seed int64) *behaviourState {
	return &behaviourState{
		b:    defaultBehaviour(),
		rng:  rand.New(rand.NewSource(seed)), //nolint:gosec // deterministic seed for reproducible test scenarios
		seed: seed,
	}
}

func (s *behaviourState) get() Behaviour {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.b
}

// merge applies a partial JSON patch into the current behaviour.
// Only fields present in the patch are updated; others retain their value.
func (s *behaviourState) merge(patch Behaviour, hasPatch patchFields) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if hasPatch.mode {
		if !validModes[patch.Mode] {
			return errUnknownMode
		}
		s.b.Mode = patch.Mode
	}
	if hasPatch.errorStatus {
		s.b.ErrorStatus = patch.ErrorStatus
	}
	if hasPatch.errorRate {
		s.b.ErrorRate = patch.ErrorRate
	}
	if hasPatch.latencyMS {
		s.b.LatencyMS = patch.LatencyMS
	}
	if hasPatch.failHealth {
		s.b.FailHealth = patch.FailHealth
	}
	if hasPatch.dropMidStream {
		s.b.DropMidStream = patch.DropMidStream
	}
	if hasPatch.malformedJSON {
		s.b.MalformedJSON = patch.MalformedJSON
	}
	return nil
}

func (s *behaviourState) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = defaultBehaviour()
	// Re-seed with the original seed so behaviour after reset is reproducible.
	s.rng = rand.New(rand.NewSource(s.seed)) //nolint:gosec
}

// float64 returns a random float64 in [0,1). Caller must not hold the mutex.
func (s *behaviourState) float64() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.Float64()
}

// patchFields tracks which JSON fields were explicitly set in a PATCH body.
// This lets us distinguish "error_rate: 0" (explicit zero) from absent field.
type patchFields struct {
	mode          bool
	errorStatus   bool
	errorRate     bool
	latencyMS     bool
	failHealth    bool
	dropMidStream bool
	malformedJSON bool
}

// parsePatch decodes a partial behaviour JSON and records which fields were present.
func parsePatch(data []byte) (Behaviour, patchFields, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Behaviour{}, patchFields{}, err
	}

	var b Behaviour
	var pf patchFields

	if v, ok := raw["mode"]; ok {
		if err := json.Unmarshal(v, &b.Mode); err != nil {
			return b, pf, err
		}
		pf.mode = true
	}
	if v, ok := raw["error_status"]; ok {
		if err := json.Unmarshal(v, &b.ErrorStatus); err != nil {
			return b, pf, err
		}
		pf.errorStatus = true
	}
	if v, ok := raw["error_rate"]; ok {
		if err := json.Unmarshal(v, &b.ErrorRate); err != nil {
			return b, pf, err
		}
		pf.errorRate = true
	}
	if v, ok := raw["latency_ms"]; ok {
		if err := json.Unmarshal(v, &b.LatencyMS); err != nil {
			return b, pf, err
		}
		pf.latencyMS = true
	}
	if v, ok := raw["fail_health"]; ok {
		if err := json.Unmarshal(v, &b.FailHealth); err != nil {
			return b, pf, err
		}
		pf.failHealth = true
	}
	if v, ok := raw["drop_mid_stream"]; ok {
		if err := json.Unmarshal(v, &b.DropMidStream); err != nil {
			return b, pf, err
		}
		pf.dropMidStream = true
	}
	if v, ok := raw["malformed_json"]; ok {
		if err := json.Unmarshal(v, &b.MalformedJSON); err != nil {
			return b, pf, err
		}
		pf.malformedJSON = true
	}

	return b, pf, nil
}

// statsStore tracks per-path request counts using atomic int64s inside a
// sync.Map so hot paths never contend on a single lock.
type statsStore struct {
	m sync.Map // map[string]*atomic.Int64
}

func (s *statsStore) inc(path string) {
	v, _ := s.m.LoadOrStore(path, &atomic.Int64{})
	if counter, ok := v.(*atomic.Int64); ok {
		counter.Add(1)
	}
}

func (s *statsStore) snapshot() (int64, map[string]int64) {
	byPath := make(map[string]int64)
	var total int64
	s.m.Range(func(k, v any) bool {
		counter, ok := v.(*atomic.Int64)
		if !ok {
			return true
		}
		key, ok := k.(string)
		if !ok {
			return true
		}
		n := counter.Load()
		byPath[key] = n
		total += n
		return true
	})
	return total, byPath
}

func (s *statsStore) reset() {
	s.m.Range(func(_, v any) bool {
		if counter, ok := v.(*atomic.Int64); ok {
			counter.Store(0)
		}
		return true
	})
}

// controlHandlers registers the /_mock/* control plane routes onto mux.
// Control routes are immune to behaviour — they always respond normally.
func (srv *mockServer) controlHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /_mock/behaviour", srv.handleGetBehaviour)
	mux.HandleFunc("POST /_mock/behaviour", srv.handlePostBehaviour)
	mux.HandleFunc("POST /_mock/reset", srv.handleReset)
	mux.HandleFunc("GET /_mock/stats", srv.handleStats)
}

func (srv *mockServer) handleGetBehaviour(w http.ResponseWriter, _ *http.Request) {
	b := srv.bstate.get()
	writeJSON(w, http.StatusOK, b)
}

func (srv *mockServer) handlePostBehaviour(w http.ResponseWriter, r *http.Request) {
	var body [4096]byte
	n, _ := r.Body.Read(body[:])
	_ = r.Body.Close()

	patch, pf, err := parsePatch(body[:n])
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := srv.bstate.merge(patch, pf); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, srv.bstate.get())
}

func (srv *mockServer) handleReset(w http.ResponseWriter, _ *http.Request) {
	srv.bstate.reset()
	srv.stats.reset()
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

func (srv *mockServer) handleStats(w http.ResponseWriter, _ *http.Request) {
	total, byPath := srv.stats.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   total,
		"by_path": byPath,
	})
}
