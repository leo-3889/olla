package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/domain"
)

// first_user_message keys affinity on the conversation, not on the prompt
// template. Agents send an identical (multi-KB) system prompt on every
// conversation, so a prefix-keyed cascade pins the whole agent to one node;
// the first user turn is unique per conversation and resent verbatim on every
// later turn, which is exactly the affinity unit KV reuse wants.

func firstTurnConfig(sources ...string) config.StickySessionConfig {
	cfg := defaultStickyConfig()
	cfg.KeySources = sources
	cfg.KeyScanBytes = 65536
	return cfg
}

// agentBody builds an OpenAI chat body with a large shared system prompt, so a
// 512-byte prefix hash sees only system-prompt bytes.
func agentBody(t *testing.T, systemPrompt string, turns ...map[string]string) []byte {
	t.Helper()

	messages := []map[string]string{{"role": "system", "content": systemPrompt}}
	messages = append(messages, turns...)
	body, err := json.Marshal(map[string]any{"model": "qwen4exp", "messages": messages})
	require.NoError(t, err)
	return body
}

func TestStickyKey_FirstTurnSeparatesConversationsThatShareASystemPrompt(t *testing.T) {
	t.Parallel()

	// ~4 KiB of identical system prompt: the shape every agent client sends.
	shared := strings.Repeat("You are a coding agent. ", 150)
	require.Greater(t, len(shared), 512, "fixture must exceed prefix_hash_bytes so the prefix collides")

	conversationA := agentBody(t, shared, map[string]string{"role": "user", "content": "fix the flaky test in billing.py"})
	conversationB := agentBody(t, shared, map[string]string{"role": "user", "content": "explain the migration lock in schema.sql"})
	req, _ := http.NewRequest(http.MethodPost, "/", nil)

	prefixCfg := firstTurnConfig("prefix_hash")
	keyA, _ := ComputeStickyKey(req, "qwen4exp", prefixCfg, conversationA)
	keyB, _ := ComputeStickyKey(req, "qwen4exp", prefixCfg, conversationB)
	assert.Equal(t, keyA, keyB, "prefix_hash keys the shared system prompt (the flocking this source replaces)")

	turnCfg := firstTurnConfig("first_user_message", "prefix_hash")
	keyA2, sourceA := ComputeStickyKey(req, "qwen4exp", turnCfg, conversationA)
	keyB2, sourceB := ComputeStickyKey(req, "qwen4exp", turnCfg, conversationB)
	assert.Equal(t, "first_user_message", sourceA)
	assert.Equal(t, "first_user_message", sourceB)
	assert.NotEqual(t, keyA2, keyB2, "two conversations must not share an affinity key")
	assert.True(t, strings.HasPrefix(keyA2, "turn:qwen4exp:"), "key must stay model-scoped, got %s", keyA2)
}

func TestStickyKey_FirstTurnIsStableAcrossLaterTurns(t *testing.T) {
	t.Parallel()

	shared := strings.Repeat("system prompt bytes. ", 200)
	first := map[string]string{"role": "user", "content": "first question"}
	turn1 := agentBody(t, shared, first)
	turn2 := agentBody(t, shared,
		first,
		map[string]string{"role": "assistant", "content": "an answer"},
		map[string]string{"role": "user", "content": "and now a follow-up that changes the whole tail of the body"},
	)
	require.Greater(t, len(turn2), len(turn1))

	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	cfg := firstTurnConfig("first_user_message", "prefix_hash")

	key1, _ := ComputeStickyKey(req, "qwen4exp", cfg, turn1)
	key2, _ := ComputeStickyKey(req, "qwen4exp", cfg, turn2)
	assert.Equal(t, key1, key2, "a conversation must keep its pin as turns are appended")
}

func TestStickyKey_FirstTurnPinsTheConversationToOneBackend(t *testing.T) {
	t.Parallel()

	w := makeWrapper(t, firstTurnConfig("first_user_message", "prefix_hash"))
	a := makeEndpoint("a", "http://backend-a:8000")
	b := makeEndpoint("b", "http://backend-b:8000")
	endpoints := []*domain.Endpoint{a, b}
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	shared := strings.Repeat("system. ", 300)

	// 20 conversations: each first turn must land on the next node in the
	// rotation (even queue), and every later turn of that conversation must hit
	// the same node (pinning).
	byEndpoint := map[string]int{}
	for i := range 20 {
		first := map[string]string{"role": "user", "content": fmt.Sprintf("conversation %d opens here", i)}
		body := agentBody(t, shared, first)

		key, source := ComputeStickyKey(req, "qwen4exp", w.cfg, body)
		require.Equal(t, "first_user_message", source)
		ctx, _ := injectKey(context.Background(), key, source)
		chosen, err := w.Select(ctx, endpoints)
		require.NoError(t, err)
		byEndpoint[chosen.Name]++

		for turn := range 5 {
			grown := agentBody(t, shared, first,
				map[string]string{"role": "assistant", "content": fmt.Sprintf("answer %d", turn)},
				map[string]string{"role": "user", "content": fmt.Sprintf("follow-up %d", turn)},
			)
			grownKey, grownSource := ComputeStickyKey(req, "qwen4exp", w.cfg, grown)
			require.Equal(t, source, grownSource)
			grownCtx, grownOutcome := injectKey(context.Background(), grownKey, grownSource)
			again, err := w.Select(grownCtx, endpoints)
			require.NoError(t, err)
			assert.Equal(t, chosen.URLString, again.URLString, "turn %d of conversation %d left its pin", turn, i)
			assert.Equal(t, "hit", grownOutcome.Result)
		}
	}

	assert.Equal(t, 10, byEndpoint["a"], "new conversations must spread evenly across the twins")
	assert.Equal(t, 10, byEndpoint["b"], "new conversations must spread evenly across the twins")
}

func TestStickyKey_FirstTurnFallsBackToPrefixHashWithoutAUserTurn(t *testing.T) {
	t.Parallel()

	// A system-only body (e.g. a probe) has no conversation to key on.
	body := agentBody(t, "only a system prompt here")
	req, _ := http.NewRequest(http.MethodPost, "/", nil)

	key, source := ComputeStickyKey(req, "qwen4exp", firstTurnConfig("first_user_message", "prefix_hash"), body)
	assert.Equal(t, "prefix_hash", source)
	assert.NotEmpty(t, key)
}

func TestStickyKey_FirstTurnHandlesCompletionsPrompt(t *testing.T) {
	t.Parallel()

	cfg := firstTurnConfig("first_user_message")
	req, _ := http.NewRequest(http.MethodPost, "/", nil)

	key1, source := ComputeStickyKey(req, "qwen4exp", cfg, []byte(`{"model":"qwen4exp","prompt":"once upon a time"}`))
	assert.Equal(t, "first_user_message", source)
	key2, _ := ComputeStickyKey(req, "qwen4exp", cfg, []byte(`{"model":"qwen4exp","prompt":"once upon a time"}`))
	key3, _ := ComputeStickyKey(req, "qwen4exp", cfg, []byte(`{"model":"qwen4exp","prompt":"a different story"}`))
	assert.Equal(t, key1, key2)
	assert.NotEqual(t, key1, key3)
}

func TestStickyKey_FirstTurnHandlesMultimodalContent(t *testing.T) {
	t.Parallel()

	cfg := firstTurnConfig("first_user_message")
	req, _ := http.NewRequest(http.MethodPost, "/", nil)

	body := []byte(`{"model":"qwen4exp","messages":[{"role":"system","content":"sys"},{"role":"user","content":[{"type":"text","text":"what is in this image"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	key, source := ComputeStickyKey(req, "qwen4exp", cfg, body)
	assert.Equal(t, "first_user_message", source)

	// The same conversation with a different image must move to a new pin, and
	// the key must be stable for an identical body.
	other := strings.Replace(string(body), "AAAA", "BBBB", 1)
	keyOther, _ := ComputeStickyKey(req, "qwen4exp", cfg, []byte(other))
	keySame, _ := ComputeStickyKey(req, "qwen4exp", cfg, body)
	assert.Equal(t, key, keySame)
	assert.NotEqual(t, key, keyOther)
}

func TestStickyKey_FirstTurnSkippedWhenBodyNotBuffered(t *testing.T) {
	t.Parallel()

	// No body at all (streaming path that never buffered): the source must
	// contribute nothing rather than fabricate a key, so the cascade continues.
	cfg := firstTurnConfig("first_user_message", "auth_header")
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer key-1")

	key, source := ComputeStickyKey(req, "qwen4exp", cfg, nil)
	assert.Equal(t, "auth_header", source)
	assert.Contains(t, key, "auth:")
}
