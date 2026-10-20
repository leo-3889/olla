package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/thushan/olla/internal/core/constants"
)

// stickyStatsBlock returns the sticky-session statistics for embedding in
// larger payloads (/internal/status). Same shape as GET /internal/stats/sticky:
// callers always branch on "enabled", never on presence/absence.
func (a *Application) stickyStatsBlock() any {
	enabledShape := map[string]any{"enabled": false}
	if a.stickyStatsFn == nil {
		return enabledShape
	}
	stats := a.stickyStatsFn()
	if stats == nil {
		return enabledShape
	}
	return stats
}

func (a *Application) stickyStatsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(http.StatusOK)

	if a.stickyStatsFn == nil {
		// Sticky sessions are disabled — return a stable JSON shape so callers
		// can branch on the "enabled" field rather than on status codes.
		if err := json.NewEncoder(w).Encode(struct {
			Enabled bool `json:"enabled"`
		}{Enabled: false}); err != nil {
			a.logger.Error("Failed to encode sticky stats response", "error", err)
		}
		return
	}

	stats := a.stickyStatsFn()
	if stats == nil {
		// stickyStatsFn is wired but the wrapper was not created because sticky
		// sessions are disabled in config — return the same stable shape as the
		// nil-function path so callers always branch on "enabled", not status codes.
		if err := json.NewEncoder(w).Encode(struct {
			Enabled bool `json:"enabled"`
		}{Enabled: false}); err != nil {
			a.logger.Error("Failed to encode sticky stats response", "error", err)
		}
		return
	}
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		a.logger.Error("Failed to encode sticky stats response", "error", err)
	}
}
