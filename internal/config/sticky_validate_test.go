package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStickySessionConfig_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     StickySessionConfig
		wantErr string
	}{
		{
			name: "default cascade accepted",
			cfg:  StickySessionConfig{Enabled: true, KeySources: []string{"session_header", "first_user_message", "prefix_hash", "auth_header"}, PrefixHashBytes: 512, KeyScanBytes: 65536},
		},
		{
			name: "every implemented source is valid",
			cfg:  StickySessionConfig{Enabled: true, KeySources: []string{"session_header", "first_user_message", "prefix_hash", "auth_header", "ip"}},
		},
		{
			name:    "misspelled source rejected",
			cfg:     StickySessionConfig{Enabled: true, KeySources: []string{"prefx_hash"}},
			wantErr: "prefx_hash",
		},
		{
			name:    "legacy ordering rule name rejected",
			cfg:     StickySessionConfig{Enabled: true, KeySources: []string{"first_turn"}},
			wantErr: "first_turn",
		},
		{
			name:    "negative key_scan_bytes rejected",
			cfg:     StickySessionConfig{Enabled: true, KeySources: []string{"prefix_hash"}, KeyScanBytes: -1},
			wantErr: "key_scan_bytes",
		},
		{
			name: "disabled config is not validated",
			cfg:  StickySessionConfig{Enabled: false, KeySources: []string{"nonsense"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestConfig_Validate_RejectsUnknownStickyKeySource(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Proxy.StickySessions.Enabled = true
	cfg.Proxy.StickySessions.KeySources = []string{"session_header", "prefix_hashs"}

	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "proxy.sticky_sessions")
	assert.Contains(t, err.Error(), "prefix_hashs")
}

func TestDefaultConfig_StickyCascadeCoversEverySource(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	// The shipped cascade is what operators copy; it must validate as-is and
	// key per conversation (first_user_message) ahead of the shared-prefix key,
	// with a body budget big enough to reach past a multi-KB system prompt.
	require.NoError(t, cfg.Proxy.StickySessions.Validate())
	assert.Equal(t, []string{"session_header", "first_user_message", "prefix_hash", "auth_header"}, cfg.Proxy.StickySessions.KeySources)
	assert.Greater(t, cfg.Proxy.StickySessions.KeyScanBytes, cfg.Proxy.StickySessions.PrefixHashBytes)
}
