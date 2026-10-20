package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProxyConfig_Validate_RetryOnStatus pins the boot validation for
// proxy.retry_on_status: only 4xx/5xx backend refusal codes may arm endpoint
// failover. A 2xx/3xx entry describes nothing to recover from; 0/negative/
// 6xx+ entries can never match a committed response status and would make
// the feature silently inert.
func TestProxyConfig_Validate_RetryOnStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		codes   []int
		wantErr string
	}{
		{name: "empty is valid (feature off)", codes: nil},
		{name: "empty slice is valid", codes: []int{}},
		{name: "canonical 429/503 accepted", codes: []int{429, 503}},
		{name: "400 boundary accepted", codes: []int{400}},
		{name: "599 boundary accepted", codes: []int{599}},
		{name: "200 rejected", codes: []int{200}, wantErr: "retry_on_status"},
		{name: "399 rejected", codes: []int{399}, wantErr: "retry_on_status"},
		{name: "600 rejected", codes: []int{600}, wantErr: "retry_on_status"},
		{name: "zero rejected", codes: []int{0}, wantErr: "retry_on_status"},
		{name: "negative rejected", codes: []int{-1}, wantErr: "retry_on_status"},
		{name: "bad entry inside a good list rejected", codes: []int{429, 1}, wantErr: "retry_on_status"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ProxyConfig{RetryOnStatus: tc.codes}.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestConfig_Validate_RejectsNonRefusalRetryOnStatus proves the key is a boot
// ERROR through the full Config.Validate path (the StickySessions.Validate
// precedent), not a request-time no-op.
func TestConfig_Validate_RejectsNonRefusalRetryOnStatus(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Proxy.RetryOnStatus = []int{429, 200}

	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "proxy config invalid")
	assert.Contains(t, err.Error(), "retry_on_status")
}

// TestDefaultConfig_RetryOnStatusIsOff pins the byte-identical default: the
// shipped default arms nothing, and it validates as-is.
func TestDefaultConfig_RetryOnStatusIsOff(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	assert.Empty(t, cfg.Proxy.RetryOnStatus)
	require.NoError(t, cfg.Proxy.Validate())
}
