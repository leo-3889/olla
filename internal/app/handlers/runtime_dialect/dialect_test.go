package runtime_dialect

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func f(v float64) *float64 { return &v }

func TestSaturationSelectPerKind(t *testing.T) {
	// vLLM publishes the scheduler queue and the running gauge (V1 colon
	// spelling, underscore spelling for pre-V1 builds).
	c := Select("vllm", map[string]float64{
		"vllm:num_requests_running": 4,
		"vllm:num_requests_waiting": 2,
	})
	assert.Equal(t, f(4), c.RequestsRunning)
	assert.Equal(t, f(2), c.QueueDepth)
	c = Select("vllm", map[string]float64{
		"vllm_num_requests_running": 1,
		"vllm_num_requests_waiting": 9,
	})
	assert.Equal(t, f(1), c.RequestsRunning)
	assert.Equal(t, f(9), c.QueueDepth)

	// Radiance exposes its running gauge and its own HTTP queue gauge.
	c = Select("radiance", map[string]float64{
		"vllm:num_requests_running": 6,
		"radiance:http_queue_depth": 3,
	})
	assert.Equal(t, f(6), c.RequestsRunning)
	assert.Equal(t, f(3), c.QueueDepth)

	// llama.cpp reports running requests; it has no scheduler queue, so
	// QueueDepth stays nil (honest absence -> a dash, never a zero).
	c = Select("llamacpp", map[string]float64{
		"llamacpp:requests_running": 5,
	})
	assert.Equal(t, f(5), c.RequestsRunning)
	assert.Nil(t, c.QueueDepth)
	c = Select("llamacpp", map[string]float64{
		"llamacpp_requests_running": 2,
	})
	assert.Equal(t, f(2), c.RequestsRunning)
}

func TestRunningAndWaitingColumnsInDisplayOrder(t *testing.T) {
	// Radiance-capable fleet: the queue column is relabelled Waiting and
	// Running renders as its own sibling column immediately before it.
	cols := UnionColumns([]string{"radiance"})
	require.Len(t, cols, 5)
	assert.Equal(t, []string{"generation_tps", "prefill_tps", "cache_hit_percent", "requests_running", "queue_depth"},
		[]string{cols[0].Key, cols[1].Key, cols[2].Key, cols[3].Key, cols[4].Key})
	assert.Equal(t, "Running", cols[3].Label)
	assert.Equal(t, UnitCount, cols[3].Unit)
	assert.False(t, cols[3].Windowed)
	assert.Equal(t, "Waiting", cols[4].Label)
	assert.Equal(t, UnitCount, cols[4].Unit)

	// Mixed fleet: every dialect declares Running before Waiting, and the
	// first declarer owns the label (llamacpp sorts first).
	cols = UnionColumns([]string{"radiance", "llamacpp", "vllm"})
	require.Len(t, cols, 5)
	assert.Equal(t, "requests_running", cols[3].Key)
	assert.Equal(t, "Running", cols[3].Label)
	assert.Equal(t, "queue_depth", cols[4].Key)
	assert.Equal(t, "Waiting", cols[4].Label)
	// S8: every dialect column carries header help (window/source/aggregate).
	for _, c := range cols {
		assert.NotEmpty(t, c.Help, "column %s must ship help text", c.Key)
	}

	// A llama.cpp-only fleet honestly has no Waiting column.
	cols = UnionColumns([]string{"llamacpp"})
	keys := make([]string, 0, len(cols))
	for _, c := range cols {
		keys = append(keys, c.Key)
	}
	assert.Equal(t, []string{"generation_tps", "prefill_tps", "cache_hit_percent", "requests_running"}, keys)
}
