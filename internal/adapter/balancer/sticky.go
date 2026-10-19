package balancer

import (
	"context"
	"hash/fnv"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/constants"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/tidwall/gjson"
)

// StickyOutcome is the domain type; alias it here for callers that import
// this package directly (e.g. handler layer). The canonical definition lives in
// core/domain so that adapter/proxy/core can read it without a cycle.
type StickyOutcome = domain.StickyOutcome

// StickySessionWrapper is a decorator around any EndpointSelector that adds
// KV-cache affinity routing. It remembers which backend last handled a
// conversation (identified by a computed key) and steers subsequent turns
// to the same backend while it remains routable.
//
// TOCTOU on the session store is intentionally acceptable — both racing
// goroutines will select valid backends and the last writer wins, which
// converges quickly in practice.
type StickySessionWrapper struct {
	inner        domain.EndpointSelector
	store        *ttlcache.Cache[string, string]
	resultCounts map[string]uint64
	sourceCounts map[string]uint64
	cfg          config.StickySessionConfig

	// mu guards the outcome counters (results per outcome, per key source).
	// Select is the per-request hot path; the critical section is two map
	// increments, so a mutex is cheaper than per-source atomics here.
	mu sync.Mutex
}

// recordOutcome counts a routing decision for /internal/stats/sticky.
func (s *StickySessionWrapper) recordOutcome(result, source string) {
	s.mu.Lock()
	s.resultCounts[result]++
	s.sourceCounts[source]++
	s.mu.Unlock()
}

// NewStickySessionWrapper wraps inner with sticky session affinity using cfg.
// Call Start() after construction and Stop() on shutdown.
func NewStickySessionWrapper(inner domain.EndpointSelector, cfg config.StickySessionConfig) *StickySessionWrapper {
	idleTTL := time.Duration(cfg.IdleTTLSeconds) * time.Second

	if cfg.IdleTTLSeconds <= 0 {
		// ttlcache treats a zero TTL as no expiration — sessions accumulate until
		// capacity pressure forces eviction. Warn so operators notice the config.
		slog.Warn("sticky sessions TTL is zero — sessions will never expire by TTL")
	}

	store := ttlcache.New[string, string](
		ttlcache.WithTTL[string, string](idleTTL),
		ttlcache.WithCapacity[string, string](cfg.MaxSessions),
	)

	return &StickySessionWrapper{
		inner:        inner,
		store:        store,
		cfg:          cfg,
		resultCounts: make(map[string]uint64),
		sourceCounts: make(map[string]uint64),
	}
}

// Start launches the ttlcache background goroutine that handles TTL expiry and
// capacity-based eviction. Must be called before the wrapper is used for routing.
func (s *StickySessionWrapper) Start() {
	go s.store.Start()
}

// Stop shuts down the ttlcache background goroutine. Safe to call multiple times.
func (s *StickySessionWrapper) Stop() {
	s.store.Stop()
}

// Name returns a descriptive name that composes the inner balancer name.
func (s *StickySessionWrapper) Name() string {
	return "sticky(" + s.inner.Name() + ")"
}

// IncrementConnections delegates to the inner selector.
func (s *StickySessionWrapper) IncrementConnections(endpoint *domain.Endpoint) {
	s.inner.IncrementConnections(endpoint)
}

// DecrementConnections delegates to the inner selector.
func (s *StickySessionWrapper) DecrementConnections(endpoint *domain.Endpoint) {
	s.inner.DecrementConnections(endpoint)
}

// Select routes the request to the pinned backend when the affinity key is present
// and the backend is still routable. On a miss or dead-backend, it delegates to
// the inner selector and pins the newly chosen backend for next time.
func (s *StickySessionWrapper) Select(ctx context.Context, endpoints []*domain.Endpoint) (*domain.Endpoint, error) {
	outcome, _ := ctx.Value(constants.ContextStickyOutcomeKey).(*StickyOutcome)

	key, _ := ctx.Value(constants.ContextStickyKeyKey).(string)
	source, _ := ctx.Value(constants.ContextStickyKeySourceKey).(string)

	if key == "" {
		// No affinity key — pass through transparently. This wrapper only
		// exists when the feature is enabled, so "disabled" would be a lie:
		// the cascade ran and every source declined. Operators reading
		// "disabled" concluded sticky was off when it was the key that was
		// missing (an over-eager scan budget, a streaming path that never
		// buffered) — "no_key" states what actually happened.
		s.recordOutcome("no_key", "none")
		if outcome != nil {
			outcome.Result = "no_key"
			outcome.Source = "none"
		}
		return s.inner.Select(ctx, endpoints)
	}

	// ttlcache.Get refreshes the sliding TTL automatically.
	item := s.store.Get(key)
	if item != nil {
		pinnedURL := item.Value()
		for _, ep := range endpoints {
			if ep.Status.IsRoutable() && ep.URLString == pinnedURL {
				// Sticky hit — backend is still alive and serving this model.
				s.recordOutcome("hit", source)
				if outcome != nil {
					outcome.Result = "hit"
					outcome.Source = source
				}
				return ep, nil
			}
		}
	}

	chosen, err := s.inner.Select(ctx, endpoints)
	if err != nil {
		// Record before returning: an inner selector failure pins nothing, so
		// it IS a miss, and these balancer-side failures (selector errors, an
		// empty routable tier under a full outage) are exactly what the sticky
		// counters - the F1/F4 acceptance probe - were blind to.
		s.recordOutcome("miss", source)
		if outcome != nil {
			outcome.Result = "miss"
			outcome.Source = source
		}
		return nil, err
	}

	result := "miss"
	if item != nil {
		// Blip: the pinned backend is present (this candidate list is
		// per-model and registry-scoped) but currently non-routable. Ride
		// the turn out on the inner selector and KEEP the pin — no store.Set.
		// The old behaviour overwrote the entry with the surviving node, so
		// one 46s health blip migrated every conversation on that node and
		// none of them ever went home: a permanent, self-inflicted KV-cache
		// cold start on the whole fleet. With the pin kept, the conversation
		// returns to its warm node the moment it recovers.
		result = "bypass"
	} else {
		// First turn of a conversation — pin the chosen backend.
		s.store.Set(key, chosen.URLString, ttlcache.DefaultTTL)
	}

	s.recordOutcome(result, source)
	if outcome != nil {
		outcome.Result = result
		outcome.Source = source
	}

	return chosen, nil
}

// PurgeDeadEndpoints removes session entries that point to backends ABSENT
// from the provided endpoint set. The contract is REGISTRY membership, not
// health: callers must pass every registered endpoint (GetAll), healthy or
// not, so this evicts pins only when an endpoint leaves the fleet (config
// removal) - the one unambiguous case. Passing the routable subset used to
// delete every pin to a node the moment it flapped unhealthy, which -
// combined with Select's pin-overwrite on repin - turned a 46s blip into a
// fleet-wide KV cold start. Blips are now ridden out by Select (bypass).
// Pins to a permanently dead node age out through the idle TTL.
func (s *StickySessionWrapper) PurgeDeadEndpoints(registered []*domain.Endpoint) {
	alive := make(map[string]struct{}, len(registered))
	for _, ep := range registered {
		alive[ep.URLString] = struct{}{}
	}

	// Collect first, delete after: ttlcache's Range walks the LRU list with
	// `item = item.Next()` and stdlib container/list.Remove nils the removed
	// element's next/prev, so deleting INSIDE the callback ends the walk at the
	// first eviction (Range's own doc: "mutating the cache while ranging is not
	// supported"). One pin per call is exactly the defect that left every pin of
	// a retired endpoint alive for the whole idle TTL.
	var dead []string
	s.store.Range(func(item *ttlcache.Item[string, string]) bool {
		if _, ok := alive[item.Value()]; !ok {
			dead = append(dead, item.Key())
		}
		return true
	})
	for _, k := range dead {
		s.store.Delete(k)
	}
}

// ComputeStickyKey derives an affinity key for this request using the configured
// key_sources cascade. The key is model-scoped so the same client talking to
// different models does not cross-contaminate their session state.
//
// Sources are tried in the configured order: session_header,
// first_user_message, prefix_hash, auth_header, ip.
//
// Returns ("", "") when no source produces a usable key.
// Exported so handlers can compute the key before invoking the balancer.
//
// bodyTruncated reports that `body` is a scan SNAPSHOT cut short by
// proxy.sticky_sessions.key_scan_bytes, i.e. the conversation's first turn is
// not present. In that state the body-scanning sources are worse than useless:
// first_user_message yields nothing, and prefix_hash falls back to hashing the
// shared system-prompt prefix - the exact whole-agent flock this source was
// built to replace. Both are skipped, and the cascade then STOPS: the
// identity-wide sources (auth_header hashes the shared Authorization value, ip
// the shared client address) would pin every conversation of one credential to
// one node, silently and far coarser than prefix_hash. Returning no_key keeps
// the request routable (bypass: plain balancer selection) and visible in the
// sticky_source=no_key counter, which is how an operator learns the scan budget
// is too small.
func ComputeStickyKey(r *http.Request, modelName string, cfg config.StickySessionConfig, body []byte, bodyTruncated bool) (key, source string) {
	bodyScanTruncated := false

	for _, src := range cfg.KeySources {
		var k, s string
		switch src {
		case "session_header":
			k, s = stickyKeyFromSessionHeader(r, modelName)
		case "prefix_hash":
			if bodyTruncated {
				bodyScanTruncated = true
				continue
			}
			k, s = stickyKeyFromPrefixHash(body, modelName, cfg.PrefixHashBytes)
		case "first_user_message":
			if bodyTruncated {
				bodyScanTruncated = true
				continue
			}
			k, s = stickyKeyFromFirstTurn(body, modelName)
		case "auth_header":
			if bodyScanTruncated {
				continue
			}
			k, s = stickyKeyFromAuthHeader(r, modelName)
		case "ip":
			if bodyScanTruncated {
				continue
			}
			k, s = stickyKeyFromIP(r, modelName)
		}
		if k != "" {
			return k, s
		}
	}

	return "", ""
}

// stickyKeyFromSessionHeader hashes the session ID header with FNV-64a so that
// unbounded client-supplied strings do not inflate cache key memory.
func stickyKeyFromSessionHeader(r *http.Request, modelName string) (string, string) {
	v := r.Header.Get(constants.HeaderXOllaSessionID)
	if v == "" {
		return "", ""
	}
	h := fnv.New64a()
	h.Write([]byte(v))
	return uint64ToHex(h.Sum64()) + ":" + modelName, "session_header"
}

// stickyKeyFromPrefixHash hashes the first prefixBytes bytes of the messages
// JSON array so requests with identical conversation prefixes are routed together.
// Falls back to the legacy completions "prompt" field so non-chat endpoints
// (e.g. /v1/completions, llamaswap-style passthroughs) also produce a key.
func stickyKeyFromPrefixHash(body []byte, modelName string, prefixBytes int) (string, string) {
	if len(body) == 0 {
		return "", ""
	}
	raw := gjson.GetBytes(body, "messages").Raw
	if raw == "" || raw == "[]" || raw == "null" {
		raw = gjson.GetBytes(body, "prompt").Raw
	}
	if raw == "" {
		return "", ""
	}
	limit := prefixBytes
	if limit <= 0 || limit > len(raw) {
		limit = len(raw)
	}
	h := fnv.New64a()
	h.Write([]byte(raw[:limit]))
	return strings.ReplaceAll(modelName, ":", "_") + ":" + uint64ToHex(h.Sum64()), "prefix_hash"
}

// stickyKeyFromFirstTurn hashes the first non-system turn of the conversation
// (OpenAI chat "messages"; the raw "prompt" for completions-style bodies).
//
// Why this exists: prefix_hash keys the first prefix_hash_bytes bytes of the
// messages array, which for agent clients is the shared system prompt. Every
// conversation from one agent therefore produces the SAME key, so the whole
// agent - hundreds of conversations - is pinned to one node while the sliding
// TTL keeps refreshing on each hit, and the fleet migrates as a flock when the
// entry finally dies. Measured 2026-10-19 on the live gateway: 10191 sticky
// hits over 12 active sessions.
//
// The first user turn is unique per conversation and stable across every later
// turn of it (chat-completions clients resend the history verbatim), so this
// source pins one conversation to one node - the KV-warm reuse prefix_hash was
// meant to provide, at conversation granularity instead of prompt-template
// granularity. Requires the handler to have buffered past the system prompt:
// see proxy.sticky_sessions.key_scan_bytes.
func stickyKeyFromFirstTurn(body []byte, modelName string) (string, string) {
	if len(body) == 0 {
		return "", ""
	}

	// The key folds the system prompt fingerprint into the first user turn:
	// hashing the turn ALONE collides on identical openers - "hi", "continue",
	// a fixed job prompt - and every conversation that opens with the same
	// text on the same model shares a key (and a node). A fleet running
	// fixed-opener consumers (the wa-bot class) re-flocks exactly like the
	// prefix_hash design this source replaced. Two agents that share a system
	// prompt AND an opener remain one pin: from the gateway's view they are
	// byte-identical requests, and no key can separate them.
	const sysFingerprintBytes = 2048
	var first, system string
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		msgs.ForEach(func(_, turn gjson.Result) bool {
			if turn.Get("role").String() == "system" {
				if system == "" {
					system = turn.Get("content").Raw
				}
				return true
			}
			content := turn.Get("content").Raw
			if content == "" || content == "null" {
				return true
			}
			first = content
			return false
		})
	}
	if first == "" {
		// Completions-style body (/v1/completions and passthroughs): the prompt
		// itself is the conversation's opening turn.
		first = gjson.GetBytes(body, "prompt").Raw
	}
	if first == "" {
		return "", ""
	}

	if len(system) > sysFingerprintBytes {
		system = system[:sysFingerprintBytes]
	}
	h := fnv.New64a()
	h.Write([]byte(system))
	h.Write([]byte{0x1f}) // unit separator: system||turn must not equal turn of a body with the bytes merged
	h.Write([]byte(first))
	return "turn:" + strings.ReplaceAll(modelName, ":", "_") + ":" + uint64ToHex(h.Sum64()), "first_user_message"
}

// stickyKeyFromAuthHeader hashes the Authorization header value so that tokens
// are never stored in plaintext inside the session store.
func stickyKeyFromAuthHeader(r *http.Request, modelName string) (string, string) {
	v := r.Header.Get("Authorization")
	if v == "" {
		return "", ""
	}
	h := fnv.New64a()
	h.Write([]byte(v))
	return "auth:" + uint64ToHex(h.Sum64()) + ":" + modelName, "auth_header"
}

// stickyKeyFromIP extracts the remote host using net.SplitHostPort, which
// handles bracketed IPv6 addresses correctly (strings.LastIndex cannot).
func stickyKeyFromIP(r *http.Request, modelName string) (string, string) {
	addr := r.RemoteAddr
	if addr == "" {
		return "", ""
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Bare address with no port — use as-is.
		host = addr
	}
	if host == "" {
		return "", ""
	}
	return "ip:" + host + ":" + modelName, "ip"
}

// StickyStats holds a point-in-time snapshot of sticky session activity.
type StickyStats struct {

	// Results counts Select outcomes by label (hit/miss/bypass/no_key) and
	// Sources by producing key source; both are wrapper-level, unlike the
	// ttlcache hits/misses above which are store lookups. SessionsByEndpoint
	// maps pinned backend URL -> live session count: the per-node answer
	// ("is the fast tier even right now") that the aggregate counters cannot
	// give.
	Results            map[string]uint64 `json:"results"`
	Sources            map[string]uint64 `json:"key_sources"`
	SessionsByEndpoint map[string]int    `json:"sessions_by_endpoint"`
	ActiveSessions     int               `json:"active_sessions"`
	Insertions         uint64            `json:"insertions"`
	Hits               uint64            `json:"hits"`
	Misses             uint64            `json:"misses"`
	Evictions          uint64            `json:"evictions"`
	MaxSessions        uint64            `json:"max_sessions"`
	IdleTTLSeconds     int               `json:"idle_ttl_seconds"`

	Enabled bool `json:"enabled"`
}

// Stats returns a point-in-time snapshot of the session store metrics.
func (s *StickySessionWrapper) Stats() StickyStats {
	m := s.store.Metrics()

	s.mu.Lock()
	results := make(map[string]uint64, len(s.resultCounts))
	for k, v := range s.resultCounts {
		results[k] = v
	}
	sources := make(map[string]uint64, len(s.sourceCounts))
	for k, v := range s.sourceCounts {
		sources[k] = v
	}
	s.mu.Unlock()

	byEndpoint := make(map[string]int)
	s.store.Range(func(item *ttlcache.Item[string, string]) bool {
		byEndpoint[item.Value()]++
		return true
	})

	return StickyStats{
		Enabled:            true,
		ActiveSessions:     s.store.Len(),
		Insertions:         m.Insertions,
		Hits:               m.Hits,
		Misses:             m.Misses,
		Evictions:          m.Evictions,
		MaxSessions:        s.cfg.MaxSessions,
		IdleTTLSeconds:     s.cfg.IdleTTLSeconds,
		Results:            results,
		Sources:            sources,
		SessionsByEndpoint: byEndpoint,
	}
}

// uint64ToHex converts a uint64 to a hex string without importing fmt (avoids allocation).
func uint64ToHex(v uint64) string {
	const digits = "0123456789abcdef"
	buf := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf)
}
